package updates

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestArchiveSafetyAndArchitecture(t *testing.T) {
	for _, attack := range []string{"traversal", "absolute", "nested", "symlink", "hardlink", "fifo", "setuid", "wrong architecture", "wrong OS ABI", "not ELF", "extra entry", "concatenated gzip", "oversized expanded binary", "truncated gzip"} {
		t.Run(attack, func(t *testing.T) {
			f := newFixture(t)
			f.github.add(t, "1.1.0", 1)
			payload := fakeELF(runtime.GOARCH, "1.1.0")
			header := &tar.Header{Name: BinaryName, Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(payload))}
			switch attack {
			case "traversal":
				header.Name = "../world"
			case "absolute":
				header.Name = f.world
			case "nested":
				header.Name = "bin/" + BinaryName
			case "symlink":
				header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, f.world, 0
			case "hardlink":
				header.Typeflag, header.Linkname, header.Size = tar.TypeLink, f.world, 0
			case "fifo":
				header.Typeflag, header.Size = tar.TypeFifo, 0
			case "setuid":
				header.Mode = 04755
			case "wrong architecture":
				arch := "amd64"
				if runtime.GOARCH == arch {
					arch = "arm64"
				}
				payload = fakeELF(arch, "1.1.0")
				header.Size = int64(len(payload))
			case "not ELF":
				payload = bytes.Repeat([]byte("x"), len(payload))
			case "wrong OS ABI":
				payload[7] = 9 // FreeBSD, not Linux or System V.
			}
			b := tarball(t, header, payload)
			switch attack {
			case "extra entry":
				var out bytes.Buffer
				gz := gzip.NewWriter(&out)
				tw := tar.NewWriter(gz)
				for i := 0; i < 2; i++ {
					must(t, tw.WriteHeader(header))
					_, err := tw.Write(payload)
					must(t, err)
				}
				must(t, tw.Close())
				must(t, gz.Close())
				b = out.Bytes()
			case "concatenated gzip":
				b = append(b, tarball(t, header, payload)...)
			case "oversized expanded binary":
				var out bytes.Buffer
				gz := gzip.NewWriter(&out)
				tw := tar.NewWriter(gz)
				header.Size = maxBinaryBytes + 1
				must(t, tw.WriteHeader(header))
				_ = tw.Close() // Deliberately omit the oversized body.
				must(t, gz.Close())
				b = out.Bytes()
			case "truncated gzip":
				b = b[:len(b)-4]
			}
			f.github.replaceArchive(t, "1.1.0", runtime.GOARCH, b)
			if _, err := f.m.Stage(context.Background(), "1.1.0", Stable); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if f.service.current() != "1.0.0" || len(f.service.calls) != 0 || string(read(t, f.world)) != "world must never change" {
				t.Fatal("unsafe archive affected live data")
			}
		})
	}
}

func TestStageRejectsSignatureHashAndSizeMismatch(t *testing.T) {
	for _, mismatch := range []string{"signature", "hash", "size", "metadata size"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newFixture(t)
			f.github.add(t, "1.1.0", 1)
			address := downloadURL("1.1.0", archiveName("1.1.0", runtime.GOARCH))
			switch mismatch {
			case "signature":
				f.github.bodies[downloadURL("1.1.0", "release.json.sig")] = bytes.Repeat([]byte("A"), 88)
			case "hash":
				b := bytes.Clone(f.github.bodies[address])
				b[len(b)/2] ^= 1
				f.github.bodies[address] = b
			case "size":
				f.github.bodies[address] = append(f.github.bodies[address], 'x')
			case "metadata size":
				f.github.release("1.1.0").Assets[0].Size++
			}
			if _, err := f.m.Stage(context.Background(), "1.1.0", Stable); !errors.Is(err, ErrVerification) {
				t.Fatal("mismatch did not fail verification", err)
			}
			if len(f.service.calls) != 0 || f.service.current() != "1.0.0" {
				t.Fatal("unverified release touched live service")
			}
		})
	}
}
