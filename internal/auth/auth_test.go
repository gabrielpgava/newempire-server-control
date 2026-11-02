package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

func TestBootstrapSessionsAndRoles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vault, err := secrets.Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(db, vault)
	if _, err := s.CreateUser(ctx, "owner", "long-test-password", "owner", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "other", "long-test-password", "owner", true); err == nil {
		t.Fatal("allowed second bootstrap")
	}
	if _, err := s.Login(ctx, "owner", "bad", ""); err == nil {
		t.Fatal("accepted wrong password")
	}
	sess, err := s.Login(ctx, "owner", "long-test-password", "")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Session(ctx, sess.Token)
	if err != nil || loaded.User.Role != "owner" || loaded.CSRF != sess.CSRF {
		t.Fatalf("session: %+v %v", loaded, err)
	}
	if err := s.Logout(ctx, sess.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess.Token); err == nil {
		t.Fatal("logged-out session accepted")
	}
	if Permitted("viewer", "operator") || Permitted("invalid", "viewer") || !Permitted("admin", "operator") {
		t.Fatal("incorrect permission hierarchy")
	}
}

func TestTOTPAndThrottle(t *testing.T) {
	// RFC 6238 SHA-1 vector, truncated to the six digits accepted by this product.
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if !VerifyTOTP(secret, "287082", time.Unix(59, 0)) || VerifyTOTP(secret, "000000", time.Unix(59, 0)) {
		t.Fatal("TOTP verification failed")
	}
	s := &Service{limits: map[string]attempt{}}
	for range 10 {
		if !s.Allow("ip") {
			t.Fatal("throttled before limit")
		}
	}
	if s.Allow("ip") {
		t.Fatal("did not throttle")
	}
}
