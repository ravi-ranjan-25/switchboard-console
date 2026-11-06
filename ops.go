package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpReq is the one request shape behind every multi-step change: POST /api/plan previews it,
// POST /api/apply commits exactly what the preview showed (Hash).
type OpReq struct {
	Op          string     `json:"op"` // new-app | replicas | image | resume | delete | domain
	App         string     `json:"app"`
	Replicas    *int       `json:"replicas"`
	Tag         string     `json:"tag"`
	New         *NewApp    `json:"new"`
	Domain      *DomainReq `json:"domain"`
	Confirm     string     `json:"confirm"`
	AckDataLoss bool       `json:"ack_data_loss"`
	Hash        string     `json:"hash"`
	SecretName  string     `json:"secret_name"` // secret-declare
	Src         string     `json:"src"`         // net-allow / net-revoke: caller (Dst is req.App)
}

type FilePreview struct {
	Repo    string `json:"repo,omitempty"` // set for files of a new project repo (not the citadel repo)
	Path    string `json:"path"`
	Action  string `json:"action"`
	Content string `json:"content,omitempty"` // creates only
	Diff    string `json:"diff"`
}

type PlanResp struct {
	OK     bool          `json:"ok"`
	Op     string        `json:"op"`
	App    string        `json:"app"`
	Hash   string        `json:"hash,omitempty"`
	Msg    string        `json:"commit_message,omitempty"`
	Errors []FieldError  `json:"errors,omitempty"`
	Files  []FilePreview `json:"files"`
	Checks []Check       `json:"checks"`
	Notes  []string      `json:"notes,omitempty"`  // things to know / manual steps
	Danger []string      `json:"danger,omitempty"` // shown in red; commit needs extra acknowledgement
}

type planned struct {
	plan   *Plan
	resp   *PlanResp
	status int
	wait   string // ready | gone | none
	kname  string
}

