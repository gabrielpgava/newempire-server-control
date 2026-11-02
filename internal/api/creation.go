package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
	"github.com/gabrielpgava/newempire-server-control/internal/model"
)

type createRequest struct {
	Mode             string `json:"mode"`
	Name             string `json:"name"`
	JavaPath         string `json:"java_path"`
	Jar              string `json:"jar"`
	MinecraftVersion string `json:"minecraft_version"`
	LoaderVersion    string `json:"loader_version"`
	InstallerVersion string `json:"installer_version"`
	MemoryMB         int    `json:"memory_mb"`
	Port             int    `json:"port"`
	RCONPort         int    `json:"rcon_port"`
	EULA             bool   `json:"eula"`
	UploadID         string `json:"upload_id"`
	Path             string `json:"path"`
	PreviewHash      string `json:"preview_hash"`
	ServerRoot       string `json:"server_root,omitempty"`
	ReservationID    string `json:"reservation_id,omitempty"`
	Owner            string `json:"owner,omitempty"`
}

func (a *App) javaInstallations(w http.ResponseWriter, r *http.Request) {
	all, err := fabric.InspectJavaInstallations(r.Context(), a.Config.JavaPaths)
	if err != nil {
		fail(w, 503, err)
		return
	}
	respond(w, 200, all)
}
func (a *App) fabricVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := a.Fabric.Versions(r.Context())
	if err != nil {
		fail(w, 502, err)
		return
	}
	games, loaders, installers := []string{}, []string{}, []string{}
	for _, game := range versions.Games {
		if game.Stable {
			games = append(games, game.Version)
		}
	}
	choices := versions.Loaders
	if game := r.URL.Query().Get("minecraft_version"); game != "" {
		choices, err = a.Fabric.Loaders(r.Context(), game)
		if err != nil {
			fail(w, 502, err)
			return
		}
	}
	for _, loader := range choices {
		if loader.Stable {
			loaders = append(loaders, loader.Version)
		}
	}
	for _, installer := range versions.Installers {
		if installer.Stable {
			installers = append(installers, installer.Version)
		}
	}
	respond(w, 200, map[string]any{"game_versions": games, "loader_versions": loaders, "installer_versions": installers})
}
func (a *App) createInstance(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if body.Mode != "create" && body.Mode != "import" && body.Mode != "adopt" {
		fail(w, 400, errors.New("select create, import, or adopt"))
		return
	}
	if !body.EULA {
		fail(w, 400, errors.New("Minecraft EULA acceptance must be explicit"))
		return
	}
	if !slices.Contains(a.Config.JavaPaths, body.JavaPath) {
		fail(w, 400, errors.New("select an administrator-allowlisted Java installation"))
		return
	}
	if !validCreationName(body.Name) || body.MemoryMB < 512 || body.MemoryMB > 1048576 || body.Port < 1024 || body.Port > 65535 || body.RCONPort < 1024 || body.RCONPort > 65535 || body.Port == body.RCONPort {
		fail(w, 400, errors.New("invalid instance name, memory, or port configuration"))
		return
	}
	if body.Mode != "create" && body.PreviewHash == "" {
		fail(w, 400, errors.New("inspect the import/adoption and confirm its preview first"))
		return
	}
	if body.Mode != "create" {
		if err := files.ValidatePath(body.ServerRoot); err != nil {
			fail(w, 400, errors.New("confirm the exact server_root reported by source inspection"))
			return
		}
	} else if body.ServerRoot != "" || body.ReservationID != "" {
		fail(w, 400, errors.New("source roots and adoption reservations do not apply to new Fabric installations"))
		return
	}
	if body.Mode != "adopt" && body.ReservationID != "" {
		fail(w, 400, errors.New("reservation_id is only valid for adoption"))
		return
	}
	if body.Jar == "" {
		if body.Mode != "create" {
			fail(w, 400, errors.New("choose the exact launcher path from the import report"))
			return
		}
		body.Jar = "fabric-server-launch.jar"
	}
	if err := files.ValidatePath(body.Jar); err != nil || strings.HasPrefix(body.Jar, "..") || !strings.HasSuffix(body.Jar, ".jar") {
		fail(w, 400, errors.New("launcher must be a safe relative JAR path"))
		return
	}
	body.Owner = session(r).User.ID
	id := model.NewID()
	if body.Mode == "adopt" {
		id = body.ReservationID
		if _, err := a.adoptionInstance(r.Context(), body, id); err != nil {
			fail(w, 400, err)
			return
		}
	}
	a.enqueue(w, r, id, "instance_create", body)
}

