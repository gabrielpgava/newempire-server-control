// Package fabric discovers authoritative runtime metadata and stages installations.
// Applying staged files, backups, EULA acceptance, and runtime jobs belong to callers.
package fabric

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gabrielpgava/newempire-server-control/internal/netguard"
)

const (
	metaOrigin       = "https://meta.fabricmc.net"
	mojangManifest   = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"
	maxMetadataBytes = 8 << 20
	LauncherTrust    = "official_fabric_https_without_published_checksum"
)

var allowedHosts = []string{"meta.fabricmc.net", "piston-meta.mojang.com", "launchermeta.mojang.com", "maven.fabricmc.net", "piston-data.mojang.com", "launcher.mojang.com"}
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}$`)
var gameVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._+\-]{0,127}$`)

type Client struct{ http *http.Client }

// NewClient(nil) uses the repository's public-address-only HTTPS transport.
// A supplied client is for trusted transports/tests, never end-user configuration;
// URL and redirect host validation still applies.
func NewClient(client *http.Client) *Client {
	if client == nil {
		client = netguard.New(allowedHosts...)
	}
	copyClient := *client
	previousRedirect := copyClient.CheckRedirect
	copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !officialURL(req.URL.String()) {
			return errors.New("unsafe upstream metadata redirect")
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	return &Client{http: &copyClient}
}

type GameVersion struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

type LoaderVersion struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	Maven   string `json:"maven"`
}

type InstallerVersion struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	URL     string `json:"url"`
	Maven   string `json:"maven"`
}

type Versions struct {
	Games      []GameVersion      `json:"games"`
	Loaders    []LoaderVersion    `json:"loaders"`
	Installers []InstallerVersion `json:"installers"`
}

func (c *Client) Versions(ctx context.Context) (Versions, error) {
	var versions Versions
	if err := c.metadata(ctx, metaOrigin+"/v2/versions/game", &versions.Games); err != nil {
		return versions, err
	}
	if err := c.metadata(ctx, metaOrigin+"/v2/versions/loader", &versions.Loaders); err != nil {
		return versions, err
	}
	if err := c.metadata(ctx, metaOrigin+"/v2/versions/installer", &versions.Installers); err != nil {
		return versions, err
	}
	if len(versions.Games) == 0 || len(versions.Loaders) == 0 || len(versions.Installers) == 0 {
		return versions, errors.New("Fabric returned an empty version catalog")
	}
	seen := make(map[string]bool)
	for _, game := range versions.Games {
		if !validGameVersion(game.Version) || seen[game.Version] {
			return versions, errors.New("Fabric returned an invalid game version")
		}
		seen[game.Version] = true
	}
	seen = make(map[string]bool)
	for _, loader := range versions.Loaders {
		if !versionPattern.MatchString(loader.Version) || loader.Maven != "net.fabricmc:fabric-loader:"+loader.Version || seen[loader.Version] {
			return versions, errors.New("Fabric returned invalid loader metadata")
		}
		seen[loader.Version] = true
	}
	seen = make(map[string]bool)
	for _, installer := range versions.Installers {
		if !versionPattern.MatchString(installer.Version) || !installerURL(installer) || installer.Maven != "net.fabricmc:fabric-installer:"+installer.Version || seen[installer.Version] {
			return versions, errors.New("Fabric returned invalid installer metadata")
		}
		seen[installer.Version] = true
	}
	return versions, nil
}

type loaderEntry struct {
	Loader       LoaderVersion `json:"loader"`
	Intermediary struct {
		Version string `json:"version"`
		Maven   string `json:"maven"`
	} `json:"intermediary"`
	LauncherMeta struct {
		MinJavaVersion int `json:"min_java_version"`
	} `json:"launcherMeta"`
}

func (c *Client) Loaders(ctx context.Context, game string) ([]LoaderVersion, error) {
	entries, err := c.gameLoaders(ctx, game)
	if err != nil {
		return nil, err
	}
	loaders := make([]LoaderVersion, 0, len(entries))
	for _, entry := range entries {
		loaders = append(loaders, entry.Loader)
	}
	return loaders, nil
}

