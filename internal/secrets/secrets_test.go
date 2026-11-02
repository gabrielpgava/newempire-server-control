package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCredentialPurposeAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	v, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := v.Seal([]byte("dummy-credential"), "rcon:instance-a")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := v.Open(ciphertext, "rcon:instance-a")
	if err != nil || string(plain) != "dummy-credential" {
		t.Fatal("credential did not round-trip")
	}
	if _, err := v.Open(ciphertext, "rcon:instance-b"); err == nil {
		t.Fatal("credential could be moved between instances")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := v.Open(ciphertext, "rcon:instance-a"); err == nil {
		t.Fatal("tampered credential was accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("accepted public master key")
	}
}
