package fabric

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/gabrielpgava/newempire-server-control/internal/files"
)

// SourceRuntime describes an inspected unattended Fabric bootstrap, not its origin or mod compatibility.
type SourceRuntime struct {
	MinecraftVersion string `json:"minecraft_version"`
	LoaderVersion    string `json:"loader_version"`
	JavaMajor        int    `json:"java_major"`
}

// InspectSourceLauncher never executes uploaded code. Legacy/custom launchers without
// embedded install.properties are deliberately unsupported rather than guessed.
func InspectSourceLauncher(ctx context.Context, file *os.File) (SourceRuntime, error) {
	var source SourceRuntime
	info, err := file.Stat()
	if err != nil {
		return source, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxLauncherBytes {
		return source, errors.New("source launcher must be a bounded regular JAR")
	}
	var data bytes.Buffer
	if _, err := files.Copy(ctx, &data, io.NewSectionReader(file, 0, info.Size()), MaxLauncherBytes, nil); err != nil {
		return source, err
	}
	archive, err := zip.NewReader(bytes.NewReader(data.Bytes()), int64(data.Len()))
	if err != nil || len(archive.File) > 10000 {
		return source, errors.New("source launcher is not a supported bounded Fabric bootstrap JAR")
	}
	properties, err := archive.Open("install.properties")
	if err != nil {
		return source, errors.New("source launcher has no embedded Fabric install.properties; legacy/custom launchers are unsupported")
	}
	raw, readErr := io.ReadAll(io.LimitReader(properties, (32<<10)+1))
	err = errors.Join(readErr, properties.Close())
	if err != nil || len(raw) > 32<<10 {
		return source, errors.New("source launcher has invalid Fabric install.properties")
	}
	values, err := ReadProperties(raw)
	if err != nil || !validGameVersion(values["game-version"]) || !versionPattern.MatchString(values["fabric-loader-version"]) {
		return source, errors.New("source launcher does not declare valid Minecraft/Fabric versions")
	}
	source.MinecraftVersion, source.LoaderVersion = values["game-version"], values["fabric-loader-version"]
	if err := validateLauncher(ctx, data.Bytes(), Selection{MinecraftVersion: source.MinecraftVersion, LoaderVersion: source.LoaderVersion}); err != nil {
		return SourceRuntime{}, err
	}
	bootstrap, err := archive.Open("net/fabricmc/installer/ServerLauncher.class")
	if err != nil {
		return SourceRuntime{}, err
	}
	var header [8]byte
	_, readErr = io.ReadFull(bootstrap, header[:])
	if err := errors.Join(readErr, bootstrap.Close()); err != nil {
		return SourceRuntime{}, err
	}
	major := int(binary.BigEndian.Uint16(header[6:])) - 44
	if major < 1 || major > 255 || binary.BigEndian.Uint16(header[4:]) != 0 {
		return SourceRuntime{}, errors.New("source bootstrap has an unsupported Java class version")
	}
	source.JavaMajor = major
	return source, nil
}