func (c *Client) gameLoaders(ctx context.Context, game string) ([]loaderEntry, error) {
	if !validGameVersion(game) {
		return nil, errors.New("invalid Minecraft version")
	}
	var entries []loaderEntry
	if err := c.metadata(ctx, metaOrigin+"/v2/versions/loader/"+url.PathEscape(game), &entries); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("Minecraft version has no Fabric loader metadata")
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if !versionPattern.MatchString(entry.Loader.Version) || entry.Loader.Maven != "net.fabricmc:fabric-loader:"+entry.Loader.Version || !validGameVersion(entry.Intermediary.Version) || entry.Intermediary.Maven != "net.fabricmc:intermediary:"+entry.Intermediary.Version || entry.LauncherMeta.MinJavaVersion < 0 || entry.LauncherMeta.MinJavaVersion > 255 || seen[entry.Loader.Version] {
			return nil, errors.New("Fabric returned inconsistent game/loader metadata")
		}
		seen[entry.Loader.Version] = true
	}
	return entries, nil
}

type Selection struct {
	MinecraftVersion string `json:"minecraft_version"`
	LoaderVersion    string `json:"loader_version"`
	InstallerVersion string `json:"installer_version"`
}

type Download struct {
	URL  string `json:"url"`
	SHA1 string `json:"sha1"`
	Size int64  `json:"size"`
}

type InstallPlan struct {
	Selection
	IntermediaryVersion   string   `json:"intermediary_version"`
	JavaMajor             int      `json:"java_major"`
	LauncherURL           string   `json:"launcher_url"`
	LauncherTrust         string   `json:"launcher_trust"`
	MinecraftMetadataURL  string   `json:"minecraft_metadata_url"`
	MinecraftMetadataSHA1 string   `json:"minecraft_metadata_sha1"`
	MinecraftServer       Download `json:"minecraft_server"`
	Warnings              []string `json:"warnings"`
}

func (c *Client) Plan(ctx context.Context, selection Selection) (InstallPlan, error) {
	plan := InstallPlan{Selection: selection, Warnings: []string{}}
	if !validGameVersion(selection.MinecraftVersion) || !versionPattern.MatchString(selection.LoaderVersion) || !versionPattern.MatchString(selection.InstallerVersion) {
		return plan, errors.New("explicit Minecraft, Fabric loader, and installer versions are required")
	}
	versions, err := c.Versions(ctx)
	if err != nil {
		return plan, err
	}
	gameFound, loaderFound, installerFound := false, false, false
	for _, game := range versions.Games {
		if game.Version == selection.MinecraftVersion {
			gameFound = true
			if !game.Stable {
				plan.Warnings = append(plan.Warnings, "Selected Minecraft version is not marked stable by Fabric")
			}
		}
	}
	for _, loader := range versions.Loaders {
		if loader.Version == selection.LoaderVersion {
			loaderFound = true
		}
	}
	for _, installer := range versions.Installers {
		if installer.Version == selection.InstallerVersion {
			installerFound = true
			if !installer.Stable {
				plan.Warnings = append(plan.Warnings, "Selected installer is not marked stable by Fabric")
			}
		}
	}
	if !gameFound || !loaderFound || !installerFound {
		return plan, errors.New("selected versions are absent from authoritative Fabric metadata")
	}
	entries, err := c.gameLoaders(ctx, selection.MinecraftVersion)
	if err != nil {
		return plan, err
	}
	compatible := false
	for _, entry := range entries {
		if entry.Loader.Version == selection.LoaderVersion {
			compatible = true
			plan.IntermediaryVersion = entry.Intermediary.Version
			plan.JavaMajor = entry.LauncherMeta.MinJavaVersion
			if !entry.Loader.Stable {
				plan.Warnings = append(plan.Warnings, "Selected loader is not marked stable by Fabric")
			}
		}
	}
	if !compatible {
		return plan, errors.New("selected Fabric loader has no metadata for this Minecraft version")
	}
	var manifest struct {
		Versions []struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			SHA1 string `json:"sha1"`
		} `json:"versions"`
	}
	if err := c.metadata(ctx, mojangManifest, &manifest); err != nil {
		return plan, err
	}
	for _, version := range manifest.Versions {
		if version.ID != selection.MinecraftVersion {
			continue
		}
		if plan.MinecraftMetadataURL != "" {
			return plan, errors.New("Mojang returned duplicate version metadata")
		}
		if !metadataURL(version.URL) || !validSHA1(version.SHA1) {
			return plan, errors.New("Mojang returned invalid version metadata origin or hash")
		}
		plan.MinecraftMetadataURL, plan.MinecraftMetadataSHA1 = version.URL, version.SHA1
	}
	if plan.MinecraftMetadataURL == "" {
		return plan, errors.New("Minecraft version is absent from Mojang metadata")
	}
	data, err := c.get(ctx, plan.MinecraftMetadataURL, maxMetadataBytes)
	if err != nil {
		return plan, err
	}
	digest := sha1.Sum(data)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), plan.MinecraftMetadataSHA1) {
		return plan, errors.New("Mojang version metadata hash mismatch")
	}
	var minecraft struct {
		ID          string `json:"id"`
		JavaVersion struct {
			MajorVersion int `json:"majorVersion"`
		} `json:"javaVersion"`
		Downloads struct {
			Server Download `json:"server"`
		} `json:"downloads"`
	}
	if err := decodeMetadata(data, &minecraft); err != nil {
		return plan, err
	}
	if minecraft.ID != selection.MinecraftVersion || minecraft.JavaVersion.MajorVersion < 1 || minecraft.JavaVersion.MajorVersion > 255 {
		return plan, errors.New("Mojang does not declare a valid Java minimum for this version; choose a supported version instead of guessing")
	}
	if !serverURL(minecraft.Downloads.Server.URL) || !validSHA1(minecraft.Downloads.Server.SHA1) || minecraft.Downloads.Server.Size <= 0 || minecraft.Downloads.Server.Size > 1<<30 {
		return plan, errors.New("Mojang returned invalid server download metadata")
	}
	plan.JavaMajor = max(plan.JavaMajor, minecraft.JavaVersion.MajorVersion)
	plan.MinecraftServer = minecraft.Downloads.Server
	plan.LauncherURL = fmt.Sprintf("%s/v2/versions/loader/%s/%s/%s/server/jar", metaOrigin, url.PathEscape(selection.MinecraftVersion), url.PathEscape(selection.LoaderVersion), url.PathEscape(selection.InstallerVersion))
	plan.LauncherTrust = LauncherTrust
	plan.Warnings = append(plan.Warnings, "Fabric's generated launcher endpoint publishes no independent checksum; origin trust is official HTTPS, and the recorded SHA-256 is local integrity only", "The bootstrap downloads Minecraft and libraries on its first administrator-approved launch; staging does not execute Java or accept the EULA", "A Java minimum does not prove mod compatibility; test the selected Java and mods before migrating a world")
	return plan, nil
}

