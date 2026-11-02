package modpacks

import (
	"archive/zip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/modrinth"
)

type ExportOptions struct {
	Name        string
	VersionID   string
	Target      modrinth.Target
	ConfigPaths []string
}

// Export references exact original CDN artifacts; it never embeds or redistributes mod binaries.
// Write to a temporary artifact and publish it only after Export succeeds.
func Export(ctx context.Context, root *os.Root, writer io.Writer, opts ExportOptions) error {
	if err := opts.Target.Validate(); err != nil {
		return err
	}
	lock, err := modrinth.LoadLock(root)
	if err != nil {
		return err
	}
	if len(lock.Files) > modrinth.MaxProjects {
		return errors.New("export exceeds the supported server mod project limit")
	}
	if lock.Target.MinecraftVersion != opts.Target.MinecraftVersion || lock.Target.LoaderVersion != opts.Target.LoaderVersion || opts.Target.LoaderVersion == "" {
		return errors.New("export target does not match the installed runtime/content lock")
	}
	index := Index{FormatVersion: 1, Game: "minecraft", Name: opts.Name, VersionID: opts.VersionID, Files: []File{}, Dependencies: map[string]string{"minecraft": opts.Target.MinecraftVersion, "fabric-loader": opts.Target.LoaderVersion}}
	locked := map[string]modrinth.LockedFile{}
	for _, f := range lock.Files {
		locked[f.Filename] = f
	}
	seen := map[string]bool{}
	count := 0
	if info, err := root.Lstat("mods"); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("mods must be a real directory")
		}
		mods, err := openRealDir(root, "mods")
		if err != nil {
			return err
		}
		defer mods.Close()
		err = fs.WalkDir(mods.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if name == "." {
				return nil
			}
			count++
			if count > modrinth.MaxFiles || !modrinth.SafePath(name) || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("unsafe or excessive mod content")
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeType != 0 {
				return errors.New("special files are not allowed in exported content")
			}
			if !strings.HasSuffix(strings.ToLower(name), ".jar") {
				return nil
			}
			file, ok := locked[name]
			if !ok {
				return errors.New("unmanaged JARs cannot be exported without verified redistribution metadata")
			}
			if err := safeParents(mods, path.Dir(name)); err != nil {
				return err
			}
			info, err := mods.Lstat(name)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() != file.Size {
				return errors.New("locked JAR is not regular or changed size")
			}
			f, err := mods.Open(name)
			if err != nil {
				return err
			}
			opened, statErr := f.Stat()
			if statErr != nil || !os.SameFile(info, opened) {
				f.Close()
				return errors.New("JAR changed while opening")
			}
			h512 := sha512.New()
			h1 := sha1.New()
			n, copyErr := io.Copy(io.MultiWriter(h512, h1), io.LimitReader(contextReader{ctx, f}, file.Size+1))
			closeErr := f.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return err
			}
			sha512Value := hex.EncodeToString(h512.Sum(nil))
			sha1Value := hex.EncodeToString(h1.Sum(nil))
			if n != file.Size || sha512Value != file.SHA512 || file.SHA1 != "" && file.SHA1 != sha1Value {
				return errors.New("locked JAR hash changed; export refused")
			}
			seen[name] = true
			index.Files = append(index.Files, File{Path: "mods/" + name, Hashes: map[string]string{"sha512": sha512Value, "sha1": sha1Value}, Env: &Environment{Client: "unsupported", Server: "required"}, Downloads: []string{file.URL}, FileSize: file.Size})
			return nil
		})
		if err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(seen) != len(lock.Files) {
		return errors.New("locked JARs are missing; export refused")
	}
	slices.SortFunc(index.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	if err := validateIndex(index, opts.Target); err != nil {
		return err
	}
	if len(opts.ConfigPaths) > modrinth.MaxFiles {
		return errors.New("too many config export paths")
	}
	configs := map[string][]byte{}
	var total int64
	var configRoot *os.Root
	if len(opts.ConfigPaths) > 0 {
		configRoot, err = openRealDir(root, "config")
		if err != nil {
			return err
		}
		defer configRoot.Close()
	}
	for _, name := range opts.ConfigPaths {
		if !modrinth.SafePath(name) || !strings.HasPrefix(name, "config/") {
			return errors.New("only explicitly selected config/ files can be exported")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := readRootFile(configRoot, strings.TrimPrefix(name, "config/"), modrinth.MaxConfigSize)
		if err != nil {
			return err
		}
		total += int64(len(data))
		if total > 64<<20 {
			return errors.New("selected configs exceed the export size limit")
		}
		configs[name] = data
	}
	if len(index.Files)+len(configs)+1 > modrinth.MaxFiles {
		return errors.New("pack exceeds the file count limit")
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("exported index exceeds the size limit")
	}
	archive := zip.NewWriter(contextWriter{ctx, writer})
	write := func(name string, data []byte) error {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0640)
		file, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		return err
	}
	if err := write("modrinth.index.json", data); err != nil {
		_ = archive.Close()
		return err
	}
	for _, name := range sortedKeys(configs) {
		if err := write("server-overrides/"+name, configs[name]); err != nil {
			_ = archive.Close()
			return err
		}
	}
	return archive.Close()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(data)
}
