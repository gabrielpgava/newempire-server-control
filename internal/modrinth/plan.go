package modrinth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

type Request struct {
	Target   Target            `json:"target"`
	Projects []string          `json:"projects,omitempty"`
	Remove   []string          `json:"remove,omitempty"`
	Versions map[string]string `json:"versions"`
	Policy   *Policy           `json:"policy,omitempty"`
}

type Change struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"project_id"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Rationale string `json:"rationale"`
}

// ConfigFile is a new config-only override. Existing local configuration always wins.
type ConfigFile struct {
	Path   string `json:"path"`
	SHA512 string `json:"sha512"`
	Size   int64  `json:"size"`
}

type Plan struct {
	ID        string        `json:"id"`
	Target    Target        `json:"target"`
	Before    []FileState   `json:"before"`
	HadMods   bool          `json:"had_mods"`
	LockHash  string        `json:"lock_hash,omitempty"`
	Files     []LockedFile  `json:"files"`
	Changes   []Change      `json:"changes"`
	Policy    Policy        `json:"policy"`
	Rationale []string      `json:"rationale"`
	Blockers  []string      `json:"blockers"`
	Config    []ConfigFile  `json:"config,omitempty"`
	Pack      *PackMetadata `json:"pack,omitempty"`
}

func (s *Service) Plan(ctx context.Context, root *os.Root, req Request) (*Plan, error) {
	plan := &Plan{Target: req.Target, Files: []LockedFile{}, Changes: []Change{}, Rationale: []string{}, Blockers: []string{}}
	defer plan.Seal()
	if plan.Target.Loader == "" {
		plan.Target.Loader = "fabric"
	}
	if err := plan.Target.Validate(); err != nil {
		plan.Blockers = append(plan.Blockers, err.Error())
		return plan, nil
	}
	if len(req.Projects)+len(req.Versions)+len(req.Remove) > MaxProjects {
		plan.Blockers = append(plan.Blockers, "content request exceeds the project limit")
		return plan, nil
	}
	inv, err := s.Inventory(ctx, root)
	if err != nil {
		return nil, err
	}
	plan.Before = inv.before
	plan.HadMods = inv.hadMods
	plan.Pack = inv.Lock.Pack
	lockData, err := readOptional(root, LockFilename, 4<<20)
	if err != nil {
		return nil, err
	}
	if lockData != nil {
		plan.LockHash = hashBytes(lockData)
	}
	policy := inv.Lock.Policy
	if req.Policy != nil {
		policy = *req.Policy
	}
	plan.Policy, err = policy.normalized()
	if err != nil {
		plan.Blockers = append(plan.Blockers, err.Error())
		return plan, nil
	}
	existing := map[string]LockedFile{}
	unmanaged := map[string]bool{}
	for _, local := range inv.Files {
		if !local.Managed {
			plan.Rationale = append(plan.Rationale, "Preserved unmanaged JAR "+local.Filename+": "+local.Diagnostic)
			if plan.Policy.Mode == "automatic" && local.ProjectID == "" {
				plan.Blockers = append(plan.Blockers, "Automatic updates cannot verify compatibility with unknown JAR: "+local.Filename)
			}
			if local.ProjectID != "" {
				unmanaged[local.ProjectID] = true
			}
			continue
		}
		for _, locked := range inv.Lock.Files {
			if locked.Filename == local.Filename {
				existing[locked.ProjectID] = locked
			}
		}
	}
	for _, f := range inv.Lock.Files {
		if _, ok := existing[f.ProjectID]; !ok {
			plan.Blockers = append(plan.Blockers, "Locked file is missing, modified, or no longer identifiable: "+f.Filename+"; reconcile explicitly before updating.")
		}
	}
	removed := map[string]bool{}
	requested := map[string]string{}
	projectCache := map[string]Project{}
	canonical := func(id string) (string, error) {
		if p, ok := projectCache[id]; ok {
			return p.ID, nil
		}
		p, err := s.Client.Project(ctx, id)
		if err != nil {
			return "", err
		}
		projectCache[p.ID] = p
		return p.ID, nil
	}
	policyIDs := append(sortedKeys(plan.Policy.Pins), plan.Policy.Excludes...)
	slices.Sort(policyIDs)
	policyIDs = slices.Compact(policyIDs)
	for _, id := range policyIDs {
		resolved, err := canonical(id)
		if err != nil {
			return planFailure(ctx, plan, err)
		}
		if resolved != id {
			plan.Blockers = append(plan.Blockers, "Pins/excludes must use canonical Modrinth project IDs, not slugs: "+id)
		}
	}
	for _, id := range req.Remove {
		id, err = canonical(id)
		if err != nil {
			return planFailure(ctx, plan, err)
		}
		if _, ok := existing[id]; !ok {
			plan.Blockers = append(plan.Blockers, "Cannot remove an unmanaged or absent project: "+id)
		}
		if plan.Policy.Pins[id] != "" || slices.Contains(plan.Policy.Excludes, id) {
			plan.Blockers = append(plan.Blockers, "Pinned/excluded project cannot be removed: "+id)
		}
		removed[id] = true
	}
	for _, id := range req.Projects {
		id, err = canonical(id)
		if err != nil {
			return planFailure(ctx, plan, err)
		}
		requested[id] = ""
	}
	for _, key := range sortedKeys(req.Versions) {
		id, version := key, req.Versions[key]
		id, err = canonical(id)
		if err != nil {
			return planFailure(ctx, plan, err)
		}
		if !validID(version) {
			plan.Blockers = append(plan.Blockers, "Invalid exact version for project "+id)
			continue
		}
		if previous := requested[id]; previous != "" && previous != version {
			plan.Blockers = append(plan.Blockers, "Conflicting exact version requests for project "+id)
			continue
		}
		requested[id] = version
	}
	roots := map[string]string{}
	for id, f := range existing {
		if removed[id] {
			continue
		}
		_, update := requested[id]
		if len(req.Projects) == 0 && req.Versions == nil && len(req.Remove) == 0 {
			update = true
		}
		if update && !slices.Contains(plan.Policy.Excludes, id) {
			roots[id] = ""
		} else {
			roots[id] = f.VersionID
		}
	}
	for id, version := range requested {
		if removed[id] {
			plan.Blockers = append(plan.Blockers, "Project cannot be installed and removed in one plan: "+id)
			continue
		}
		if slices.Contains(plan.Policy.Excludes, id) {
			plan.Blockers = append(plan.Blockers, "Project is excluded by policy: "+id)
			continue
		}
		roots[id] = version
	}
	resolver := solver{client: s.Client, target: plan.Target, policy: plan.Policy, projects: projectCache, versions: map[string]Version{}, constraints: map[string]string{}, hints: map[string]string{}, forbidden: removed, unmanaged: unmanaged}
	for id, exact := range roots {
		if exact != "" {
			resolver.constraints[id] = exact
		}
	}
	for _, id := range sortedKeys(plan.Policy.Pins) {
		version := plan.Policy.Pins[id]
		if exact := resolver.constraints[id]; exact != "" && exact != version {
			plan.Blockers = append(plan.Blockers, "Version pin conflicts with retained/requested version for project "+id)
		}
		resolver.constraints[id] = version
	}
	if len(plan.Blockers) > 0 {
		return plan, nil
	}
	ids := sortedKeys(roots)
	for round := 0; ; round++ {
		if round == 16 {
			return planFailure(ctx, plan, errors.New("dependency constraints did not converge; choose an explicit compatible version set"))
		}
		resolver.selected = map[string]Version{}
		resolver.visiting = map[string]bool{}
		resolver.reasons = map[string]string{}
		resolver.required = map[string]string{}
		var resolveErr error
		for _, id := range ids {
			if resolveErr = resolver.visit(ctx, id, "", "Requested or retained server content.", 0); resolveErr != nil {
				break
			}
		}
		if errors.Is(resolveErr, errReselect) {
			continue
		}
		if resolveErr != nil {
			return planFailure(ctx, plan, resolveErr)
		}
		break
	}
	if err := resolver.checkRelations(ctx); err != nil {
		return planFailure(ctx, plan, err)
	}
	for _, local := range inv.Files {
		if !local.Managed && local.ProjectID != "" {
			if err := resolver.checkExternal(ctx, local.ProjectID, local.VersionID); err != nil {
				return planFailure(ctx, plan, err)
			}
		}
	}
	for _, id := range sortedKeys(resolver.selected) {
		v := resolver.selected[id]
		f, err := PrimaryFile(v)
		if err != nil {
			return planFailure(ctx, plan, fmt.Errorf("project %s: %w", id, err))
		}
		locked := LockedFile{Filename: f.Filename, SHA512: f.Hashes["sha512"], SHA1: f.Hashes["sha1"], Size: f.Size, ProjectID: id, VersionID: v.ID, URL: f.URL, Environment: v.Environment}
		plan.Files = append(plan.Files, locked)
		reason := resolver.reasons[id]
		plan.Rationale = append(plan.Rationale, id+": "+reason)
		old, ok := existing[id]
		if ok && old.SHA512 == locked.SHA512 && old.Filename == locked.Filename {
			continue
		}
		change := Change{Kind: "install", ProjectID: id, To: locked.Filename, Rationale: reason}
		if ok {
			change.Kind = "update"
			change.From = old.Filename
		}
		plan.Changes = append(plan.Changes, change)
	}
	for _, id := range sortedKeys(existing) {
		if removed[id] {
			plan.Changes = append(plan.Changes, Change{Kind: "remove", ProjectID: id, From: existing[id].Filename, Rationale: "Explicit administrator removal; unknown files remain untouched."})
		}
	}
	if err := validateLocked(plan.Files); err != nil {
		plan.Blockers = append(plan.Blockers, err.Error())
	}
	for _, file := range plan.Files {
		if info, err := root.Lstat("mods/" + file.Filename); err == nil && !info.Mode().IsRegular() {
			plan.Blockers = append(plan.Blockers, "Destination is not a regular managed file: "+file.Filename)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, local := range inv.Files {
			if strings.EqualFold(file.Filename, local.Filename) && (!local.Managed || local.ProjectID != file.ProjectID) {
				plan.Blockers = append(plan.Blockers, "Destination would overwrite another or unmanaged file: "+file.Filename)
			}
		}
	}
	return plan, nil
}

// Seal gives identical plans identical IDs. Call it after attaching validated pack overrides.
func (p *Plan) Seal() {
	p.ID = ""
	data, _ := json.Marshal(p)
	p.ID = hashBytes(data)[:32]
}

func planFailure(ctx context.Context, plan *Plan, err error) (*Plan, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status != 404 {
		return nil, err
	}
	plan.Blockers = append(plan.Blockers, err.Error())
	return plan, nil
}

func sortedKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type solver struct {
	client      *Client
	target      Target
	policy      Policy
	projects    map[string]Project
	versions    map[string]Version
	selected    map[string]Version
	visiting    map[string]bool
	constraints map[string]string
	hints       map[string]string
	required    map[string]string
	forbidden   map[string]bool
	unmanaged   map[string]bool
	reasons     map[string]string
}

var errReselect = errors.New("dependency requires reselection")

func (s *solver) project(ctx context.Context, id string) (Project, error) {
	if p, ok := s.projects[id]; ok {
		return p, nil
	}
	p, err := s.client.Project(ctx, id)
	if err == nil {
		if p.ID != id {
			return Project{}, errors.New("dependency project identity mismatch")
		}
		s.projects[id] = p
	}
	return p, err
}
func (s *solver) version(ctx context.Context, id string) (Version, error) {
	if v, ok := s.versions[id]; ok {
		return v, nil
	}
	v, err := s.client.Version(ctx, id)
	if err == nil {
		s.versions[id] = v
	}
	return v, err
}

func (s *solver) visit(ctx context.Context, project, exact, reason string, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return errors.New("dependency graph exceeds the safe resolution limit")
	}
	if !validID(project) {
		return errors.New("required dependency does not identify a known project")
	}
	if s.forbidden[project] {
		return fmt.Errorf("required dependency conflicts with removal of %s", project)
	}
	if s.unmanaged[project] {
		return fmt.Errorf("project %s is externally managed; explicit reconciliation is required", project)
	}
	if slices.Contains(s.policy.Excludes, project) && s.constraints[project] == "" {
		return fmt.Errorf("required dependency %s is excluded by policy", project)
	}
	if exact != "" {
		if required := s.required[project]; required != "" && required != exact {
			return fmt.Errorf("conflicting required dependency versions for %s: %s versus %s", project, required, exact)
		}
		s.required[project] = exact
	}
	if pin := s.constraints[project]; pin != "" {
		if exact != "" && exact != pin {
			return fmt.Errorf("dependency/version pin conflict for %s: %s versus %s", project, exact, pin)
		}
		exact = pin
	}
	if s.visiting[project] {
		return fmt.Errorf("required dependency cycle at project %s", project)
	}
	if v, ok := s.selected[project]; ok {
		if exact != "" && v.ID != exact {
			s.hints[project] = exact
			return errReselect
		}
		return nil
	}
	if len(s.selected) >= MaxProjects {
		return errors.New("dependency graph exceeds the safe resolution limit")
	}
	if exact == "" {
		exact = s.hints[project]
	}
	p, err := s.project(ctx, project)
	if err != nil {
		return err
	}
	var v Version
	if exact != "" {
		v, err = s.version(ctx, exact)
	} else {
		var candidates []Version
		candidates, err = s.client.Versions(ctx, project, s.target, s.policy.AllowPrerelease)
		if err == nil {
			for _, candidate := range candidates {
				if Compatible(p, candidate, s.target, s.policy.AllowPrerelease) == nil {
					v = candidate
					break
				}
			}
			if v.ID == "" {
				err = fmt.Errorf("project %s has no compatible %s server release", project, s.target.MinecraftVersion)
			}
		}
	}
	if err != nil {
		return err
	}
	if err := Compatible(p, v, s.target, s.policy.AllowPrerelease); err != nil {
		return err
	}
	if _, err := PrimaryFile(v); err != nil {
		return fmt.Errorf("project %s: %w", project, err)
	}
	s.selected[project] = v
	s.visiting[project] = true
	if exact != "" {
		reason += " Exact version " + v.ID + " (dependency, retained version, or pin)."
	} else {
		reason += " Newest compatible permitted server release " + v.ID + "."
	}
	s.reasons[project] = reason
	for _, dep := range v.Dependencies {
		switch dep.Type {
		case "required":
			id, version, err := s.dependency(ctx, dep)
			if err != nil {
				return err
			}
			if err := s.visit(ctx, id, version, "Required by "+project+" / "+v.ID+".", depth+1); err != nil {
				return err
			}
		case "optional", "incompatible", "embedded":
		default:
			return fmt.Errorf("unknown dependency type %q on project %s", dep.Type, project)
		}
	}
	s.visiting[project] = false
	return nil
}

