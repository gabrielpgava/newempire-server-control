package fabric

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectSourceLauncherReportsEmbeddedVersionsWithoutExecution(t *testing.T) {
	selection := Selection{MinecraftVersion: "source-game", LoaderVersion: "0.99.0"}
	data := launcherFixture(t, selection)
	file, err := os.CreateTemp(t.TempDir(), "source-*.jar")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	source, err := InspectSourceLauncher(t.Context(), file)
	if err != nil || source.MinecraftVersion != selection.MinecraftVersion || source.LoaderVersion != selection.LoaderVersion || source.JavaMajor != 8 {
		t.Fatalf("source %+v: %v", source, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectSourceLauncher(ctx, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestInspectSourceLauncherRejectsUnverifiableOrMalformedJARs(t *testing.T) {
	for _, mode := range []string{"not-jar", "legacy", "wrong-main", "duplicate-properties", "bad-version", "bad-class", "preview-class", "large-properties", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			var data bytes.Buffer
			writer := zip.NewWriter(&data)
			entries := []struct{ name, value string }{
				{"META-INF/MANIFEST.MF", "Manifest-Version: 1.0\r\nMain-Class: net.fabricmc.installer.ServerLauncher\r\n\r\n"},
				{"install.properties", "game-version=source-game\nfabric-loader-version=0.99.0\n"},
				{"net/fabricmc/installer/ServerLauncher.class", "\xca\xfe\xba\xbe\x00\x00\x00\x34"},
			}
			switch mode {
			case "legacy":
				entries = entries[:1]
			case "wrong-main":
				entries[0].value = "Main-Class: arbitrary.Main\r\n\r\n"
			case "duplicate-properties":
				entries = append(entries, entries[1])
			case "bad-version":
				entries[1].value = "game-version=../outside\nfabric-loader-version=0.99.0\n"
			case "bad-class":
				entries[2].value = "\xca\xfe\xba\xbe\x00\x00\x00\x00"
			case "preview-class":
				entries[2].value = "\xca\xfe\xba\xbe\xff\xff\x00\x3f"
			case "large-properties":
				entries[1].value += strings.Repeat("#", (32<<10)+1)
			}
			for _, entry := range entries {
				f, err := writer.Create(entry.name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write([]byte(entry.value)); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "not-jar" {
				data.Reset()
				data.WriteString("not Java code")
			}
			name := filepath.Join(t.TempDir(), "source.jar")
			if err := os.WriteFile(name, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(name, os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if mode == "oversized" {
				if err := file.Truncate(MaxLauncherBytes + 1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InspectSourceLauncher(t.Context(), file); err == nil {
				t.Fatal("accepted an unverifiable source launcher")
			}
		})
	}
}
