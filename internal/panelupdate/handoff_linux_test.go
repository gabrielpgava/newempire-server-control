package panelupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"golang.org/x/sys/unix"
)

func TestPublishReportReadableByPanelDespitePrivateUmask(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("two-UID fixture requires isolated root")
	}
	dir := t.TempDir()
	for p := dir; p != filepath.Clean(os.TempDir()); p = filepath.Dir(p) {
		if err := os.Chmod(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(dir, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	mask := unix.Umask(0077)
	defer unix.Umask(mask)
	report := Report{RequestID: model.NewID(), JobID: model.NewID(), Action: "stage", Version: "1.1.0", State: "succeeded"}
	if err := Publish(context.Background(), root, report); err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat(ReportFile)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("report mode was changed by the service umask", info, err)
	}
	got, err := ReadReport(context.Background(), root)
	if err != nil || got.RequestID != report.RequestID || got.Action != "stage" {
		t.Fatal("root report did not round trip", got, err)
	}
	command := exec.Command("/usr/bin/test", "-r", filepath.Join(dir, ReportFile))
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + os.TempDir()}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	if err := command.Run(); err != nil {
		t.Fatal("panel UID cannot read the root report", err)
	}
	if err := root.Chown(ReportFile, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(context.Background(), root); err == nil {
		t.Fatal("panel-owned report was trusted")
	}
}
