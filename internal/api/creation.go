package api

import (
	"context"
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
	if len(strings.TrimSpace(body.Name)) == 0 || len(body.Name) > 100 || body.MemoryMB < 512 || body.MemoryMB > 1048576 || body.Port < 1024 || body.Port > 65535 || body.RCONPort < 1024 || body.RCONPort > 65535 || body.Port == body.RCONPort {
		fail(w, 400, errors.New("invalid instance name, memory, or port configuration"))
		return
	}
	if body.Mode != "create" && body.PreviewHash == "" {
		fail(w, 400, errors.New("inspect the import/adoption and confirm its preview first"))
		return
	}
	if body.Jar == "" {
		if body.Mode != "create" {
			fail(w, 400, errors.New("choose the exact launcher path from the import report"))
			return
		}
		body.Jar = "fabric-server-launch.jar"
	}
	if err := files.ValidatePath(body.Jar); err != nil || body.Jar == "." || !strings.HasSuffix(body.Jar, ".jar") {
		fail(w, 400, errors.New("launcher must be a safe relative JAR path"))
		return
	}
	body.Owner = session(r).User.ID
	id := model.NewID()
	a.enqueue(w, r, id, "instance_create", body)
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
	if err := a.checkRestorePending(ctx, i.ID); err != nil {
		return err
	}
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
		i := model.Instance{ID: t.Job.InstanceID, Name: body.Name, JavaPath: body.JavaPath, Jar: body.Jar, MinecraftVersion: body.MinecraftVersion, LoaderVersion: body.LoaderVersion, MemoryMB: body.MemoryMB, Port: body.Port, RCONPort: body.RCONPort, EULA: body.EULA, Adopted: body.Mode == "adopt", CreatedAt: time.Now().UTC(), RCONSecret: auth.Token()}
		i.Root = filepath.Join(a.Config.InstanceDir, i.ID)
		if i.Adopted {
			i.Root = filepath.Clean(body.Path)
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
			if p.Hash != body.PreviewHash {
				return nil, errors.New("import preview changed; inspect the upload again")
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
			if manifestHash(m) != body.PreviewHash {
				return nil, errors.New("adoption preview changed; stop the existing server and inspect again")
			}
			propsRaw, err := files.ReadText(ctx, root, "server.properties", 1<<20)
			if err != nil {
				return nil, err
			}
			props, err := fabric.ReadProperties([]byte(propsRaw))
			if err != nil {
				return nil, err
			}
			if props["enable-rcon"] != "true" || props["server-port"] != strconv.Itoa(i.Port) || props["rcon.port"] != strconv.Itoa(i.RCONPort) || len(props["rcon.password"]) < 16 {
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
		}
		if err := t.Progress(ctx, 65, "Staging verified; provisioning the restricted instance service"); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
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