func (s *solver) dependency(ctx context.Context, dep Dependency) (string, string, error) {
	project := dep.ProjectID
	if dep.VersionID != "" {
		v, err := s.version(ctx, dep.VersionID)
		if err != nil {
			return "", "", err
		}
		if project != "" && project != v.ProjectID {
			return "", "", errors.New("dependency project/version identity mismatch")
		}
		project = v.ProjectID
	}
	if !validID(project) {
		return "", "", errors.New("file-only or unidentified required dependencies cannot be verified")
	}
	return project, dep.VersionID, nil
}

func (s *solver) checkRelations(ctx context.Context) error {
	for _, id := range sortedKeys(s.selected) {
		for _, dep := range s.selected[id].Dependencies {
			if dep.Type != "incompatible" && dep.Type != "optional" {
				continue
			}
			project, exact, err := s.dependency(ctx, dep)
			if err != nil {
				return err
			}
			v, present := s.selected[project]
			if dep.Type == "incompatible" && present && (exact == "" || exact == v.ID) {
				return fmt.Errorf("incompatible projects/versions: %s and %s / %s", id, project, v.ID)
			}
			if dep.Type == "optional" && present && exact != "" && exact != v.ID {
				return fmt.Errorf("optional dependency version conflict: %s requires %s / %s when installed", id, project, exact)
			}
		}
	}
	return nil
}

