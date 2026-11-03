package api

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
)

func totpCode(secret string) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
}

func TestSelfPasswordChangeForEveryRole(t *testing.T) {
	h := newHarness(t, nil)
	for _, role := range []string{"owner", "admin", "operator", "viewer"} {
		t.Run(role, func(t *testing.T) {
			session := h.session
			if role != "owner" {
				if _, err := h.app.Auth.CreateUser(t.Context(), role, "controlled-password", role, false); err != nil {
					t.Fatal(err)
				}
				var err error
				session, err = h.app.Auth.Login(t.Context(), role, "controlled-password", "")
				if err != nil {
					t.Fatal(err)
				}
			}
			body := map[string]string{"current_password": "controlled-password", "new_password": "another-controlled-password"}
			if r := h.request(t, "POST", "/auth/password", body, &session, false); r.Code != 403 {
				t.Fatal("password change bypassed CSRF")
			}
			body["user_id"] = auth.Token()
			if r := h.request(t, "POST", "/auth/password", body, &session, true); r.Code != 400 {
				t.Fatal("password endpoint accepted a target account")
			}
			delete(body, "user_id")
			body["current_password"] = "incorrect"
			if r := h.request(t, "POST", "/auth/password", body, &session, true); r.Code != 400 {
				t.Fatal("password changed without current-password proof")
			}
			body["current_password"] = "controlled-password"
			decodeResponse[map[string]bool](t, h.request(t, "POST", "/auth/password", body, &session, true), 200)
			if r := h.request(t, "GET", "/auth/session", nil, &session, false); r.Code != 401 {
				t.Fatal("password change left the browser session authenticated")
			}
			if _, err := h.app.Auth.Login(t.Context(), role, body["new_password"], ""); err != nil {
				t.Fatal("new password failed:", err)
			}
		})
	}
}

func TestUserRevocationIsOwnerOnlyAndRemovesSessionsAndGrants(t *testing.T) {
	h := newHarness(t, nil)
	target, err := h.app.Auth.CreateUser(t.Context(), "target", "controlled-password", "viewer", false)
	if err != nil {
		t.Fatal(err)
	}
	targetSession, err := h.app.Auth.Login(t.Context(), "target", "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec("INSERT INTO instance_permissions VALUES(?,?,?)", target.ID, h.instance.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.Auth.CreateUser(t.Context(), "admin", "controlled-password", "admin", false); err != nil {
		t.Fatal(err)
	}
	admin, err := h.app.Auth.Login(t.Context(), "admin", "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	path := "/users/" + target.ID
	if r := h.request(t, "DELETE", path, map[string]any{}, &admin, true); r.Code != 403 {
		t.Fatal("admin revoked another user")
	}
	if r := h.request(t, "DELETE", path, map[string]any{}, &h.session, false); r.Code != 403 {
		t.Fatal("revocation bypassed CSRF")
	}
	if r := h.request(t, "DELETE", "/users/"+h.session.User.ID, map[string]any{}, &h.session, true); r.Code != 400 {
		t.Fatal("owner revoked their own account")
	}
	decodeResponse[map[string]bool](t, h.request(t, "DELETE", path, map[string]any{}, &h.session, true), 200)
	if r := h.request(t, "GET", "/auth/session", nil, &targetSession, false); r.Code != 401 {
		t.Fatal("revoked user's session remains valid")
	}
	if _, err := h.app.Auth.Login(t.Context(), "target", "controlled-password", ""); err == nil {
		t.Fatal("revoked user can still sign in")
	}
	var count int
	if err := h.app.DB.QueryRow("SELECT count(*) FROM instance_permissions WHERE user_id=?", target.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("revoked grants remain: %d %v", count, err)
	}
}

func TestTOTPHTTPEnrollmentAndProtectedDisable(t *testing.T) {
	h := newHarness(t, nil)
	setup := decodeResponse[map[string]string](t, h.request(t, "POST", "/auth/totp", map[string]any{}, &h.session, true), 200)
	secret := setup["secret"]
	body := map[string]string{"password": "controlled-password", "secret": secret, "code": totpCode(secret)}
	decodeResponse[map[string]bool](t, h.request(t, "POST", "/auth/totp", body, &h.session, true), 200)
	if r := h.request(t, "GET", "/auth/session", nil, &h.session, false); r.Code != 401 {
		t.Fatal("TOTP enrollment did not revoke the browser session")
	}
	session, err := h.app.Auth.Login(t.Context(), "owner", "controlled-password", totpCode(secret))
	if err != nil {
		t.Fatal(err)
	}
	disable := map[string]string{"password": "controlled-password", "code": "invalid"}
	if r := h.request(t, "DELETE", "/auth/totp", disable, &session, true); r.Code != 400 {
		t.Fatal("TOTP disabled without its code")
	}
	disable["code"] = totpCode(secret)
	if r := h.request(t, "DELETE", "/auth/totp", disable, &session, false); r.Code != 403 {
		t.Fatal("TOTP disable bypassed CSRF")
	}
	disable["password"] = "incorrect"
	if r := h.request(t, "DELETE", "/auth/totp", disable, &session, true); r.Code != 400 {
		t.Fatal("TOTP disabled without its password")
	}
	disable["password"] = "controlled-password"
	decodeResponse[map[string]bool](t, h.request(t, "DELETE", "/auth/totp", disable, &session, true), 200)
	if r := h.request(t, "GET", "/auth/session", nil, &session, false); r.Code != 401 {
		t.Fatal("TOTP disable did not revoke the browser session")
	}
	if _, err := h.app.Auth.Login(t.Context(), "owner", "controlled-password", ""); err != nil {
		t.Fatal(err)
	}
	var audit string
	if err := h.app.DB.QueryRow("SELECT group_concat(detail) FROM audit WHERE action LIKE 'auth.%'").Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, secret) || strings.Contains(audit, "controlled-password") {
		t.Fatal("account audit leaked credentials")
	}
}
