package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

func accountService(t *testing.T) (*Service, Session) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	vault, err := secrets.Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, vault)
	if _, err := s.CreateUser(t.Context(), "owner", "controlled-password", "owner", true); err != nil {
		t.Fatal(err)
	}
	session, err := s.Login(t.Context(), "owner", "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, session
}

func accountCode(secret string) string {
	key, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 15
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
}

func TestPasswordChangeValidatesAndRevokesEverySession(t *testing.T) {
	s, first := accountService(t)
	second, err := s.Login(t.Context(), "owner", "controlled-password", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range [][2]string{{"incorrect", "another-controlled-password"}, {"controlled-password", "short"}} {
		if err := s.ChangePassword(t.Context(), first.User.ID, attempt[0], attempt[1]); err == nil {
			t.Fatal("invalid password change accepted")
		}
		if _, err := s.Session(t.Context(), first.Token); err != nil {
			t.Fatal("rejected change revoked a valid session:", err)
		}
	}
	password := strings.Repeat("é", 6)
	if err := s.ChangePassword(t.Context(), first.User.ID, "controlled-password", password); err != nil {
		t.Fatal("a 12-byte UTF-8 password was rejected:", err)
	}
	for _, token := range []string{first.Token, second.Token} {
		if _, err := s.Session(t.Context(), token); err == nil {
			t.Fatal("a session survived its password change")
		}
	}
	if _, err := s.Login(t.Context(), "owner", "controlled-password", ""); err == nil {
		t.Fatal("the old password still works")
	}
	if _, err := s.Login(t.Context(), "owner", password, ""); err != nil {
		t.Fatal("the new password failed:", err)
	}
}

func TestTOTPChangesRequireProofAndRevokeSessions(t *testing.T) {
	s, first := accountService(t)
	secret := NewTOTP()
	if err := s.EnableTOTP(t.Context(), first.User.ID, "incorrect", secret, accountCode(secret)); err == nil {
		t.Fatal("enabled TOTP without the current password")
	}
	if err := s.EnableTOTP(t.Context(), first.User.ID, "controlled-password", secret, "invalid"); err == nil {
		t.Fatal("enabled TOTP without a valid code")
	}
	if err := s.EnableTOTP(t.Context(), first.User.ID, "controlled-password", secret, accountCode(secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(t.Context(), first.Token); err == nil {
		t.Fatal("enrollment did not revoke the old session")
	}
	if _, err := s.Login(t.Context(), "owner", "controlled-password", ""); err == nil {
		t.Fatal("login bypassed TOTP")
	}
	enabled, err := s.Login(t.Context(), "owner", "controlled-password", accountCode(secret))
	if err != nil || !enabled.User.TwoFactor {
		t.Fatalf("TOTP login: %+v %v", enabled.User, err)
	}
	replacement := NewTOTP()
	if err := s.EnableTOTP(t.Context(), first.User.ID, "controlled-password", replacement, accountCode(replacement)); err == nil {
		t.Fatal("re-enrollment replaced an enabled factor without its code")
	}
	for _, attempt := range [][2]string{{"incorrect", accountCode(secret)}, {"controlled-password", "invalid"}} {
		if err := s.DisableTOTP(t.Context(), first.User.ID, attempt[0], attempt[1]); err == nil {
			t.Fatal("disabled TOTP without both proofs")
		}
	}
	if _, err := s.Session(t.Context(), enabled.Token); err != nil {
		t.Fatal("a rejected security change revoked a session:", err)
	}
	if err := s.DisableTOTP(t.Context(), first.User.ID, "controlled-password", accountCode(secret)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(t.Context(), enabled.Token); err == nil {
		t.Fatal("disabling TOTP did not revoke sessions")
	}
	disabled, err := s.Login(t.Context(), "owner", "controlled-password", "")
	if err != nil || disabled.User.TwoFactor {
		t.Fatalf("login after disabling TOTP: %+v %v", disabled.User, err)
	}
}

func TestAccountChangeRollsBackWhenSessionRevocationFails(t *testing.T) {
	for _, operation := range []string{"password", "enable", "disable"} {
		t.Run(operation, func(t *testing.T) {
			s, session := accountService(t)
			secret := NewTOTP()
			if operation == "disable" {
				if err := s.EnableTOTP(t.Context(), session.User.ID, "controlled-password", secret, accountCode(secret)); err != nil {
					t.Fatal(err)
				}
				var err error
				session, err = s.Login(t.Context(), "owner", "controlled-password", accountCode(secret))
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.DB.Exec("CREATE TRIGGER reject_revocation BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'controlled revocation failure'); END"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "password":
				err = s.ChangePassword(t.Context(), session.User.ID, "controlled-password", "another-controlled-password")
			case "enable":
				err = s.EnableTOTP(t.Context(), session.User.ID, "controlled-password", secret, accountCode(secret))
			case "disable":
				err = s.DisableTOTP(t.Context(), session.User.ID, "controlled-password", accountCode(secret))
			}
			if err == nil {
				t.Fatal("security change succeeded without revoking sessions")
			}
			if _, err := s.Session(t.Context(), session.Token); err != nil {
				t.Fatal("failed transaction destroyed the original session:", err)
			}
			code := ""
			if operation == "disable" {
				code = accountCode(secret)
			}
			if _, err := s.Login(t.Context(), "owner", "controlled-password", code); err != nil {
				t.Fatal("failed transaction changed account credentials:", err)
			}
		})
	}
}

func TestLoginRechecksCredentialsBeforeIssuingSession(t *testing.T) {
	for _, credential := range []string{"password", "totp"} {
		t.Run(credential, func(t *testing.T) {
			s, session := accountService(t)
			writer, err := database.Open(s.DB.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			tx, err := writer.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if credential == "password" {
				if _, err := tx.Exec("UPDATE users SET password_hash='changed' WHERE id=?", session.User.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := tx.Exec("UPDATE users SET totp=x'01' WHERE id=?", session.User.ID); err != nil {
					t.Fatal(err)
				}
			}
			// Queue the initial SELECT first, then hold its connection after it read the old credentials.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			connection, err := s.DB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			waiting := s.DB.Stats().WaitCount
			result := make(chan error, 1)
			go func() {
				_, err := s.Login(ctx, "owner", "controlled-password", "")
				result <- err
			}()
			for s.DB.Stats().WaitCount == waiting && ctx.Err() == nil {
				time.Sleep(time.Millisecond)
			}
			connection.Close()
			afterRead, err := s.DB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				afterRead.Close()
				t.Fatal(err)
			}
			afterRead.Close()
			if err := <-result; err == nil {
				t.Fatal("issued a session from credentials changed during password verification")
			}
		})
	}
}
