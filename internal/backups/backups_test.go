package backups

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/importer"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

func rootFixture(t *testing.T) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func sourceFixture(t *testing.T, parent *os.Root) *os.Root {
	t.Helper()
	if err := parent.Mkdir("server", 0700); err != nil {
		t.Fatal(err)
	}
	r, err := parent.OpenRoot("server")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if err = r.Mkdir("world", 0700); err != nil {
		t.Fatal(err)
	}
	if err = r.WriteFile("world/level.dat", []byte("original world"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = r.WriteFile("server.properties", []byte("rcon.password=private\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateVerifyRangeRestoreAndExplicitRecovery(t *testing.T) {
	for _, format := range []Format{ZIP, TARZSTD} {
		t.Run(string(format), func(t *testing.T) {
			ctx := context.Background()
			parent, storage := rootFixture(t), rootFixture(t)
			source := sourceFixture(t, parent)
			b, err := Create(ctx, source, storage, format, nil)
			if err != nil {
				t.Fatal(err)
			}
			m, err := Verify(ctx, storage, b.ID, nil)
			if err != nil || len(m.Entries) != 3 || m.Bytes != 36 {
				t.Fatalf("manifest: %+v %v", m, err)
			}
			list, err := List(ctx, storage)
			if err != nil || len(list) != 1 || list[0].ID != b.ID {
				t.Fatalf("list: %+v %v", list, err)
			}
			f, _, err := Open(storage, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/backup", nil)
			req.Header.Set("Range", "bytes=0-3")
			w := httptest.NewRecorder()
			http.ServeContent(w, req, b.ID, b.CreatedAt, f)
			f.Close()
			if w.Code != http.StatusPartialContent || w.Body.Len() != 4 {
				t.Fatalf("range: %d %q", w.Code, w.Body.String())
			}
			if err = source.WriteFile("world/level.dat", []byte("newer world"), 0600); err != nil {
				t.Fatal(err)
			}
			stage, err := StageRestore(ctx, storage, b.ID, parent, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := CommitRestore(ctx, parent, "server", stage.Name)
			if err != nil || !result.Installed {
				t.Fatalf("restore: %+v %v", result, err)
			}
			restored, err := parent.ReadFile("server/world/level.dat")
			if err != nil || string(restored) != "original world" {
				t.Fatalf("restore data: %q %v", restored, err)
			}
			retained, err := parent.ReadFile(result.Recovery + "/world/level.dat")
			if err != nil || string(retained) != "newer world" {
				t.Fatalf("original not retained: %q %v", retained, err)
			}
			if _, err = parent.Stat(result.Journal); err != nil {
				t.Fatalf("journal absent: %v", err)
			}
			recovered, err := Recover(ctx, parent, "server", result.Recovery)
			if err != nil || !recovered.Installed {
				t.Fatalf("recovery: %+v %v", recovered, err)
			}
			data, _ := parent.ReadFile("server/world/level.dat")
			if string(data) != "newer world" {
				t.Fatal("explicit recovery failed")
			}
			data, _ = parent.ReadFile(recovered.Recovery + "/world/level.dat")
			if string(data) != "original world" {
				t.Fatal("recovery discarded displaced world")
			}
		})
	}
}

func TestBackupSafetyCancellationAndStagedTampering(t *testing.T) {
	ctx := context.Background()
	parent, storage := rootFixture(t), rootFixture(t)
	source := sourceFixture(t, parent)
	if err := source.Mkdir("nested-backups", 0700); err != nil {
		t.Fatal(err)
	}
	nested, err := source.OpenRoot("nested-backups")
	if err != nil {
		t.Fatal(err)
	}
	defer nested.Close()
	if _, err = Create(ctx, source, nested, ZIP, nil); err == nil {
		t.Fatal("nested backup storage accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = Create(cancelled, source, storage, ZIP, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel ignored: %v", err)
	}
	list, err := List(ctx, storage)
	if err != nil || len(list) != 0 {
		t.Fatalf("failed backup listed: %+v %v", list, err)
	}
	b, err := Create(ctx, source, storage, ZIP, nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := StageRestore(ctx, storage, b.ID, parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = parent.WriteFile(stage.Name+"/world/level.dat", []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CommitRestore(ctx, parent, "server", stage.Name); err == nil {
		t.Fatal("tampered stage installed")
	}
	data, _ := source.ReadFile("world/level.dat")
	if string(data) != "original world" {
		t.Fatal("failed restore modified original")
	}
	if _, err = CommitRestore(cancelled, parent, "server", stage.Name); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled restore: %v", err)
	}
	f, _, err := Open(storage, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	ext, _ := extension(b.Format)
	w, err := storage.OpenFile(b.ID+ext, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.WriteAt([]byte("bad"), 0); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err = Verify(ctx, storage, b.ID, nil); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("archive corruption ignored: %v", err)
	}
}

func TestVerifyChecksManifestAndEveryDataFile(t *testing.T) {
	for _, alterManifest := range []bool{false, true} {
		t.Run(map[bool]string{false: "data", true: "manifest"}[alterManifest], func(t *testing.T) {
			parent, storage := rootFixture(t), rootFixture(t)
			source := sourceFixture(t, parent)
			b, err := Create(context.Background(), source, storage, ZIP, nil)
			if err != nil {
				t.Fatal(err)
			}
			f, _, err := Open(storage, b.ID)
			if err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(f, b.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			zw := zip.NewWriter(&out)
			for _, entry := range zr.File {
				r, err := entry.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				if alterManifest && entry.Name == "manifest.json" {
					var m Manifest
					if err = json.Unmarshal(data, &m); err != nil {
						t.Fatal(err)
					}
					m.Entries = append(m.Entries, importer.Entry{Path: "missing", Size: 0, SHA256: strings.Repeat("0", 64)})
					data, _ = json.Marshal(m)
					h := sha256.Sum256(data)
					b.ManifestSHA256 = hex.EncodeToString(h[:])
				} else if !alterManifest && entry.Name == "data/world/level.dat" {
					data = []byte("altered payload")
				}
				h := entry.FileHeader
				h.CRC32, h.CompressedSize64, h.UncompressedSize64 = 0, 0, 0
				w, err := zw.CreateHeader(&h)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = w.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			f.Close()
			if err = zw.Close(); err != nil {
				t.Fatal(err)
			}
			b.Bytes = int64(out.Len())
			h := sha256.Sum256(out.Bytes())
			b.ArchiveSHA256 = hex.EncodeToString(h[:])
			if err = storage.WriteFile(b.ID+".zip", out.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(b)
			if err = storage.WriteFile(b.ID+".json", raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = Verify(context.Background(), storage, b.ID, nil); err == nil {
				t.Fatal("manifest/data mismatch accepted despite matching archive checksum")
			}
		})
	}
}

func TestBackupRejectsLinks(t *testing.T) {
	parent, storage := rootFixture(t), rootFixture(t)
	source := sourceFixture(t, parent)
	if err := source.Symlink("world/level.dat", "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), source, storage, TARZSTD, nil); !errors.Is(err, files.ErrUnsafeType) {
		t.Fatalf("unsafe backup: %v", err)
	}
}

func TestRecoveryAfterInterruptedRenameAndHiddenStageInjection(t *testing.T) {
	ctx := context.Background()
	parent, storage := rootFixture(t), rootFixture(t)
	source := sourceFixture(t, parent)
	b, err := Create(ctx, source, storage, ZIP, nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := StageRestore(ctx, storage, b.ID, parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = parent.WriteFile(stage.Name+"/.nesc-hidden", []byte("unexpected"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CommitRestore(ctx, parent, "server", stage.Name); err == nil {
		t.Fatal("hidden extra file installed")
	}
	recovery := ".nesc-recovery-" + model.NewID()
	if err = files.RenameNoReplace(parent, "server", recovery); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(ctx, parent, "server", recovery)
	if err != nil || !result.Installed {
		t.Fatalf("missing-target recovery failed: %+v %v", result, err)
	}
	data, err := parent.ReadFile("server/world/level.dat")
	if err != nil || string(data) != "original world" {
		t.Fatalf("interrupted original lost: %q %v", data, err)
	}
}
