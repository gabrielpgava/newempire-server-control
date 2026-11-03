package fabric

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestLauncherRejectsMalformedArchiveEntriesAndBootstrap(t *testing.T) {
	selection := Selection{MinecraftVersion: "fixture-game", LoaderVersion: "0.99.0", InstallerVersion: "9.9.0"}
	for _, mode := range []string{"traversal", "absolute", "backslash", "drive-path", "symlink", "duplicate", "main-class", "no-bootstrap", "bad-bootstrap", "bad-properties", "oversized-entry", "too-many-entries", "crc"} {
		t.Run(mode, func(t *testing.T) {
			var buf bytes.Buffer
			writer := zip.NewWriter(&buf)
			write := func(name, content string, mode os.FileMode, size uint64) {
				t.Helper()
				header := &zip.FileHeader{Name: name, Method: zip.Store, UncompressedSize64: size, CompressedSize64: size}
				header.SetMode(mode)
				var entry io.Writer
				var err error
				if size != 0 {
					entry, err = writer.CreateRaw(header)
				} else {
					entry, err = writer.CreateHeader(header)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(entry, content); err != nil {
					t.Fatal(err)
				}
			}
			manifest := "Manifest-Version: 1.0\nMain-Class: net.fabricmc.installer.ServerLauncher\n\n"
			if mode == "main-class" {
				manifest = strings.ReplaceAll(manifest, "net.fabricmc.installer.ServerLauncher", "untrusted.Main")
			}
			write("META-INF/MANIFEST.MF", manifest, 0600, 0)
			properties := "game-version=fixture-game\nfabric-loader-version=0.99.0\n"
			if mode == "bad-properties" {
				properties = "game-version=wrong\nfabric-loader-version=0.99.0\n"
			}
			write("install.properties", properties, 0600, 0)
			if mode != "no-bootstrap" {
				bootstrap := "\xca\xfe\xba\xbe\x00\x00\x00\x34"
				if mode == "bad-bootstrap" {
					bootstrap = "NOTABYTECODE"
				}
				write("net/fabricmc/installer/ServerLauncher.class", bootstrap, 0600, 0)
			}
			switch mode {
			case "traversal":
				write("../outside", "x", 0600, 0)
			case "absolute":
				write("/outside", "x", 0600, 0)
			case "backslash":
				write(`..\outside`, "x", 0600, 0)
			case "drive-path":
				write("C:/outside", "x", 0600, 0)
			case "symlink":
				write("link", "target", os.ModeSymlink|0600, 0)
			case "duplicate":
				write("install.properties", properties, 0600, 0)
			case "oversized-entry":
				write("bomb", "", 0600, 65<<20)
			case "too-many-entries":
				for i := 0; i < 10000; i++ {
					write("entry"+strings.Repeat("x", i%10), "", 0600, 0)
				}
			case "crc":
				write("bad-crc", "nonempty", 0600, 8)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := validateLauncher(t.Context(), buf.Bytes(), selection); err == nil {
				t.Fatal("accepted unsafe launcher")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := validateLauncher(ctx, launcherFixture(t, selection), selection); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func FuzzLauncher(f *testing.F) {
	f.Add([]byte("not a jar"))
	f.Add([]byte("PK\x05\x06" + strings.Repeat("\x00", 18)))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxLauncherBytes {
			return
		}
		_ = validateLauncher(t.Context(), data, Selection{MinecraftVersion: "fixture-game", LoaderVersion: "0.99.0", InstallerVersion: "9.9.0"})
	})
}