func validCreationName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0 && len(name) <= 100
}

type adoptionReservation struct {
	Owner       string          `json:"owner"`
	Path        string          `json:"path"`
	PreviewHash string          `json:"preview_hash"`
	CreatedAt   time.Time       `json:"created_at"`
	Instance    *model.Instance `json:"instance,omitempty"`
}

func (a *App) reserveAdoption(ctx context.Context, owner, path, hash, id string) (string, error) {
	if id != "" {
		_, err := a.adoptionReservation(ctx, owner, path, hash, id)
		return id, err
	}
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var key string
	err = tx.QueryRowContext(ctx, "SELECT key FROM settings WHERE key LIKE 'adoption-reservation:%' AND json_extract(value,'$.owner')=? AND json_extract(value,'$.path')=? AND json_extract(value,'$.preview_hash')=? ORDER BY key LIMIT 1", owner, path, hash).Scan(&key)
	if err == nil {
		return strings.TrimPrefix(key, "adoption-reservation:"), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = model.NewID()
	raw, err := json.Marshal(adoptionReservation{Owner: owner, Path: path, PreviewHash: hash, CreatedAt: time.Now().UTC()})
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?)", "adoption-reservation:"+id, string(raw)); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func (a *App) adoptionReservation(ctx context.Context, owner, path, hash, id string) (adoptionReservation, error) {
	var reservation adoptionReservation
	if !model.ValidID(id) {
		return reservation, errors.New("inspect the adoption source to obtain a valid reservation_id")
	}
	if err := a.setting(ctx, "adoption-reservation:"+id, &reservation); err != nil {
		return reservation, err
	}
	if reservation.Owner != owner || reservation.Path != path || reservation.PreviewHash != hash || reservation.CreatedAt.IsZero() {
		return adoptionReservation{}, errors.New("adoption reservation is missing, belongs to another owner/path, or its preview changed; inspect the source again")
	}
	return reservation, nil
}

func creationInstance(body createRequest, id string, createdAt time.Time) model.Instance {
	return model.Instance{ID: id, Name: body.Name, Root: body.Path, JavaPath: body.JavaPath, Jar: body.Jar, MinecraftVersion: body.MinecraftVersion, LoaderVersion: body.LoaderVersion, MemoryMB: body.MemoryMB, Port: body.Port, RCONPort: body.RCONPort, EULA: body.EULA, Adopted: body.Mode == "adopt", CreatedAt: createdAt}
}

func (a *App) adoptionInstance(ctx context.Context, body createRequest, id string) (model.Instance, error) {
	reservation, err := a.adoptionReservation(ctx, body.Owner, body.Path, body.PreviewHash, id)
	if err != nil {
		return model.Instance{}, err
	}
	if body.ServerRoot != "." || !filepath.IsAbs(body.Path) || filepath.Clean(body.Path) != body.Path {
		return model.Instance{}, errors.New("adoption requires the inspected absolute server directory and server_root '.'")
	}
	i := creationInstance(body, id, reservation.CreatedAt)
	if reservation.Instance != nil && etagJSON(*reservation.Instance) != etagJSON(i) {
		return model.Instance{}, errors.New("adoption registration details are fixed after provisioning is attempted; retry with the original name, runtime, launcher, memory, and ports")
	}
	return i, nil
}