func (s *Server) planFor(ctx context.Context, snap *Snapshot, req *OpReq) *planned {
	pl := &planned{resp: &PlanResp{Op: req.Op, App: req.App, Files: []FilePreview{}, Checks: []Check{}}, status: 200, wait: "ready"}
	fail := func(code int, msg string) *planned {
		pl.status = code
		pl.resp.Errors = append(pl.resp.Errors, FieldError{"", msg})
		return pl
	}
	cfg := s.cfg
	switch req.Op {
	case "new-app":
		if req.New == nil {
			return fail(400, "missing form")
		}
		p, errs := PlanNewApp(snap, cfg, req.New)
		if errs != nil {
			pl.status, pl.resp.Errors = 422, errs
			return pl
		}
		pl.plan, pl.resp.App = p, req.New.Name
		pl.resp.Checks = CheckApp(p.After(snap), cfg, p.App)
		n := req.New
		if p.Source != nil {
			if errs := s.checkSource(ctx, p, pl.resp); errs != nil {
				pl.status, pl.resp.Errors, pl.plan = 422, errs, nil
				return pl
			}
		}
		if p.Source != nil {
			pl.resp.Notes = append(pl.resp.Notes,
				"A new PRIVATE repo "+p.Source.Owner+"/"+p.Source.Repo+" is created first, with the project and its CI. Its first green run builds the first image; Flux then replaces the placeholder tag (about 10 minutes). Then switch the app on in Switchboard.",
				"The GHCR package inherits the repo's visibility (private); pods pull it with the ghcr-pull secret.")
		} else if n.Built() {
			pl.resp.Notes = append(pl.resp.Notes,
				"Built image: copy docs/app-ci/portfolio-build.yml from the citadel repo into "+n.Image[len(cfg.ImagePrefix):]+"/.github/workflows/build.yml. Flux replaces the placeholder tag once CI has pushed an image, then turn replicas on.",
				"Keep the GHCR package private: pods pull it with the ghcr-pull secret.")
		}
		for _, k := range n.Secrets {
			pl.resp.Notes = append(pl.resp.Notes, "Secret "+k+" is read from the vault as "+VaultKey(n.Name, k)+". Enter its value with make secret (never here).")
		}
		if n.Mode != ModePrivate {
			pl.resp.Notes = append(pl.resp.Notes, "Exposure is "+n.Mode+": the app is reachable as soon as Flux applies it.")
		}
		pl.kname = p.App
		if n.Database {
			pl.resp.Checks = append(pl.resp.Checks, CheckDatabase(p.After(snap), cfg, n.Name)...)
			pl.resp.Notes = append(pl.resp.Notes, dbSecretsNote)
		}
	case "replicas":
		if req.Replicas == nil {
			return fail(400, "replicas missing")
		}
		p, a, err := PlanReplicas(snap, cfg, req.App, *req.Replicas)
		if err != nil {
			return fail(422, err.Error())
		}
		pl.plan, pl.kname = p, a.KName
		pl.resp.Checks = newFailures(CheckApp(snap, cfg, req.App), CheckApp(p.After(snap), cfg, req.App))
		if *req.Replicas == 0 {
			pl.resp.Notes = append(pl.resp.Notes, "The pod is stopped; data on a PVC is kept.")
		}
	case "image", "resume":
		p, a, err := PlanImage(snap, cfg, req.App, req.Tag, req.Op == "resume")
		if err != nil {
			return fail(422, err.Error())
		}
		pl.plan, pl.kname = p, a.KName
		pl.resp.Checks = newFailures(CheckApp(snap, cfg, req.App), CheckApp(p.After(snap), cfg, req.App))
		if req.Op == "image" {
			pl.resp.Notes = append(pl.resp.Notes, "The Flux image marker is removed on purpose, otherwise image automation would bump the tag straight back to the newest build. Use Resume auto-update to hand it back.")
		} else {
			pl.resp.Notes = append(pl.resp.Notes, "Flux image automation may commit a newer tag within about 10 minutes.")
		}
	case "delete":
		p, a, err := PlanDelete(snap, cfg, req.App)
		if err != nil {
			code := 422
			if errors.Is(err, errNotFoundApp) {
				code = 404
			}
			return fail(code, err.Error())
		}
		pl.plan, pl.kname, pl.wait = p, a.KName, "gone"
		after := p.After(snap)
		pl.resp.Checks = deleteChecks(after, cfg, a)
		pl.resp.Danger = append(pl.resp.Danger, "Flux will prune "+a.Name+": its Deployment, Service, Ingress and NetworkPolicies are removed from the cluster.")
		if a.HasPVC() {
			pl.resp.Danger = append(pl.resp.Danger, "This app has a PVC ("+join(a.PVCs)+"). Pruning deletes the volume and its data. Nightly restic backups in the object stores are not touched.")
		}
		if len(a.Secrets) > 0 {
			pl.resp.Notes = append(pl.resp.Notes, "Vault secrets (app-"+a.Name+"--*) are not deleted; remove them from the vault by hand if you no longer need them.")
		}
		if a.Built {
			pl.resp.Notes = append(pl.resp.Notes, "The GHCR package and its CI workflow stay as they are.")
		}
	case "add-database":
		p, a, err := PlanAddDatabase(snap, cfg, req.App)
		if err != nil {
			return fail(422, err.Error())
		}
		pl.plan, pl.kname = p, a.KName
		after := p.After(snap)
		pl.resp.Checks = append(newFailures(CheckApp(snap, cfg, req.App), CheckApp(after, cfg, req.App)), CheckDatabase(after, cfg, req.App)...)
		pl.resp.Notes = append(pl.resp.Notes, dbSecretsNote, "The database is kept when the app is deleted (reclaim policy retain). Existing data is not touched: this creates an empty database "+dbName(req.App)+".")
	case "net-allow", "net-revoke":
		var p *Plan
		var err error
		if req.Op == "net-allow" {
			p, err = PlanNetAllow(snap, cfg, req.Src, req.App)
		} else {
			p, err = PlanNetRevoke(snap, cfg, req.Src, req.App)
		}
		if err != nil {
			return fail(422, err.Error())
		}
		a := findApp(loadApps(snap, cfg.AppsDir), req.App)
		pl.plan, pl.kname = p, a.KName
		after := p.After(snap)
		pl.resp.Checks = append(newFailures(CheckApp(snap, cfg, req.App), CheckApp(after, cfg, req.App)), CheckNetwork(after, cfg, req.App)...)
		if req.Op == "net-allow" {
			pl.resp.Notes = append(pl.resp.Notes, fmt.Sprintf("Allows pods of %s to open TCP %d to pods of %s (ingress only; NetworkPolicies are additive, so the app's other rules stay). Verify with a throwaway pod after Flux applied it: it should fail before and work after.", req.Src, a.ContainerPort, req.App))
		} else {
			pl.resp.Notes = append(pl.resp.Notes, fmt.Sprintf("Removes the rule that let %s reach %s. If a hand-written policy also allows it, that rule still applies.", req.Src, req.App))
		}
	case "secret-declare":
		a, err := manageable(snap, cfg, req.App)
		if err != nil {
			return fail(422, err.Error())
		}
		app, key, ok := ParseSecretName(req.SecretName)
		if !ok || app != a.Name {
			return fail(422, "secret_name must be app-"+a.Name+"--<key>")
		}
		if len(a.Secrets) != 1 {
			return fail(422, "declaring needs exactly one ExternalSecret in "+a.Name+"; add the secret names in citadel or recreate the app with secret names")
		}
		secretKey := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
		c, err := newUpdate(snap, a.Secrets[0].File, func(b []byte) ([]byte, error) { return AddExternalSecretKey(b, secretKey, req.SecretName) })
		if err != nil {
			return fail(422, err.Error())
		}
		p := &Plan{Kind: "secret-declare", App: a.Name, Message: "switchboard: " + a.Name + " declares secret " + req.SecretName, Changes: []Change{c}}
		pl.plan, pl.kname = p, a.KName
		pl.resp.Checks = newFailures(CheckApp(snap, cfg, a.Name), CheckApp(p.After(snap), cfg, a.Name))
		pl.resp.Notes = append(pl.resp.Notes, "Declares the secret only (name, no value). External Secrets then creates the env var "+secretKey+" in the app's Secret; the Deployment must read the whole Secret (envFrom) or reference it, and the pod needs a restart to see it.")
	case "domain":
		if req.Domain == nil {
			return fail(400, "missing form")
		}
		p, checks, notes, errs := PlanDomain(snap, cfg, req.Domain)
		if errs != nil {
			pl.status, pl.resp.Errors = 422, errs
			return pl
		}
		pl.plan, pl.resp.Checks, pl.resp.Notes = p, checks, notes
		pl.resp.App = p.App
		pl.wait = "none"
		if req.Domain.App != "" {
			pl.wait, pl.kname = "ready", req.Domain.App
		}
	default:
		return fail(400, "unknown op")
	}
	pl.resp.Msg, pl.resp.Hash = pl.plan.Message, pl.plan.Hash()
	if sp := pl.plan.Source; sp != nil {
		for _, f := range sp.Files {
			pl.resp.Files = append(pl.resp.Files, FilePreview{Repo: sp.Owner + "/" + sp.Repo, Path: f.Path, Action: ActCreate, Diff: unifiedDiff("/dev/null", f.Path, "", f.Content)})
		}
	}
	for _, c := range pl.plan.Changes {
		fp := FilePreview{Path: c.Path, Action: c.Action}
		switch c.Action {
		case ActCreate:
			fp.Content = string(c.New)
			fp.Diff = unifiedDiff("/dev/null", c.Path, "", string(c.New))
		case ActUpdate:
			fp.Diff = unifiedDiff(c.Path, c.Path, string(c.Old), string(c.New))
		default:
			fp.Diff = unifiedDiff(c.Path, "/dev/null", string(c.Old), "")
		}
		pl.resp.Files = append(pl.resp.Files, fp)
	}
	pl.resp.OK = ChecksOK(pl.resp.Checks)
	return pl
}

