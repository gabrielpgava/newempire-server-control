package fabric

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const MaxLauncherBytes = 64 << 20

type StageResult struct {
	Directory        string      `json:"directory"`
	Plan             InstallPlan `json:"plan"`
	LauncherSHA256   string      `json:"launcher_sha256"`
	PropertiesSHA256 string      `json:"properties_sha256"`
}

// Stage creates a private, unique directory under a trusted staging root. It
// never changes an instance, starts Java, writes eula.txt, or overwrites a file.
// The caller must use a panel-owned staging root outside all instance roots.
func (c *Client) Stage(ctx context.Context, selection Selection, stagingRoot string, properties []byte) (result StageResult, err error) {
	if !filepath.IsAbs(stagingRoot) {
		return result, errors.New("runtime staging root must be absolute")
	}
	values, err := ReadProperties(properties)
	if err != nil {
		return result, err
	}
	for key, value := range values {
		if propertyKeyPattern.MatchString(key) {
			if err := ValidateProperty(key, value); err != nil {
				return result, err
			}
		}
	}
	result.Plan, err = c.Plan(ctx, selection)
	if err != nil {
		return result, err
	}
	launcher, err := c.get(ctx, result.Plan.LauncherURL, MaxLauncherBytes)
	if err != nil {
		return result, err
	}
	if err := validateLauncher(ctx, launcher, selection); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	root, err := os.OpenRoot(stagingRoot)
	if err != nil {
		return result, fmt.Errorf("open runtime staging root: %w", err)
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil || info.Mode().Perm() != 0700 {
		return result, errors.New("runtime staging root must be private (0700)")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return result, err
	}
	name := "runtime-" + hex.EncodeToString(random[:])
	if err := root.Mkdir(name, 0700); err != nil {
		return result, fmt.Errorf("create runtime stage: %w", err)
	}
	stage, err := root.OpenRoot(name)
	if err != nil {
		_ = root.Remove(name)
		return result, err
	}
	defer stage.Close()
	defer func() {
		if err != nil {
			for _, file := range []string{"launcher.jar", "server.properties", "manifest.json"} {
				_ = stage.Remove(file)
			}
			_ = root.Remove(name)
		}
	}()
	launcherHash, propertiesHash := sha256.Sum256(launcher), sha256.Sum256(properties)
	result.Directory = filepath.Join(stagingRoot, name)
	result.LauncherSHA256 = hex.EncodeToString(launcherHash[:])
	result.PropertiesSHA256 = hex.EncodeToString(propertiesHash[:])
	if err = writeStaged(stage, "launcher.jar", launcher); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err = writeStaged(stage, "server.properties", properties); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	manifest, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return result, err
	}
	if err = writeStaged(stage, "manifest.json", append(manifest, '\n')); err != nil {
		return result, err
	}
	dir, err := stage.Open(".")
	if err != nil {
		return result, err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return result, fmt.Errorf("sync runtime stage: %w", err)
	}
	parent, err := root.Open(".")
	if err != nil {
		return result, err
	}
	defer parent.Close()
	if err = parent.Sync(); err != nil {
		return result, fmt.Errorf("sync runtime staging root: %w", err)
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func writeStaged(root *os.Root, name string, data []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create staged runtime file: %w", err)
	}
	defer file.Close()
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write staged runtime file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync staged runtime file: %w", err)
	}
	return file.Close()
}

func validateLauncher(ctx context.Context, data []byte, selection Selection) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) == 0 || len(archive.File) > 10000 {
		return errors.New("Fabric launcher is not a bounded JAR archive")
	}
	var manifest, properties, bootstrap *zip.File
	var total uint64
	seen := make(map[string]bool, len(archive.File))
	for _, file := range archive.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if file.UncompressedSize64 > 64<<20 || total > (256<<20)-file.UncompressedSize64 {
			return errors.New("Fabric launcher exceeds expanded size limit")
		}
		total += file.UncompressedSize64
		if !validJarPath(file.Name) || file.Mode()&os.ModeSymlink != 0 {
			return errors.New("Fabric launcher contains an unsafe archive entry")
		}
		if seen[file.Name] {
			return errors.New("Fabric launcher has duplicate archive entries")
		}
		seen[file.Name] = true
		reader, err := file.Open()
		if err != nil {
			return errors.New("cannot read Fabric launcher archive entry")
		}
		checksum := crc32.NewIEEE()
		n, readErr := io.Copy(checksum, io.LimitReader(reader, int64(file.UncompressedSize64)+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || uint64(n) != file.UncompressedSize64 || checksum.Sum32() != file.CRC32 {
			return errors.New("Fabric launcher archive integrity check failed")
		}
		switch file.Name {
		case "META-INF/MANIFEST.MF":
			manifest = file
		case "install.properties":
			properties = file
		case "net/fabricmc/installer/ServerLauncher.class":
			bootstrap = file
		}
	}
	if manifest == nil || properties == nil || bootstrap == nil || bootstrap.UncompressedSize64 < 8 || manifest.UncompressedSize64 == 0 || manifest.UncompressedSize64 > 64<<10 || properties.UncompressedSize64 > 32<<10 {
		return errors.New("Fabric launcher is missing bounded bootstrap metadata")
	}
	classReader, err := bootstrap.Open()
	if err != nil {
		return errors.New("cannot read Fabric server bootstrap class")
	}
	var magic [4]byte
	_, classErr := io.ReadFull(classReader, magic[:])
	classCloseErr := classReader.Close()
	if classErr != nil || classCloseErr != nil || magic != [4]byte{0xCA, 0xFE, 0xBA, 0xBE} {
		return errors.New("Fabric server bootstrap class is malformed")
	}
	manifestReader, err := manifest.Open()
	if err != nil {
		return errors.New("cannot read Fabric launcher manifest")
	}
	manifestData, manifestErr := io.ReadAll(io.LimitReader(manifestReader, (64<<10)+1))
	closeErr := manifestReader.Close()
	if manifestErr != nil || closeErr != nil || len(manifestData) > 64<<10 {
		return errors.New("invalid Fabric launcher manifest")
	}
	mainClass := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(manifestData), "\r\n", "\n"), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Main-Class: ") {
			if mainClass != "" {
				return errors.New("Fabric launcher has duplicate main classes")
			}
			mainClass = strings.TrimPrefix(line, "Main-Class: ")
		}
	}
	if mainClass != "net.fabricmc.installer.ServerLauncher" {
		return errors.New("Fabric launcher does not declare the supported server bootstrap entry point")
	}
	reader, err := properties.Open()
	if err != nil {
		return errors.New("cannot read Fabric launcher properties")
	}
	defer reader.Close()
	data, err = io.ReadAll(io.LimitReader(reader, 32<<10+1))
	if err != nil || len(data) > 32<<10 {
		return errors.New("invalid Fabric launcher properties")
	}
	values, err := ReadProperties(data)
	if err != nil || values["game-version"] != selection.MinecraftVersion || values["fabric-loader-version"] != selection.LoaderVersion {
		return errors.New("Fabric launcher versions do not match the installation plan")
	}
	return nil
}

func validJarPath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, ":\\\x00") {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(name, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
