package instances

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

func TestIsolatedRootsAndCrossPortCollision(t *testing.T) {
	dir := t.TempDir()
	c := config.Default()
	c.DataDir = dir
	c.InstanceDir = filepath.Join(dir, "instances")
	c.BackupDir = filepath.Join(dir, "backups")
	c.AdoptRoots = []string{dir}
	c.JavaPaths = []string{"/usr/bin/java"}
	if err := c.Prepare(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, err := secrets.Open(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{DB: db, Vault: v, Config: c}
	first := model.Instance{ID: model.NewID(), Name: "First", JavaPath: "/usr/bin/java", Jar: "launcher.jar", MemoryMB: 1024, Port: 25565, RCONPort: 25575, EULA: true, CreatedAt: time.Now(), RCONSecret: "dummy-secret"}
	first.Root = filepath.Join(c.InstanceDir, first.ID)
	os.Mkdir(first.Root, 0700)
	if err := s.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = model.NewID()
	second.Name = "Second"
	second.Root = filepath.Join(c.InstanceDir, second.ID)
	second.Port = first.RCONPort
	second.RCONPort = 25576
	os.Mkdir(second.Root, 0700)
	if err := s.Save(context.Background(), second); err == nil {
		t.Fatal("database allowed a game port to collide with RCON")
	}
	second.Root = c.BackupDir
	if err := s.Validate(second); err == nil {
		t.Fatal("adopted protected backup directory")
	}
	if _, err := s.OpenRoot(filepath.Join(dir, "outside-missing")); err == nil {
		t.Fatal("opened missing root")
	}
	outside := t.TempDir()
	link := filepath.Join(c.InstanceDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRoot(link); err == nil {
		t.Fatal("escaped trusted parent via instance symlink")
	}
}

func TestCollisionPreflightRejectsIPv6OnlyListener(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback is unavailable:", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	ipv4, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		t.Fatal("fixture is not IPv6-only:", err)
	}
	ipv4.Close()
	db, err := database.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{DB: db}
	err = s.CheckCollisions(t.Context(), filepath.Join(t.TempDir(), "instance"), port, port-1)
	if err == nil || !strings.Contains(err.Error(), "tcp6") {
		t.Fatalf("IPv6-only server was not detected before adoption: %v", err)
	}
}