func (s *solver) checkExternal(ctx context.Context, project, version string) error {
	for id, selected := range s.selected {
		for _, dep := range selected.Dependencies {
			if dep.Type == "incompatible" {
				p, v, err := s.dependency(ctx, dep)
				if err != nil {
					return err
				}
				if p == project && (v == "" || v == version) {
					return fmt.Errorf("project %s is incompatible with unmanaged project %s", id, project)
				}
			}
		}
	}
	v, err := s.version(ctx, version)
	if err != nil {
		return err
	}
	for _, dep := range v.Dependencies {
		if dep.Type != "required" && dep.Type != "incompatible" {
			continue
		}
		p, exact, err := s.dependency(ctx, dep)
		if err != nil {
			return err
		}
		selected, present := s.selected[p]
		if dep.Type == "incompatible" && present && (exact == "" || exact == selected.ID) {
			return fmt.Errorf("unmanaged project %s is incompatible with %s", project, p)
		}
		if dep.Type == "required" && s.forbidden[p] {
			return fmt.Errorf("unmanaged project %s requires removed project %s", project, p)
		}
		if dep.Type == "required" && present && exact != "" && exact != selected.ID {
			return fmt.Errorf("unmanaged project %s requires exact dependency %s / %s", project, p, exact)
		}
	}
	return nil
}