func (a *App) bindAdoption(ctx context.Context, body createRequest, i model.Instance) error {
	reservation, err := a.adoptionReservation(ctx, body.Owner, body.Path, body.PreviewHash, i.ID)
	if err != nil {
		return err
	}
	if reservation.Instance != nil && etagJSON(*reservation.Instance) != etagJSON(i) {
		return errors.New("adoption registration changed; retry the original reserved configuration")
	}
	reservation.Instance = &i
	return a.saveSetting(ctx, "adoption-reservation:"+i.ID, reservation)
}

func validateSourceChoice(body createRequest, preview importPreview, java fabric.JavaInstallation) error {
	if preview.PreviewHash != body.PreviewHash || preview.ServerRoot != body.ServerRoot {
		return errors.New("source preview or server root changed; inspect it again")
	}
	if len(preview.Launchers) != 1 || body.Jar != preview.Launchers[0] || body.MinecraftVersion != preview.MinecraftVersion || body.LoaderVersion != preview.LoaderVersion {
		return errors.New("launcher and Minecraft/Fabric versions must match the inspected source; importing or adopting does not migrate a runtime")
	}
	if java.Major < preview.JavaRequired {
		return fmt.Errorf("source runtime requires Java %d or newer", preview.JavaRequired)
	}
	return nil
}

func (a *App) provision(ctx context.Context, i model.Instance, reconfigure bool) error {
	if a.Provision != nil {
		return a.Provision(ctx, i, reconfigure)
	}
	if runtime.GOOS != "linux" {
		return errors.New("native instance provisioning requires Linux/systemd; development hosts cannot provision a service")
	}
	if !filepath.IsAbs(a.Config.RuntimeHelper) {
		return errors.New("restricted runtime helper must be configured with an absolute path")
	}
	body, err := json.Marshal(i)
	if err != nil {
		return err
	}
	operation := "provision"
	args := []string{"-n", a.Config.RuntimeHelper, operation}
	if reconfigure {
		args = []string{"-n", a.Config.RuntimeHelper, "reconfigure", i.ID}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/sudo", args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	cmd.Stdin = strings.NewReader(string(body))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restricted instance provisioning failed; review root-owned runtime allowlists and helper diagnostics: %w", err)
	}
	return nil
}

func (a *App) selection(ctx context.Context, game, loader, installer string) (fabric.Selection, error) {
	if installer == "" {
		versions, err := a.Fabric.Versions(ctx)
		if err != nil {
			return fabric.Selection{}, err
		}
		for _, v := range versions.Installers {
			if v.Stable {
				installer = v.Version
				break
			}
		}
	}
	if installer == "" {
		return fabric.Selection{}, errors.New("select an installer version from the authoritative Fabric catalog")
	}
	return fabric.Selection{MinecraftVersion: game, LoaderVersion: loader, InstallerVersion: installer}, nil
}