var errNotFoundApp = errors.New("app not found")

func join(l []string) string {
	s := ""
	for i, x := range l {
		if i > 0 {
			s += ", "
		}
		s += x
	}
	return s
}

// newFailures keeps warnings and ok-lines of the end state but only the failures that the change introduced.
func newFailures(before, after []Check) []Check {
	had := map[string]bool{}
	for _, c := range before {
		if c.Status == "fail" {
			had[c.Name+"|"+c.Detail] = true
		}
	}
	var out []Check
	for _, c := range after {
		if c.Status == "fail" && had[c.Name+"|"+c.Detail] {
			c.Status = "warn"
			c.Detail += " (already so before this change)"
		}
		out = append(out, c)
	}
	return out
}

func deleteChecks(after *Snapshot, cfg *Config, a *AppInfo) []Check {
	c := &checker{}
	c.ok("not-protected", "%s is not on the protected list", a.Name)
	c.check(!after.Has(a.KFile), "flux-kustomization", a.KFile+" removed", a.KFile+" would remain")
	if r, err := kustResources(after.Files[cfg.AppsDir+"/kustomization.yaml"]); err == nil {
		c.check(!contains(r, a.Name+".yaml"), "unregistered", "no longer listed in "+cfg.AppsDir+"/kustomization.yaml", "still listed")
	}
	c.check(!after.HasDir(a.Dir), "app-files", "all files in "+a.Dir+" removed", a.Dir+" would keep files")
	for _, o := range loadApps(after, cfg.AppsDir) {
		c.check(!contains(o.DependsOn, a.KName), "dependents", "nothing depends on "+a.Name, o.Name+" depends on "+a.Name)
	}
	return c.out
}

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req OpReq
	if !s.decodePost(w, r, &req) {
		return
	}
	snap, err := s.snapshot(r.Context(), true)
	if err != nil {
		s.log.Error("snapshot", "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not read the repo from GitHub"})
		return
	}
	pl := s.planFor(r.Context(), snap, &req)
	writeJSON(w, pl.status, pl.resp)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req OpReq
	if !s.decodePost(w, r, &req) {
		return
	}
	login := s.who(r)
	fail := func(code int, msg string) { writeJSON(w, code, map[string]any{"error": msg}) }
	if req.Hash == "" {
		fail(400, "preview first: the hash of the preview is required")
		return
	}
	snap, err := s.snapshot(r.Context(), true)
	if err != nil {
		fail(http.StatusBadGateway, "could not read the repo from GitHub")
		return
	}
	pl := s.planFor(r.Context(), snap, &req)
	if pl.plan == nil {
		writeJSON(w, pl.status, pl.resp)
		return
	}
	if !pl.resp.OK {
		fail(http.StatusUnprocessableEntity, "validation failed; fix the errors and preview again")
		return
	}
	if pl.resp.Hash != req.Hash {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the repo changed since the preview; preview again", "reload": true})
		return
	}
	if req.Op == "delete" {
		if req.Confirm != req.App {
			fail(http.StatusBadRequest, "type the app name to confirm the deletion")
			return
		}
		if len(pl.resp.Danger) > 1 && !req.AckDataLoss {
			fail(http.StatusBadRequest, "acknowledge the data loss first")
			return
		}
	}
	key := pl.plan.App
	job := &Job{ID: newToken()[:16], App: key, wake: make(chan struct{})}
	if !s.claim(key, job) {
		fail(http.StatusConflict, "a change for "+key+" is already pending")
		return
	}
	release := s.releaseFn(key)
	s.audit(login, req.Op, key, "start", fmt.Sprintf("%d files", len(pl.plan.Changes)+sourceCount(pl.plan)))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		job.emit("info", fmt.Sprintf("Committing %d file(s) to %s", len(pl.plan.Changes), s.cfg.Branch))
		last, err := Apply(ctx, s.repo, s.creator, pl.plan, job.emit)
		s.invalidate()
		if err != nil {
			s.audit(login, req.Op, key, "error", err.Error())
			release()
			return
		}
		s.audit(login, req.Op, key, "committed", last)
		if pl.wait == "none" {
			job.emit("done", "Committed. Follow the manual steps listed in the preview.")
			release()
			return
		}
		s.follow(job, pl.kname, last, pl.wait, release)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job.ID, "files": len(pl.plan.Changes)})
}

func sourceCount(p *Plan) int {
	if p.Source == nil {
		return 0
	}
	return len(p.Source.Files)
}

// checkSource verifies, before anything is created, that repo creation is configured and the name is free.
func (s *Server) checkSource(ctx context.Context, p *Plan, resp *PlanResp) []FieldError {
	sp := p.Source
	full := sp.Owner + "/" + sp.Repo
	if s.creator == nil {
		return []FieldError{{"source", "Create a new project is not configured: set GITHUB_REPO_TOKEN_PATH (see deploy/README.md)"}}
	}
	exists, err := s.creator.Exists(ctx, sp.Owner, sp.Repo)
	if err != nil {
		s.log.Error("repo exists check", "err", err.Error())
		return []FieldError{{"source", "could not check GitHub for " + full}}
	}
	if exists {
		return []FieldError{{"source_repo", full + " already exists; pick another repo name"}}
	}
	resp.Checks = append(resp.Checks,
		Check{"source-repo", "ok", full + " is free and will be created PRIVATE"},
		Check{"source-stack", "ok", fmt.Sprintf("%d files from the %s template, CI workflow pushed last", len(sp.Files), sp.Stack)})
	return nil
}
