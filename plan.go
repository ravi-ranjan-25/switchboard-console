package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

const (
	ActCreate = "create"
	ActUpdate = "update"
	ActDelete = "delete"
)

// Change is one file commit. Update changes carry Rebuild, a pure function of the file content, so that the
// edit can be re-applied to the freshest content when the file moved on between preview and commit.
type Change struct {
	Path    string
	Action  string
	Old     []byte // content in the snapshot the plan was made from
	New     []byte
	Rebuild func(old []byte) ([]byte, error)
}

// Plan is an ordered list of single-file commits. The order is chosen so that every intermediate state is
// harmless (see README, "Multi-file changes").
type Plan struct {
	Kind    string // new-app | delete-app | replicas | image | domain
	App     string
	Message string
	Changes []Change
	// Source, when set, is a new private repo created (and filled) BEFORE the first citadel commit.
	Source *SourcePlan
}

// Hash identifies exactly what a preview showed; commit refuses if the re-planned changes differ.
func (p *Plan) Hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s\n", p.Kind, p.App, p.Message)
	for _, c := range p.Changes {
		fmt.Fprintf(h, "%s|%s|%d\n", c.Action, c.Path, len(c.New))
		h.Write(c.New)
		h.Write([]byte{0})
	}
	if sp := p.Source; sp != nil {
		fmt.Fprintf(h, "source|%s|%s|%s|%s\n", sp.Owner, sp.Repo, sp.Stack, sp.Description)
		for _, f := range sp.Files {
			fmt.Fprintf(h, "%s|%d\n", f.Path, len(f.Content))
			h.Write([]byte(f.Content))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// After returns a copy of the snapshot with the plan applied (used to validate the end state).
func (p *Plan) After(s *Snapshot) *Snapshot {
	out := &Snapshot{Paths: map[string]string{}, Files: map[string][]byte{}}
	for k, v := range s.Paths {
		out.Paths[k] = v
	}
	for k, v := range s.Files {
		out.Files[k] = v
	}
	for _, c := range p.Changes {
		if c.Action == ActDelete {
			delete(out.Paths, c.Path)
			delete(out.Files, c.Path)
		} else {
			out.Paths[c.Path] = "planned"
			out.Files[c.Path] = c.New
		}
	}
	return out
}

func (p *Plan) paths() []string {
	var o []string
	for _, c := range p.Changes {
		o = append(o, c.Path)
	}
	return o
}

func newUpdate(s *Snapshot, p string, f func([]byte) ([]byte, error)) (Change, error) {
	old, ok := s.Files[p]
	if !ok {
		return Change{}, fmt.Errorf("%s not found in the repo", p)
	}
	nw, err := f(old)
	if err != nil {
		return Change{}, fmt.Errorf("%s: %w", p, err)
	}
	return Change{Path: p, Action: ActUpdate, Old: old, New: nw, Rebuild: f}, nil
}

func protectedApp(cfg *Config, a *AppInfo) bool {
	return cfg.Protected[a.Name] != "" || cfg.Protected[a.KName] != ""
}

func findApp(apps []*AppInfo, name string) *AppInfo {
	for _, a := range apps {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// PlanNewApp turns the form into the full file set. Field errors are returned separately from plan errors.
func PlanNewApp(s *Snapshot, cfg *Config, n *NewApp) (*Plan, []FieldError) {
	if errs := n.Normalize(cfg); len(errs) > 0 {
		return nil, errs
	}
	apps := loadApps(s, cfg.AppsDir)
	var errs []FieldError
	if findApp(apps, n.Name) != nil || s.HasDir("apps/"+n.Name) || s.Has(cfg.AppsDir+"/"+n.Name+".yaml") {
		errs = append(errs, FieldError{"name", "an app or directory with this name already exists"})
	}
	for _, a := range apps {
		if a.Spec.Host == n.Host {
			errs = append(errs, FieldError{"host", "already used by " + a.Name})
		}
	}
	if n.Built() {
		for _, d := range imageDocs(s, cfg) {
			if kind(d) == "ImageRepository" && (str(dig(d, "metadata", "name")) == n.Name || str(dig(d, "spec", "image")) == n.Image) {
				errs = append(errs, FieldError{"image", "an ImageRepository for this name or image already exists"})
				break
			}
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}

	files := n.Generate()
	var names []string
	for p := range files {
		names = append(names, p)
	}
	sort.Strings(names)
	p := &Plan{Kind: "new-app", App: n.Name, Message: "switchboard: add app " + n.Name}
	// Order: the app directory first (not referenced by anything yet), the Flux Kustomization next
	// (still unreferenced), then the shared files, and the registration LAST: only that commit makes Flux see it.
	for _, name := range names {
		if strings.HasPrefix(name, "apps/") {
			p.Changes = append(p.Changes, Change{Path: name, Action: ActCreate, New: []byte(files[name])})
		}
	}
	kf := cfg.AppsDir + "/" + n.Name + ".yaml"
	p.Changes = append(p.Changes, Change{Path: kf, Action: ActCreate, New: []byte(files[kf])})

	var firstErr error
	add := func(path string, f func([]byte) ([]byte, error)) {
		if c, err := newUpdate(s, path, f); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			p.Changes = append(p.Changes, c)
		}
	}
	if n.Database {
		ch, err := dbChanges(s, dbPlatformEdits(n.Name))
		if err != nil {
			return nil, []FieldError{{"database", err.Error()}}
		}
		p.Changes = append(p.Changes, ch...)
	}
	if n.Built() {
		add(cfg.AppsDir+"/image-automation.yaml", func(b []byte) ([]byte, error) { return AddImageAutomation(b, n) })
	}
	add("apps/homepage/configmap.yaml", func(b []byte) ([]byte, error) { return AddHomepage(b, n) })
	add(cfg.AppsDir+"/kustomization.yaml", func(b []byte) ([]byte, error) { return RegisterApp(b, n.Name, n.Namespace) })
	if firstErr != nil {
		return nil, []FieldError{{"", firstErr.Error()}}
	}
	if n.Source != nil {
		files, err := n.SourceFiles(cfg)
		if err != nil {
			return nil, []FieldError{{"source", err.Error()}}
		}
		p.Source = &SourcePlan{Owner: cfg.RepoOwner, Repo: n.Source.Repo, Description: n.Source.Description, Stack: n.Source.Stack, Files: files}
	}
	return p, nil
}

func imageDocs(s *Snapshot, cfg *Config) []Doc {
	ds, _ := decodeDocs(s.Files[cfg.AppsDir+"/image-automation.yaml"])
	return ds
}

// manageable returns the app when Switchboard may touch its workload.
func manageable(s *Snapshot, cfg *Config, name string) (*AppInfo, error) {
	a := findApp(loadApps(s, cfg.AppsDir), name)
	switch {
	case a == nil:
		return nil, errors.New("app not found")
	case protectedApp(cfg, a):
		return nil, fmt.Errorf("%s is protected; Switchboard will not change or remove it", name)
	case !a.Managed:
		return nil, fmt.Errorf("%s is not a standard app (needs ./apps/<dir> with one Deployment, one Service and an expose-* component)", name)
	}
	return a, nil
}

func PlanReplicas(s *Snapshot, cfg *Config, name string, replicas int) (*Plan, *AppInfo, error) {
	if replicas != 0 && replicas != 1 {
		return nil, nil, errors.New("replicas must be 0 or 1")
	}
	a, err := manageable(s, cfg, name)
	if err != nil {
		return nil, nil, err
	}
	var old int
	c, err := newUpdate(s, a.DeployPath, func(b []byte) ([]byte, error) {
		out, o, err := SetReplicas(b, replicas)
		old = o
		return out, err
	})
	if err != nil {
		return nil, nil, err
	}
	return &Plan{Kind: "replicas", App: name, Message: fmt.Sprintf("switchboard: %s replicas %d→%d", name, old, replicas), Changes: []Change{c}}, a, nil
}

// PlanImage: tag != "" sets the tag and pins (stops Flux auto-bumps); resume restores the Flux marker.
func PlanImage(s *Snapshot, cfg *Config, name, tag string, resume bool) (*Plan, *AppInfo, error) {
	a, err := manageable(s, cfg, name)
	if err != nil {
		return nil, nil, err
	}
	if !a.Built {
		return nil, nil, errors.New(name + " does not use a Flux-tracked build image; tag rollback is only for built apps")
	}
	how, msg := ImagePin, ""
	if resume {
		if !a.Pinned {
			return nil, nil, errors.New("auto-update is already active")
		}
		how = ImageResume
	} else if tag == a.Tag && a.Pinned {
		return nil, nil, errors.New("already pinned to that tag")
	}
	var old string
	c, err := newUpdate(s, a.DeployPath, func(b []byte) ([]byte, error) {
		out, o, err := EditImage(b, name, tag, how)
		old = o
		return out, err
	})
	if err != nil {
		return nil, nil, err
	}
	if resume {
		msg = fmt.Sprintf("switchboard: %s resume image auto-update (tag %s)", name, old)
	} else {
		msg = fmt.Sprintf("switchboard: %s image %s→%s (pinned)", name, old, tag)
	}
	return &Plan{Kind: "image", App: name, Message: msg, Changes: []Change{c}}, a, nil
}

// PlanDelete removes everything PlanNewApp created. Refuses protected apps and apps others depend on.
func PlanDelete(s *Snapshot, cfg *Config, name string) (*Plan, *AppInfo, error) {
	a, err := manageable(s, cfg, name)
	if err != nil {
		return nil, nil, err
	}
	apps := loadApps(s, cfg.AppsDir)
	for _, o := range apps {
		if o.Name == name {
			continue
		}
		if contains(o.DependsOn, a.KName) {
			return nil, nil, fmt.Errorf("%s depends on %s; remove that dependency first", o.Name, name)
		}
		if o.Dir == a.Dir {
			return nil, nil, fmt.Errorf("%s shares the directory %s", o.Name, a.Dir)
		}
	}
	p := &Plan{Kind: "delete-app", App: name, Message: "switchboard: delete app " + name}
	// Unregister FIRST: that is the commit that makes Flux prune the app. The remaining commits only tidy the repo.
	var firstErr error
	add := func(path string, f func([]byte) ([]byte, error)) {
		c, err := newUpdate(s, path, f)
		if err != nil && firstErr == nil {
			firstErr = err
			return
		}
		if err == nil {
			p.Changes = append(p.Changes, c)
		}
	}
	add(cfg.AppsDir+"/kustomization.yaml", func(b []byte) ([]byte, error) { return UnregisterApp(b, name) })
	if firstErr != nil {
		return nil, nil, firstErr
	}
	p.Changes = append(p.Changes, Change{Path: a.KFile, Action: ActDelete, Old: s.Files[a.KFile]})
	if a.Built || hasImageDocs(s, cfg, name) {
		if _, err := RemoveImageAutomation(s.Files[cfg.AppsDir+"/image-automation.yaml"], name); err == nil {
			add(cfg.AppsDir+"/image-automation.yaml", func(b []byte) ([]byte, error) {
				out, err := RemoveImageAutomation(b, name)
				if errors.Is(err, ErrNotFound) {
					return b, nil
				}
				return out, err
			})
		}
	}
	if a.Spec.Host != "" && contains(HomepageHrefs(s.Files["apps/homepage/configmap.yaml"]), "https://"+a.Spec.Host) {
		add("apps/homepage/configmap.yaml", func(b []byte) ([]byte, error) {
			out, err := RemoveHomepage(b, a.Spec.Host)
			if errors.Is(err, ErrNotFound) {
				return b, nil
			}
			return out, err
		})
	}
	if firstErr != nil {
		return nil, nil, firstErr
	}
	var dirFiles []string
	for pth := range s.Paths {
		if strings.HasPrefix(pth, a.Dir+"/") {
			dirFiles = append(dirFiles, pth)
		}
	}
	sort.Strings(dirFiles)
	for _, f := range dirFiles {
		p.Changes = append(p.Changes, Change{Path: f, Action: ActDelete, Old: s.Files[f]})
	}
	return p, a, nil
}

func hasImageDocs(s *Snapshot, cfg *Config, name string) bool {
	for _, d := range imageDocs(s, cfg) {
		if str(dig(d, "metadata", "name")) == name {
			return true
		}
	}
	return false
}

// ---- applying -----------------------------------------------------------------------------------

type Progress func(state, msg string)

// Apply commits the changes one by one. It stops at the first failure and says exactly what landed.
func Apply(ctx context.Context, repo Repo, creator RepoCreator, p *Plan, say Progress) (last string, err error) {
	if sp := p.Source; sp != nil {
		if creator == nil {
			say("error", "repo creation is not configured")
			return "", errors.New("no repo creator")
		}
		if err := createSourceRepo(ctx, creator, sp, say); err != nil {
			say("error", fmt.Sprintf("%v. Nothing was committed to the citadel repo.", err))
			return "", err
		}
	}
	total := len(p.Changes)
	for i, c := range p.Changes {
		msg := p.Message
		if total > 1 {
			msg = fmt.Sprintf("%s [%d/%d]", p.Message, i+1, total)
		}
		short := path.Base(c.Path)
		for attempt := 0; ; attempt++ {
			var sha string
			switch c.Action {
			case ActCreate:
				sha, err = repo.PutFile(ctx, c.Path, "", msg, c.New)
			case ActUpdate:
				var f *RepoFile
				if f, err = repo.ReadFile(ctx, c.Path); err != nil {
					break
				}
				body := c.New
				if string(f.Content) != string(c.Old) { // moved on since the preview: re-apply the edit to the fresh text
					if body, err = c.Rebuild(f.Content); err != nil {
						break
					}
				}
				sha, err = repo.PutFile(ctx, c.Path, f.SHA, msg, body)
			case ActDelete:
				var f *RepoFile
				if f, err = repo.ReadFile(ctx, c.Path); err != nil {
					if errors.Is(err, ErrNotFound) {
						err = nil // already gone
						sha = last
					}
					break
				}
				sha, err = repo.DeleteFile(ctx, c.Path, f.SHA, msg)
			}
			if errors.Is(err, ErrConflict) && attempt < 2 {
				continue
			}
			if err == nil {
				if sha != "" {
					last = sha
				}
				say("ok", fmt.Sprintf("%s %s (%d/%d)", past(c.Action), c.Path, i+1, total))
			}
			break
		}
		if err != nil {
			say("error", fmt.Sprintf("%s %s failed: %v. %d of %d commits landed; nothing was rolled back.", c.Action, short, err, i, total))
			return last, err
		}
	}
	return last, nil
}

func past(a string) string {
	switch a {
	case ActCreate:
		return "Created"
	case ActUpdate:
		return "Updated"
	}
	return "Deleted"
}