func (a *App) prepareOffline(ctx context.Context, i model.Instance) error {
	if err := a.assertStopped(ctx, i); err != nil {
		return err
	}
	if a.Prepare != nil {
		return a.Prepare(ctx, i)
	}
	if runtime.GOOS != "linux" {
		return errors.New("managed runtime preparation requires Linux")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", a.Config.RuntimeHelper, "prepare", i.ID)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	if err := command.Run(); err != nil {
		return errors.New("offline service preparation failed; check ownership, ACL support, and remaining instance processes")
	}
	return nil
}
func readOptionalText(ctx context.Context, root *os.Root, name string) (string, error) {
	text, err := files.ReadText(ctx, root, name, 1<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return text, err
}

func (a *App) registerInstanceJobs() {
	a.Jobs.Register("instance_create", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body createRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		if !validCreationName(body.Name) || files.ValidatePath(body.Jar) != nil || strings.HasPrefix(body.Jar, "..") || !strings.HasSuffix(body.Jar, ".jar") {
			return nil, errors.New("invalid instance name or launcher; no provisioning was attempted")
		}
		i := creationInstance(body, t.Job.InstanceID, time.Now().UTC())
		i.Root = filepath.Join(a.Config.InstanceDir, i.ID)
		if i.Adopted {
			var err error
			i, err = a.adoptionInstance(ctx, body, t.Job.InstanceID)
			if err != nil {
				return nil, err
			}
			existing, err := a.Instances.Get(ctx, i.ID)
			if err == nil {
				if etagJSON(existing) != etagJSON(i) {
					return nil, errors.New("reserved instance was already registered differently")
				}
				return map[string]any{"instance": existing, "state": "registered"}, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		} else {
			i.RCONSecret = auth.Token()
		}
		if err := t.Progress(ctx, 2, "Checking Java, ports, directory collisions, and the approved preview"); err != nil {
			return nil, err
		}
		if err := a.Instances.CheckCollisions(ctx, i.Root, i.Port, i.RCONPort); err != nil {
			return nil, err
		}
		java, err := fabric.InspectJava(ctx, i.JavaPath, a.Config.JavaPaths)
		if err != nil {
			return nil, err
		}
		var stageRoot *os.Root
		var runtimePlan fabric.InstallPlan
		if body.Mode == "create" {
			selection, err := a.selection(ctx, i.MinecraftVersion, i.LoaderVersion, body.InstallerVersion)
			if err != nil {
				return nil, err
			}
			runtimePlan, err = a.Fabric.Plan(ctx, selection)
			if err != nil {
				return nil, err
			}
			if java.Major < runtimePlan.JavaMajor {
				return nil, fmt.Errorf("selected runtime requires Java %d or newer", runtimePlan.JavaMajor)
			}
			props, err := fabric.ManagedProperties(i, []byte("# Managed Minecraft server configuration\nonline-mode=true\nwhite-list=true\n"))
			if err != nil {
				return nil, err
			}
			staged, err := a.Fabric.Stage(ctx, selection, filepath.Join(a.Config.DataDir, "staging"), props)
			if err != nil {
				return nil, err
			}
			if etagJSON(staged.Plan) != etagJSON(runtimePlan) {
				return nil, errors.New("runtime metadata changed during staging; review the version selection again")
			}
			stageRoot, err = os.OpenRoot(staged.Directory)
			if err != nil {
				return nil, err
			}
			defer stageRoot.Close()
		} else if body.Mode == "import" {
			p, err := a.inspectUpload(ctx, body.Owner, body.UploadID)
			if err != nil {
				return nil, err
			}
			stagingParent, err := os.OpenRoot(filepath.Join(a.Config.DataDir, "staging"))
			if err != nil {
				return nil, err
			}
			defer stagingParent.Close()
			stageName := "instance-" + i.ID
			if err := stagingParent.Mkdir(stageName, 0700); err != nil {
				return nil, err
			}
			stageRoot, err = stagingParent.OpenRoot(stageName)
			if err != nil {
				return nil, err
			}
			defer stageRoot.Close()
			free, err := freeDisk(stageRoot)
			if err != nil {
				return nil, err
			}
			if uint64(p.Bytes) > free {
				return nil, errors.New("insufficient staging disk space for the import")
			}
			extracted, err := a.extractUpload(ctx, body.Owner, body.UploadID, stageRoot)
			if err != nil {
				return nil, err
			}
			if extracted.Hash != p.Hash {
				return nil, errors.New("upload changed while extracting")
			}
			preview, err := a.instanceSourcePreview(ctx, stageRoot, p.Entries, p.Hash)
			if err != nil {
				return nil, err
			}
			if err := validateSourceChoice(body, preview, java); err != nil {
				return nil, err
			}
			if preview.ServerRoot != "." {
				stageRoot, err = stageRoot.OpenRoot(preview.ServerRoot)
				if err != nil {
					return nil, err
				}
				defer stageRoot.Close()
			}
			launcher, openErr := files.OpenRegular(stageRoot, i.Jar)
			if openErr != nil {
				return nil, errors.New("selected launcher is missing or unsafe")
			}
			launcher.Close()
			old, err := readOptionalText(ctx, stageRoot, "server.properties")
			if err != nil {
				return nil, err
			}
			props, err := fabric.ManagedProperties(i, []byte(old))
			if err != nil {
				return nil, err
			}
			if err := files.WriteText(ctx, stageRoot, "server.properties", string(props), 1<<20); err != nil {
				return nil, err
			}
		} else {
			root, err := a.Instances.OpenRoot(i.Root)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			m, err := treeManifest(ctx, root, "")
			if err != nil {
				return nil, err
			}
			preview, err := a.instanceSourcePreview(ctx, root, m.Entries, manifestHash(m))
			if err != nil {
				return nil, err
			}
			if err := validateSourceChoice(body, preview, java); err != nil {
				return nil, err
			}
			propsRaw, err := files.ReadText(ctx, root, "server.properties", 1<<20)
			if err != nil {
				return nil, err
			}
			props, err := fabric.ReadProperties([]byte(propsRaw))
			if err != nil {
				return nil, err
			}
			if props["enable-rcon"] != "true" || props["server-port"] != strconv.Itoa(i.Port) || props["rcon.port"] != strconv.Itoa(i.RCONPort) || fabric.ValidateProperty("rcon.password", props["rcon.password"]) != nil {
				return nil, errors.New("non-destructive adoption requires matching configured game/RCON ports and an enabled strong RCON credential; no original files were changed")
			}
			i.RCONSecret = props["rcon.password"]
			eula, err := files.ReadText(ctx, root, "eula.txt", 1<<20)
			if err != nil {
				return nil, err
			}
			eulaProps, err := fabric.ReadProperties([]byte(eula))
			if err != nil || eulaProps["eula"] != "true" {
				return nil, errors.New("existing instance must already record EULA acceptance; adoption never rewrites it")
			}
			launcher, err := files.OpenRegular(root, i.Jar)
			if err != nil {
				return nil, errors.New("selected adopted launcher is missing or unsafe")
			}
			launcher.Close()
			if err := a.Instances.Validate(i); err != nil {
				return nil, err
			}
		}
		if err := t.Progress(ctx, 65, "Staging verified; provisioning the restricted instance service"); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		if i.Adopted {
			if err := a.bindAdoption(ctx, body, i); err != nil {
				return nil, err
			}
		}
		if err := a.provision(ctx, i, false); err != nil {
			return nil, err
		}
		if !i.Adopted {
			root, err := a.Instances.OpenRoot(i.Root)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			stagedManifest, err := treeManifest(ctx, stageRoot, "")
			if err != nil {
				return nil, err
			}
			if err := ensureSpace(root, stagedManifest.Bytes); err != nil {
				return nil, err
			}
			if body.Mode == "create" {
				launcher, err := files.OpenRegular(stageRoot, "launcher.jar")
				if err != nil {
					return nil, err
				}
				if err := root.MkdirAll(filepath.Dir(i.Jar), 0770); err != nil {
					launcher.Close()
					return nil, err
				}
				out, err := root.OpenFile(i.Jar, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0660)
				if err != nil {
					launcher.Close()
					return nil, err
				}
				_, err = files.Copy(ctx, out, launcher, fabric.MaxLauncherBytes, nil)
				err = errors.Join(err, out.Sync(), out.Close(), launcher.Close())
				if err != nil {
					return nil, err
				}
				props, err := files.ReadText(ctx, stageRoot, "server.properties", 1<<20)
				if err != nil {
					return nil, err
				}
				if err := files.WriteText(ctx, root, "server.properties", props, 1<<20); err != nil {
					return nil, err
				}
			} else {
				if err := copyTree(ctx, stageRoot, root, false); err != nil {
					return nil, err
				}
			}
			if err := files.WriteText(ctx, root, "eula.txt", "# Accepted explicitly by the administrator through New Empire Server Control\neula=true\n", 1024); err != nil {
				return nil, err
			}
		}
		if err := a.Instances.Save(ctx, i); err != nil {
			return nil, err
		}
		if err := a.audit(ctx, body.Owner, "instance.register", i.ID, "Registered "+body.Mode+" instance; Minecraft remains stopped"); err != nil {
			return nil, err
		}
		return map[string]any{"instance": i, "state": "offline", "java_major": java.Major, "warnings": runtimePlan.Warnings}, nil
	})
}

func etagJSON(value any) string { b, _ := json.Marshal(value); return etag(string(b)) }
