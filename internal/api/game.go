package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/auth"
	"github.com/gabrielpgava/newempire-server-control/internal/fabric"
	"github.com/gabrielpgava/newempire-server-control/internal/files"
	"github.com/gabrielpgava/newempire-server-control/internal/jobs"
)

var editableProperties = map[string]bool{"difficulty": true, "gamemode": true, "pvp": true, "white-list": true, "enforce-whitelist": true, "view-distance": true, "simulation-distance": true, "max-players": true, "level-seed": true, "online-mode": true, "motd": true, "allow-flight": true, "spawn-protection": true, "enable-command-block": true, "generate-structures": true, "level-type": true, "max-tick-time": true, "op-permission-level": true}

func safeProperties(values map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		if editableProperties[key] {
			out[key] = value
		}
	}
	return out
}
func readProperties(ctx context.Context, root *os.Root) (string, map[string]string, error) {
	raw, err := readOptionalText(ctx, root, "server.properties")
	if err != nil {
		return "", nil, err
	}
	values, err := fabric.ReadProperties([]byte(raw))
	return raw, values, err
}
func (a *App) properties(w http.ResponseWriter, r *http.Request) {
	root, err := a.Instances.OpenRoot(instance(r).Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	_, values, err := readProperties(r.Context(), root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	var dirty bool
	if err := a.setting(r.Context(), "properties-dirty:"+instance(r).ID, &dirty); err != nil {
		fail(w, 500, err)
		return
	}
	respond(w, 200, map[string]any{"properties": safeProperties(values), "restart_required": dirty})
}

type propertyRequest struct {
	Properties map[string]string `json:"properties"`
	Confirm    bool              `json:"confirm"`
}

func validateSetting(key, value string) error {
	if !editableProperties[key] {
		return errors.New("this property is protected or not supported by the settings editor")
	}
	if err := fabric.ValidateProperty(key, value); err != nil {
		return err
	}
	ranges := map[string][2]int{"view-distance": {2, 64}, "simulation-distance": {2, 32}, "spawn-protection": {0, 1000}, "max-tick-time": {1000, 600000}, "op-permission-level": {1, 4}}
	if bounds, ok := ranges[key]; ok {
		n, err := strconv.Atoi(value)
		if err != nil || n < bounds[0] || n > bounds[1] {
			return fmt.Errorf("%s must be between %d and %d", key, bounds[0], bounds[1])
		}
	}
	return nil
}
func (a *App) saveProperties(w http.ResponseWriter, r *http.Request) {
	var body propertyRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || len(body.Properties) == 0 {
		fail(w, 400, errors.New("property changes require explicit confirmation"))
		return
	}
	for key, value := range body.Properties {
		if err := validateSetting(key, value); err != nil {
			fail(w, 400, err)
			return
		}
	}
	a.enqueue(w, r, instance(r).ID, "properties", body)
}

var worldNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func activeWorld(values map[string]string) string {
	if values["level-name"] == "" {
		return "world"
	}
	return values["level-name"]
}

type worldInfo struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Active     bool     `json:"active"`
	Dimensions []string `json:"dimensions"`
}

func (a *App) worlds(w http.ResponseWriter, r *http.Request) {
	i := instance(r)
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	_, props, err := readProperties(r.Context(), root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	active := activeWorld(props)
	entries, err := files.List(r.Context(), root, ".")
	if err != nil {
		fail(w, 400, err)
		return
	}
	known := []string{}
	if err := a.setting(r.Context(), "worlds:"+i.ID, &known); err != nil {
		fail(w, 500, err)
		return
	}
	all := []worldInfo{}
	for _, entry := range entries {
		if !entry.Directory || !worldNamePattern.MatchString(entry.Name) {
			continue
		}
		_, hasLevel := root.Lstat(entry.Name + "/level.dat")
		if hasLevel != nil && entry.Name != active && !slices.Contains(known, entry.Name) {
			continue
		}
		world := worldInfo{Name: entry.Name, Path: entry.Path, Active: entry.Name == active, Dimensions: []string{}}
		for _, dimension := range []string{"region", "DIM-1", "DIM1"} {
			if info, err := files.Check(root, entry.Path+"/"+dimension); err == nil && info.IsDir() {
				name := dimension
				if dimension == "region" {
					name = "minecraft:overworld"
				}
				if dimension == "DIM-1" {
					name = "minecraft:the_nether"
				}
				if dimension == "DIM1" {
					name = "minecraft:the_end"
				}
				world.Dimensions = append(world.Dimensions, name)
			}
		}
		if namespaces, err := files.List(r.Context(), root, entry.Path+"/dimensions"); err == nil {
			for _, namespace := range namespaces {
				if !namespace.Directory {
					continue
				}
				dimensions, err := files.List(r.Context(), root, namespace.Path)
				if err != nil {
					continue
				}
				for _, dimension := range dimensions {
					if dimension.Directory {
						world.Dimensions = append(world.Dimensions, namespace.Name+":"+dimension.Name)
					}
				}
			}
		}
		all = append(all, world)
	}
	respond(w, 200, map[string]any{"active": active, "worlds": all})
}

type worldRequest struct {
	Operation string `json:"operation"`
	Name      string `json:"name"`
	Confirm   bool   `json:"confirm"`
}

func (a *App) mutateWorlds(w http.ResponseWriter, r *http.Request) {
	var body worldRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if !body.Confirm || !worldNamePattern.MatchString(body.Name) || body.Operation != "create" && body.Operation != "activate" {
		fail(w, 400, errors.New("choose create or activate, a safe world name, and explicit confirmation"))
		return
	}
	a.enqueue(w, r, instance(r).ID, "world", body)
}

func readJSONList(ctx context.Context, root *os.Root, name string) ([]map[string]any, error) {
	text, err := readOptionalText(ctx, root, name)
	if err != nil {
		return nil, err
	}
	all := []map[string]any{}
	if text == "" {
		return all, nil
	}
	if err := json.Unmarshal([]byte(text), &all); err != nil {
		return nil, errors.New("Minecraft access-list file is malformed; inspect it before editing")
	}
	if all == nil {
		all = []map[string]any{}
	}
	return all, nil
}
func (a *App) players(w http.ResponseWriter, r *http.Request) {
	i := instance(r)
	status, err := a.runtimeStatus(r.Context(), i)
	if err != nil {
		fail(w, 503, err)
		return
	}
	root, err := a.Instances.OpenRoot(i.Root)
	if err != nil {
		fail(w, 400, err)
		return
	}
	defer root.Close()
	out := map[string]any{"online": status.Players}
	for key, name := range map[string]string{"whitelist": "whitelist.json", "operators": "ops.json", "bans": "banned-players.json", "ip_bans": "banned-ips.json"} {
		if key == "ip_bans" && !auth.Permitted(session(r).User.Role, "admin") {
			continue
		}
		values, err := readJSONList(r.Context(), root, name)
		if err != nil {
			fail(w, 400, err)
			return
		}
		out[key] = values
	}
	respond(w, 200, out)
}

type playerRequest struct {
	Action string `json:"action"`
	Player string `json:"player"`
	Value  string `json:"value"`
}

var playerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,16}$`)

func playerCommand(body playerRequest, admin bool) (string, error) {
	if len(body.Value) > 500 || strings.ContainsAny(body.Value, "\r\n\x00") {
		return "", errors.New("player message is too long or contains a control character")
	}
	if body.Action == "say" {
		if strings.TrimSpace(body.Value) == "" {
			return "", errors.New("broadcast message is empty")
		}
		return "say " + body.Value, nil
	}
	if body.Action == "op" || body.Action == "deop" || body.Action == "op_level" || body.Action == "ban_ip" || body.Action == "pardon_ip" {
		if !admin {
			return "", errors.New("operator privileges and IP moderation require administrator permission")
		}
	}
	if body.Action == "ban_ip" || body.Action == "pardon_ip" {
		if net.ParseIP(body.Player) == nil {
			return "", errors.New("enter a numeric IP address")
		}
	} else if !playerNamePattern.MatchString(body.Player) {
		return "", errors.New("enter a valid Minecraft player name")
	}
	verbs := map[string]string{"whitelist_add": "whitelist add", "whitelist_remove": "whitelist remove", "op": "op", "deop": "deop", "ban": "ban", "pardon": "pardon", "kick": "kick", "ban_ip": "ban-ip", "pardon_ip": "pardon-ip"}
	if body.Action == "op_level" {
		level, err := strconv.Atoi(body.Value)
		if err != nil || level < 1 || level > 4 {
			return "", errors.New("operator level must be between 1 and 4")
		}
		return "", nil
	}
	verb, ok := verbs[body.Action]
	if !ok {
		return "", errors.New("unsupported player action")
	}
	command := verb + " " + body.Player
	if body.Value != "" && (body.Action == "ban" || body.Action == "ban_ip" || body.Action == "kick") {
		command += " " + body.Value
	}
	return command, nil
}
func (a *App) playerAction(w http.ResponseWriter, r *http.Request) {
	var body playerRequest
	if err := decode(r, &body); err != nil {
		fail(w, 400, err)
		return
	}
	if _, err := playerCommand(body, auth.Permitted(session(r).User.Role, "admin")); err != nil {
		fail(w, 400, err)
		return
	}
	a.enqueue(w, r, instance(r).ID, "player", body)
}

func (a *App) registerGameJobs() {
	a.Jobs.Register("properties", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body propertyRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := contentSettled(root); err != nil {
			return nil, err
		}
		old, props, err := readProperties(ctx, root)
		if err != nil {
			return nil, err
		}
		for key, value := range body.Properties {
			if err := validateSetting(key, value); err != nil {
				return nil, err
			}
			if key == "level-seed" && props[key] != value {
				if _, err := root.Lstat(path.Join(activeWorld(props), "level.dat")); err == nil {
					return nil, errors.New("changing the seed does not regenerate an existing world; create a new world explicitly")
				}
			}
		}
		updated, err := fabric.UpdateProperties([]byte(old), body.Properties)
		if err != nil {
			return nil, err
		}
		if _, err := a.safetyBackup(ctx, t, i, "Before Minecraft property changes"); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		if err := files.WriteText(ctx, root, "server.properties", string(updated), 1<<20); err != nil {
			return nil, err
		}
		if err := a.saveSetting(ctx, "properties-dirty:"+i.ID, true); err != nil {
			return nil, err
		}
		return map[string]bool{"restart_required": true}, nil
	})
	a.Jobs.Register("world", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body worldRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if err := a.assertStopped(ctx, i); err != nil {
			return nil, err
		}
		root, err := a.Instances.OpenRoot(i.Root)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		if err := contentSettled(root); err != nil {
			return nil, err
		}
		old, _, err := readProperties(ctx, root)
		if err != nil {
			return nil, err
		}
		if _, err := a.safetyBackup(ctx, t, i, "Before world "+body.Operation); err != nil {
			return nil, err
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		switch body.Operation {
		case "create":
			if err := files.Mkdir(ctx, root, body.Name); err != nil {
				return nil, err
			}
			known := []string{}
			if err := a.setting(ctx, "worlds:"+i.ID, &known); err != nil {
				return nil, err
			}
			known = append(known, body.Name)
			if err := a.saveSetting(ctx, "worlds:"+i.ID, known); err != nil {
				return nil, err
			}
			return map[string]string{"world": body.Name, "note": "An empty world directory was created. Activate it, then start Minecraft to generate world data."}, nil
		case "activate":
			info, err := files.Check(root, body.Name)
			if err != nil || !info.IsDir() {
				return nil, errors.New("world directory not found")
			}
			updated, err := fabric.UpdateProperties([]byte(old), map[string]string{"level-name": body.Name})
			if err != nil {
				return nil, err
			}
			if err := files.WriteText(ctx, root, "server.properties", string(updated), 1<<20); err != nil {
				return nil, err
			}
			return map[string]any{"active": body.Name, "restart_required": true}, nil
		}
		return nil, errors.New("unsupported world operation")
	})
	a.Jobs.Register("player", 0, func(ctx context.Context, t *jobs.Task, raw json.RawMessage) (any, error) {
		var body playerRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		command, err := playerCommand(body, true)
		if err != nil {
			return nil, err
		}
		i, err := a.Instances.Get(ctx, t.Job.InstanceID)
		if err != nil {
			return nil, err
		}
		if body.Action == "op_level" {
			if err := a.assertStopped(ctx, i); err != nil {
				return nil, err
			}
			root, err := a.Instances.OpenRoot(i.Root)
			if err != nil {
				return nil, err
			}
			defer root.Close()
			if err := contentSettled(root); err != nil {
				return nil, err
			}
			records, err := readJSONList(ctx, root, "ops.json")
			if err != nil {
				return nil, err
			}
			found := false
			level, _ := strconv.Atoi(body.Value)
			for _, record := range records {
				if name, _ := record["name"].(string); strings.EqualFold(name, body.Player) {
					record["level"] = level
					found = true
				}
			}
			if !found {
				return nil, errors.New("operator record not found; grant operator access through Minecraft first")
			}
			if _, err := a.safetyBackup(ctx, t, i, "Before per-player operator-level change"); err != nil {
				return nil, err
			}
			b, _ := json.MarshalIndent(records, "", "  ")
			if err := t.CommitBarrier(ctx); err != nil {
				return nil, err
			}
			if err := files.WriteText(ctx, root, "ops.json", string(b), 1<<20); err != nil {
				return nil, err
			}
			return map[string]string{"output": "Operator level saved; restart Minecraft to apply it."}, nil
		}
		status, err := a.runtimeStatus(ctx, i)
		if err != nil {
			return nil, err
		}
		if !status.Ready {
			return nil, errors.New("player actions require a ready Minecraft server")
		}
		if err := t.CommitBarrier(ctx); err != nil {
			return nil, err
		}
		output, err := a.runtimeCommand(ctx, i, command)
		return map[string]string{"output": output}, err
	})
}
