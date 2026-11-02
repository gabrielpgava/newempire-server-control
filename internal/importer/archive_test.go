package importer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
)

type fixtureEntry struct {
	name, body string
	mode       os.FileMode
	tarType    byte
}

func archiveFixture(t *testing.T, format Format, entries []fixtureEntry) *os.File {
	t.Helper()
	var data bytes.Buffer
	if format == ZIP {
		zw := zip.NewWriter(&data)
		for _, e := range entries {
			h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
			h.SetMode(e.mode)
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		gz := gzip.NewWriter(&data)
		tw := tar.NewWriter(gz)
		for _, e := range entries {
			kind := e.tarType
			if kind == 0 {
				kind = tar.TypeReg
			}
			if err := tw.WriteHeader(&tar.Header{Name: e.name, Size: int64(len(e.body)), Mode: 0600, Typeflag: kind, Linkname: "../outside"}); err != nil {
				t.Fatal(err)
			}
			if len(e.body) > 0 {
				if _, err := tw.Write([]byte(e.body)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "archive"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(data.Bytes()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestInspectExtractAndHash(t *testing.T) {
	for _, format := range []Format{ZIP, TARGZ} {
		t.Run(string(format), func(t *testing.T) {
			f := archiveFixture(t, format, []fixtureEntry{{name: "world/level.dat", body: "world bytes"}, {name: "server.jar", body: "launcher"}})
			p, err := Inspect(context.Background(), f, format, Limits{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256([]byte("world bytes"))
			if p.Files != 2 || p.Bytes != 19 || len(p.Hash) != 64 || p.Entries[0].SHA256 != hex.EncodeToString(h[:]) {
				t.Fatalf("bad preview: %+v", p)
			}
			stage, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer stage.Close()
			extracted, err := Extract(context.Background(), f, format, stage, Limits{}, nil)
			if err != nil || !reflect.DeepEqual(p, extracted) {
				t.Fatalf("extract differs: %+v %v", extracted, err)
			}
			data, err := stage.ReadFile("world/level.dat")
			if err != nil || string(data) != "world bytes" {
				t.Fatalf("wrong extraction: %q %v", data, err)
			}
			if _, err = Extract(context.Background(), f, format, stage, Limits{}, nil); err == nil {
				t.Fatal("accepted nonempty staging")
			}
			changed := archiveFixture(t, format, []fixtureEntry{{name: "world/level.dat", body: "different world"}})
			newPreview, err := Inspect(context.Background(), changed, format, Limits{}, nil)
			if err != nil || p.Hash == newPreview.Hash {
				t.Fatalf("stale hash: %v", err)
			}
		})
	}
}

func TestRejectArchiveTraversalAndCollisions(t *testing.T) {
	cases := [][]fixtureEntry{}
	for _, name := range []string{"../escape", "/absolute", "a/../../escape", "a\\escape", "a//b", "a/./b", "C:escape", ".nesc-trash/file"} {
		cases = append(cases, []fixtureEntry{{name: name, body: "bad"}})
	}
	cases = append(cases,
		[]fixtureEntry{{name: "same", body: "one"}, {name: "same", body: "two"}},
		[]fixtureEntry{{name: "A/file", body: "one"}, {name: "a/other", body: "two"}},
		[]fixtureEntry{{name: "file", body: "one"}, {name: "file/child", body: "two"}},
		[]fixtureEntry{{name: "dir/child", body: "one"}, {name: "dir", body: "two"}},
	)
	for _, format := range []Format{ZIP, TARGZ} {
		for _, entries := range cases {
			t.Run(string(format)+"/"+entries[0].name, func(t *testing.T) {
				f := archiveFixture(t, format, entries)
				if _, err := Inspect(context.Background(), f, format, Limits{}, nil); err == nil {
					t.Fatal("unsafe archive accepted")
				}
			})
		}
	}
}

func TestRejectArchiveLinksAndDevices(t *testing.T) {
	for _, mode := range []os.FileMode{os.ModeSymlink | 0777, os.ModeNamedPipe | 0600, os.ModeDevice | 0600} {
		f := archiveFixture(t, ZIP, []fixtureEntry{{name: "link", body: "../outside", mode: mode}})
		if _, err := Inspect(context.Background(), f, ZIP, Limits{}, nil); !errors.Is(err, files.ErrUnsafeType) {
			t.Fatalf("ZIP special type accepted: %v", err)
		}
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		f := archiveFixture(t, TARGZ, []fixtureEntry{{name: "link", tarType: kind}})
		if _, err := Inspect(context.Background(), f, TARGZ, Limits{}, nil); !errors.Is(err, files.ErrUnsafeType) {
			t.Fatalf("tar special type accepted: %v", err)
		}
	}
}

func TestArchiveLimitsBombsCancellationAndCorruption(t *testing.T) {
	for _, format := range []Format{ZIP, TARGZ} {
		t.Run(string(format), func(t *testing.T) {
			f := archiveFixture(t, format, []fixtureEntry{{name: "bomb", body: strings.Repeat("0", 1<<20)}})
			for _, limits := range []Limits{{MaxBytes: 1024}, {MaxFileBytes: 1024}, {MaxArchiveBytes: 64}, {MaxRatio: 2}} {
				if _, err := Inspect(context.Background(), f, format, limits, nil); err == nil {
					t.Fatalf("limit ignored: %+v", limits)
				}
			}
			many := archiveFixture(t, format, []fixtureEntry{{name: "one", body: "one"}, {name: "two", body: "two"}})
			if _, err := Inspect(context.Background(), many, format, Limits{MaxEntries: 1}, nil); err == nil {
				t.Fatal("entry limit ignored")
			}
			ctx, cancel := context.WithCancel(context.Background())
			_, err := Inspect(ctx, f, format, Limits{MaxRatio: 100000}, func(int64) { cancel() })
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation ignored: %v", err)
			}
		})
	}
	f := archiveFixture(t, TARGZ, []fixtureEntry{{name: "file", body: "data"}})
	info, _ := f.Stat()
	if err := f.Truncate(info.Size() - 5); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), f, TARGZ, Limits{}, nil); err == nil {
		t.Fatal("truncated gzip accepted")
	}
	z := archiveFixture(t, ZIP, []fixtureEntry{{name: "file", body: strings.Repeat("payload", 10)}})
	info, _ = z.Stat()
	zr, err := zip.NewReader(z, info.Size())
	if err != nil {
		t.Fatal(err)
	}
	offset, err := zr.File[0].DataOffset()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = z.WriteAt([]byte{0xff, 0xff}, offset); err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(context.Background(), z, ZIP, Limits{}, nil); err == nil {
		t.Fatal("corrupt ZIP payload accepted")
	}
}
