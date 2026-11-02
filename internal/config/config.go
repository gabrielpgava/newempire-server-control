package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Listen string `json:"listen"`
	PublicURL string `json:"public_url"`
	DataDir string `json:"data_dir"`
	InstanceDir string `json:"instance_dir"`
	BackupDir string `json:"backup_dir"`
	AdoptRoots []string `json:"adopt_roots"`
	JavaPaths []string `json:"java_paths"`
	RuntimeHelper string `json:"runtime_helper"`
	SecureCookies bool `json:"secure_cookies"`
	MaxUploadBytes int64 `json:"max_upload_bytes"`
	ReleaseRepository string `json:"release_repository"`
	ReleasePublicKey string `json:"release_public_key"`
	InstallDir string `json:"install_dir"`
}

func Default() Config {
	return Config{Listen:"127.0.0.1:8080", PublicURL:"http://127.0.0.1:8080", DataDir:"data", RuntimeHelper:"/usr/local/libexec/new-empire-runtime", MaxUploadBytes: 32 << 30, ReleaseRepository:"gabrielpgava/newempire-server-control", InstallDir:"/opt/new-empire-control"}
}

func Load(path string) (Config, error) {
	c := Default()
	if path != "" {
		f, err := os.Open(path); if err != nil { return c, err }; defer f.Close()
		d := json.NewDecoder(f); d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil { return c, err }
	}
	for key, dst := range map[string]*string{"NEC_LISTEN": &c.Listen,"NEC_PUBLIC_URL": &c.PublicURL,"NEC_DATA_DIR": &c.DataDir} {
		if value := os.Getenv(key); value != "" { *dst = value }
	}
	var err error
	c.DataDir, err = filepath.Abs(c.DataDir); if err != nil { return c, err }
	if c.InstanceDir == "" { c.InstanceDir = filepath.Join(c.DataDir,"instances") }
	if c.BackupDir == "" { c.BackupDir = filepath.Join(c.DataDir,"backups") }
	c.InstanceDir, err = filepath.Abs(c.InstanceDir); if err != nil { return c, err }
	c.BackupDir, err = filepath.Abs(c.BackupDir); if err != nil { return c, err }
	if within(c.InstanceDir,c.BackupDir) || within(c.BackupDir,c.InstanceDir) { return c, errors.New("backup and instance directories must be separate") }
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" && u.Path != "/" { return c, errors.New("public_url must be an HTTP(S) origin") }
	if u.Scheme == "https" { c.SecureCookies = true }
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" { return c, errors.New("non-loopback public_url requires HTTPS") }
	if c.MaxUploadBytes < 1 { return c, errors.New("max_upload_bytes must be positive") }
	return c, nil
}

func within(root, path string) bool {
	r, err := filepath.Rel(root,path)
	return err == nil && !filepath.IsAbs(r) && r != ".." && !strings.HasPrefix(r,".."+string(filepath.Separator))
}

func (c Config) Prepare() error {
	for _, path := range []string{c.DataDir,c.InstanceDir,c.BackupDir,filepath.Join(c.DataDir,"staging"),filepath.Join(c.DataDir,"uploads")} {
		if err := os.MkdirAll(path,0700); err != nil { return fmt.Errorf("create application directory: %w",err) }
	}
	return nil
}