func (c *Client) metadata(ctx context.Context, address string, target any) error {
	data, err := c.get(ctx, address, maxMetadataBytes)
	if err != nil {
		return err
	}
	return decodeMetadata(data, target)
}

func decodeMetadata(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return errors.New("malformed upstream runtime metadata")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing upstream metadata")
	}
	return nil
}

func (c *Client) get(ctx context.Context, address string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !officialURL(address) {
		return nil, errors.New("runtime download URL is not an official HTTPS origin")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("invalid runtime metadata request")
	}
	req.Header.Set("User-Agent", "NewEmpireServerControl/runtime")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("runtime upstream request failed")
	}
	defer resp.Body.Close()
	if resp.Request != nil && !officialURL(resp.Request.URL.String()) {
		return nil, errors.New("runtime upstream returned an unsafe origin")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("runtime upstream returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return nil, errors.New("runtime upstream response exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("read runtime upstream response failed")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("runtime upstream response exceeds size limit")
	}
	return data, nil
}

func officialURL(address string) bool {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawQuery != "" || (u.Port() != "" && u.Port() != "443") || u.Path == "" || strings.ContainsAny(u.Path, "\x00\r\n\\") {
		return false
	}
	for _, host := range allowedHosts {
		if strings.EqualFold(host, u.Hostname()) {
			return true
		}
	}
	return false
}

func metadataURL(address string) bool {
	u, err := url.Parse(address)
	return err == nil && officialURL(address) && (u.Hostname() == "piston-meta.mojang.com" || u.Hostname() == "launchermeta.mojang.com") && strings.HasSuffix(u.Path, ".json")
}

func serverURL(address string) bool {
	u, err := url.Parse(address)
	return err == nil && officialURL(address) && (u.Hostname() == "piston-data.mojang.com" || u.Hostname() == "launcher.mojang.com") && strings.HasSuffix(u.Path, ".jar")
}

func installerURL(installer InstallerVersion) bool {
	u, err := url.Parse(installer.URL)
	return err == nil && officialURL(installer.URL) && u.Hostname() == "maven.fabricmc.net" && u.Path == "/net/fabricmc/fabric-installer/"+installer.Version+"/fabric-installer-"+installer.Version+".jar"
}

func validSHA1(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha1.Size
}

func validGameVersion(value string) bool {
	return gameVersionPattern.MatchString(value) && strings.TrimSpace(value) == value
}
