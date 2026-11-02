package instances

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/config"
	"github.com/gabrielpgava/newempire-server-control/internal/database"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"github.com/gabrielpgava/newempire-server-control/internal/secrets"
)

type Service struct {
	DB     *database.Store
	Vault  *secrets.Vault
	Config config.Config
}

func (s *Service) Save(ctx context.Context, i model.Instance) error {
	if err := s.Validate(i); err != nil {
		return err
	}
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	secret, err := s.Vault.Seal([]byte(i.RCONSecret), "rcon:"+i.ID)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "INSERT INTO instances(id,name,root,port,rcon_port,data,secret) VALUES(?,?,?,?,?,?,?)", i.ID, i.Name, i.Root, i.Port, i.RCONPort, string(b), secret)
	if err != nil {
		return fmt.Errorf("could not register instance; directory or port may already be registered: %w", err)
	}
	return nil
}

func (s *Service) Update(ctx context.Context, i model.Instance) error {
	if err := s.Validate(i); err != nil {
		return err
	}
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE instances SET name=?,data=? WHERE id=?", i.Name, string(b), i.ID)
	return err
}

func (s *Service) Get(ctx context.Context, id string) (model.Instance, error) {
	var i model.Instance
	if !model.ValidID(id) {
		return i, errors.New("invalid instance identifier")
	}
	var data string
	var encrypted []byte
	if err := s.DB.QueryRowContext(ctx, "SELECT data,secret FROM instances WHERE id=?", id).Scan(&data, &encrypted); err != nil {
		return i, err
	}
	if err := json.Unmarshal([]byte(data), &i); err != nil {
		return i, err
	}
	plain, err := s.Vault.Open(encrypted, "rcon:"+id)
	if err != nil {
		return i, errors.New("RCON credential could not be decrypted; restore the master key")
	}
	i.RCONSecret = string(plain)
	return i, nil
}

func (s *Service) List(ctx context.Context) ([]model.Instance, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT data FROM instances ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []model.Instance{}
	for rows.Next() {
		var b string
		var i model.Instance
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(b), &i); err != nil {
			return nil, err
		}
		all = append(all, i)
	}
	return all, rows.Err()
}

func (s *Service) Validate(i model.Instance) error {
	if !model.ValidID(i.ID) || len(i.Name) < 1 || len(i.Name) > 100 || strings.TrimSpace(i.Name) != i.Name {
		return errors.New("invalid instance name or identifier")
	}
	if i.Port < 1024 || i.Port > 65535 || i.RCONPort < 1024 || i.RCONPort > 65535 || i.Port == i.RCONPort {
		return errors.New("game and RCON ports must be different unprivileged ports")
	}
	if i.MemoryMB < 512 || i.MemoryMB > 1048576 {
		return errors.New("memory must be between 512 MiB and 1 TiB")
	}
	if !i.EULA {
		return errors.New("Minecraft EULA must be explicitly accepted")
	}
	if !filepath.IsAbs(i.JavaPath) || !slices.Contains(s.Config.JavaPaths, i.JavaPath) {
		return errors.New("Java executable is not in the administrator allowlist")
	}
	if i.Jar == "" || strings.ContainsAny(i.Jar, "\\\x00\r\n") || filepath.IsAbs(i.Jar) || filepath.Clean(i.Jar) != i.Jar || strings.HasPrefix(i.Jar, "..") || !strings.HasSuffix(i.Jar, ".jar") {
		return errors.New("launcher must be a safe relative JAR path")
	}
	if strings.ContainsAny(i.Root, "\r\n\x00") {
		return errors.New("invalid instance directory")
	}
	for _, component := range strings.Split(filepath.ToSlash(i.Root), "/") {
		lower := strings.ToLower(component)
		if strings.HasPrefix(lower, ".nesc-") || strings.HasPrefix(lower, ".newempire-") {
			return errors.New("internal recovery directories cannot be adopted as managed instances")
		}
	}
	for _, reserved := range []string{s.Config.BackupDir, filepath.Join(s.Config.DataDir, "staging"), filepath.Join(s.Config.DataDir, "uploads")} {
		if filepath.Clean(i.Root) == filepath.Clean(reserved) {
			return errors.New("instance directory overlaps protected panel storage")
		}
		if _, err := Relative(i.Root, reserved); err == nil {
			return errors.New("instance directory contains protected panel storage")
		}
		if _, err := Relative(reserved, i.Root); err == nil {
			return errors.New("instance directory is inside protected panel storage")
		}
	}
	if filepath.Clean(i.Root) == filepath.Clean(s.Config.DataDir) {
		return errors.New("panel data directory cannot be adopted as an instance")
	}
	if _, err := Relative(i.Root, s.Config.DataDir); err == nil {
		return errors.New("instance directory cannot contain the panel data directory")
	}
	root, err := s.OpenRoot(i.Root)
	if err != nil {
		return err
	}
	return root.Close()
}

func Relative(base, path string) (string, error) {
	if !filepath.IsAbs(base) || !filepath.IsAbs(path) {
		return "", errors.New("directory must be absolute")
	}
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("directory must be below an administrator-allowlisted root")
	}
	return rel, nil
}

// OpenRoot anchors the instance below a trusted root even if a descendant symlink changes.
func (s *Service) OpenRoot(path string) (*os.Root, error) {
	roots := append([]string{s.Config.InstanceDir}, s.Config.AdoptRoots...)
	for _, base := range roots {
		rel, err := Relative(base, path)
		if err != nil {
			continue
		}
		parent, err := os.OpenRoot(base)
		if err != nil {
			return nil, err
		}
		root, err := parent.OpenRoot(rel)
		parent.Close()
		if err != nil {
			return nil, fmt.Errorf("open allowlisted instance directory: %w", err)
		}
		return root, nil
	}
	return nil, errors.New("instance directory is outside all administrator-allowlisted roots")
}

func (s *Service) OpenParent(path string) (*os.Root, string, error) {
	for _, base := range append([]string{s.Config.InstanceDir}, s.Config.AdoptRoots...) {
		rel, err := Relative(base, path)
		if err != nil {
			continue
		}
		root, err := os.OpenRoot(base)
		if err != nil {
			return nil, "", err
		}
		parent, err := root.OpenRoot(filepath.Dir(rel))
		root.Close()
		if err != nil {
			return nil, "", err
		}
		return parent, filepath.Base(rel), nil
	}
	return nil, "", errors.New("instance parent is outside all administrator-allowlisted roots")
}

func (s *Service) CheckCollisions(ctx context.Context, root string, port, rconPort int) error {
	all, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.Root == root || port == other.Port || port == other.RCONPort || rconPort == other.Port || rconPort == other.RCONPort {
			return errors.New("instance port or directory collides with an existing instance")
		}
		if _, err := Relative(other.Root, root); err == nil {
			return errors.New("instance directories must not overlap")
		}
		if _, err := Relative(root, other.Root); err == nil {
			return errors.New("instance directories must not overlap")
		}
		candidate, err1 := os.Stat(root)
		existing, err2 := os.Stat(other.Root)
		if err1 == nil && err2 == nil && os.SameFile(candidate, existing) {
			return errors.New("instance directory refers to an already registered directory")
		}
	}
	for _, p := range []int{port, rconPort} {
		listener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", p))
		if err != nil {
			return fmt.Errorf("port %d is already in use or unavailable", p)
		}
		listener.Close()
	}
	return nil
}
