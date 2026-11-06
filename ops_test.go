package main

import (
	"strings"
	"testing"
	"time"
)

func builtApp() *NewApp {
	return &NewApp{Name: "notes", ImageKind: "built", Image: "notes-svc", Port: 3000, Host: "notes.example.com",
		Mode: "private", Secrets: []string{"API_KEY"}, PVCSize: "2Gi", Description: "Notes app"}
}

func TestNewAppPreviewShowsEveryFileAndValidates(t *testing.T) {
	e := newEnv(t)
	code, pr := e.plan(t, OpReq{Op: "new-app", New: builtApp()})
	if code != 200 || !pr.OK {
		t.Fatalf("%d %+v", code, pr)
	}
	var paths []string
	for _, f := range pr.Files {
		paths = append(paths, f.Action+" "+f.Path)
		if f.Diff == "" {
			t.Errorf("%s has no diff", f.Path)
		}
	}
	want := []string{"create apps/notes/kustomization.yaml", "create apps/notes/deployment.yaml", "create apps/notes/service.yaml",
		"create apps/notes/pvc.yaml", "create apps/notes/externalsecret.yaml", "create clusters/keep/apps/notes.yaml",
		"update clusters/keep/apps/image-automation.yaml", "update apps/homepage/configmap.yaml", "update clusters/keep/apps/kustomization.yaml"}
	if strings.Join(paths, "|") != strings.Join(sortedLike(want, paths), "|") && len(paths) != len(want) {
		t.Errorf("files:\n%v", paths)
	}
	for _, w := range want {
		found := false
		for _, p := range paths {
			found = found || p == w
		}
		if !found {
			t.Errorf("preview lacks %s", w)
		}
	}
	// registration is the very last commit
	if last := pr.Files[len(pr.Files)-1].Path; last != "clusters/keep/apps/kustomization.yaml" {
		t.Errorf("last commit is %s", last)
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("preview must not commit")
	}
	for _, c := range pr.Checks {
		if c.Status == "fail" {
			t.Errorf("check failed: %+v", c)
		}
	}
	if len(pr.Notes) == 0 {
		t.Error("expected notes (CI workflow, vault names)")
	}
}

func sortedLike(want, _ []string) []string { return want }

func TestNewAppGeneratedContent(t *testing.T) {
	e := newEnv(t)
	_, pr := e.plan(t, OpReq{Op: "new-app", New: builtApp()})
	by := map[string]string{}
	for _, f := range pr.Files {
		by[f.Path] = f.Content
	}
	dep := by["apps/notes/deployment.yaml"]
	for _, w := range []string{`image: ghcr.io/example-owner/notes-svc:20260101000000-0000000 # {"$imagepolicy": "flux-system:notes"}`,
		"imagePullSecrets:\n        - name: ghcr-pull", "runAsNonRoot: true", "type: RuntimeDefault", "allowPrivilegeEscalation: false",
		"drop: [ALL]", "replicas: 0", "type: Recreate", "claimName: notes-data", "name: notes\n"} {
		if !strings.Contains(dep, w) {
			t.Errorf("deployment lacks %q", w)
		}
	}
	ks := by["clusters/keep/apps/notes.yaml"]
	for _, w := range []string{"expose-private", "app-netpol", "backup-pvc", "pvc-backup", "restore-if-empty", "PVC: notes-data", `PORT: "3000"`, "HOST: notes.example.com", "- name: infra-configs"} {
		if !strings.Contains(ks, w) {
			t.Errorf("flux kustomization lacks %q", w)
		}
	}
	if strings.Contains(ks, "ALLOWED_EMAILS") || strings.Contains(ks, "components/db-client") {
		t.Error("unexpected optional parts")
	}
	if es := by["apps/notes/externalsecret.yaml"]; !strings.Contains(es, "key: app-notes--api-key") || !strings.Contains(es, "secretKey: API_KEY") {
		t.Errorf("externalsecret:\n%s", es)
	}
	ia := by["clusters/keep/apps/image-automation.yaml"]
	_ = ia
	for _, f := range pr.Files {
		if f.Path == "clusters/keep/apps/image-automation.yaml" {
			if !strings.Contains(f.Diff, "+  image: ghcr.io/example-owner/notes-svc") || !strings.Contains(f.Diff, "+    name: ghcr-pull") {
				t.Errorf("image automation diff:\n%s", f.Diff)
			}
		}
		if f.Path == "apps/homepage/configmap.yaml" && !strings.Contains(f.Diff, "+            href: https://notes.example.com") {
			t.Errorf("homepage diff:\n%s", f.Diff)
		}
	}
}

func TestNewAppRefusals(t *testing.T) {
	e := newEnv(t)
	cases := map[string]func(n *NewApp){
		"name":           func(n *NewApp) { n.Name = "Bad_Name" },
		"name ":          func(n *NewApp) { n.Name = "excalidraw" }, // exists
		"reserved":       func(n *NewApp) { n.Name = "switchboard" },
		"host":           func(n *NewApp) { n.Host = "draw.example.com" }, // used
		"port":           func(n *NewApp) { n.Port = 70000 },
		"image latest":   func(n *NewApp) { n.ImageKind, n.Image = "pinned", "docker.io/library/nginx:latest" },
		"image untagged": func(n *NewApp) { n.ImageKind, n.Image = "pinned", "docker.io/library/nginx" },
		"image no host":  func(n *NewApp) { n.ImageKind, n.Image = "pinned", "nginx:1.27" },
		"restricted":     func(n *NewApp) { n.Mode = "google-restricted" }, // no emails
		"pvc":            func(n *NewApp) { n.PVCSize = "lots" },
		"secret name":    func(n *NewApp) { n.Secrets = []string{"lower case"} },
		"env secret":     func(n *NewApp) { n.Env = []EnvVar{{Name: "DB_PASSWORD", Value: "x"}} },
		"env value":      func(n *NewApp) { n.Env = []EnvVar{{Name: "FOO", Value: "ghp_" + strings.Repeat("a", 26)}} },
		"ns":             func(n *NewApp) { n.Namespace = "kube-system" },
		"uid":            func(n *NewApp) { n.UID = 70000 },
	}
	for name, mut := range cases {
		n := builtApp()
		if name == "name " {
			n.Name = "x"
		}
		mut(n)
		code, pr := e.plan(t, OpReq{Op: "new-app", New: n})
		if code != 422 || pr.OK || len(pr.Errors) == 0 {
			t.Errorf("%s: %d %+v", name, code, pr)
		}
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("nothing may be committed")
	}
}

func TestNewAppPinnedPublicAndRestrictedAndDB(t *testing.T) {
	e := newEnv(t)
	n := &NewApp{Name: "wiki", ImageKind: "pinned", Image: "docker.io/library/nginx:1.27.2", Port: 80, Host: "wiki.example.com",
		Mode: "google-restricted", AllowedEmails: "B@x.com, a@x.com", DBClient: true, UID: 101, Env: []EnvVar{{Name: "LOG_LEVEL", Value: "info"}}}
	code, pr := e.plan(t, OpReq{Op: "new-app", New: n})
	if code != 200 || !pr.OK {
		t.Fatalf("%d %+v", code, pr)
	}
	by := map[string]string{}
	for _, f := range pr.Files {
		by[f.Path] = f.Content
	}
	ks := by["clusters/keep/apps/wiki.yaml"]
	for _, w := range []string{`ALLOWED_EMAILS: "b@x.com,a@x.com"`, "db-client", "- name: db", "expose-google-restricted"} {
		if !strings.Contains(ks, w) {
			t.Errorf("lacks %q:\n%s", w, ks)
		}
	}
	if strings.Contains(by["apps/wiki/deployment.yaml"], "imagepolicy") || strings.Contains(by["apps/wiki/deployment.yaml"], "ghcr-pull") {
		t.Error("pinned public image must have no image automation")
	}
	for _, f := range pr.Files {
		if strings.Contains(f.Path, "image-automation") {
			t.Error("pinned image must not touch image-automation")
		}
	}
	if !strings.Contains(by["apps/wiki/deployment.yaml"], `{name: LOG_LEVEL, value: "info"}`) {
		t.Error("env missing")
	}
}

func TestNewAppApplyCommitsInOrderAndFollowsFlux(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) { e.cl.set("notes", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, evs := e.apply(t, OpReq{Op: "new-app", New: builtApp()})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	log := e.gh.CommitLog()
	if len(log) != 9 {
		t.Fatalf("%d commits: %+v", len(log), log)
	}
	if log[len(log)-1].Path != "clusters/keep/apps/kustomization.yaml" {
		t.Errorf("registration must be last: %+v", log)
	}
	for _, c := range log {
		if !strings.HasPrefix(c.Message, "switchboard: add app notes [") {
			t.Errorf("message %q", c.Message)
		}
	}
	if !strings.Contains(e.file(t, "clusters/keep/apps/kustomization.yaml"), "  - notes.yaml") {
		t.Error("not registered")
	}
	// The end state passes the same checks.
	var d Detail
	if c := e.get(t, "/api/apps/notes", &d); c != 200 || d.Mode != "private" || !d.HasPVC {
		t.Errorf("%d %+v", c, d.AppView)
	}
	e.s.invalidate()
	snap, _ := e.s.snapshot(t.Context(), true)
	if !ChecksOK(CheckApp(snap, e.s.cfg, "notes")) {
		t.Errorf("end state fails checks: %+v", CheckApp(snap, e.s.cfg, "notes"))
	}
	// Re-creating is refused.
	if code, pr := e.plan(t, OpReq{Op: "new-app", New: builtApp()}); code != 422 {
		t.Errorf("duplicate: %d %+v", code, pr)
	}
}

func TestApplyNeedsMatchingHash(t *testing.T) {
	e := newEnv(t)
	req := OpReq{Op: "new-app", New: builtApp()}
	_, pr := e.plan(t, req)
	req.Hash = ""
	if r, _ := e.post(t, "/api/apply", mustJSON(req)); r.StatusCode != 400 {
		t.Errorf("no hash: %d", r.StatusCode)
	}
	req.Hash = "deadbeef"
	if r, m := e.post(t, "/api/apply", mustJSON(req)); r.StatusCode != 409 || m["reload"] != true {
		t.Errorf("wrong hash: %d %v", r.StatusCode, m)
	}
	// the repo moves on after the preview
	req.Hash = pr.Hash
	e.gh.SetFile("clusters/keep/apps/kustomization.yaml", append(append([]byte{}, e.gh.MustFile("clusters/keep/apps/kustomization.yaml")...), []byte("  - other.yaml\n")...))
	if r, _ := e.post(t, "/api/apply", mustJSON(req)); r.StatusCode != 409 {
		t.Errorf("stale preview: %d", r.StatusCode)
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("nothing may be committed")
	}
}

func TestPartialFailureStopsAndSaysSo(t *testing.T) {
	e := newEnv(t)
	req := OpReq{Op: "new-app", New: builtApp()}
	_, pr := e.plan(t, req)
	req.Hash = pr.Hash
	e.gh.FailNext = 500
	// first commit fails: the job reports the error and nothing landed
	r, m := e.post(t, "/api/apply", mustJSON(req))
	if r.StatusCode != 202 {
		t.Fatal(r.StatusCode, m)
	}
	evs := e.events(t, m["job"].(string))
	if last := lastEvent(evs); last.State != "error" || !strings.Contains(last.Msg, "0 of 9 commits landed") {
		t.Errorf("%+v", evs)
	}
	// pending slot released
	if r, _ := e.post(t, "/api/apply", mustJSON(req)); r.StatusCode != 202 {
		t.Errorf("slot not released: %d", r.StatusCode)
	}
}

func TestReplicasSwitch(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) { e.cl.set("excalidraw", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, evs := e.apply(t, OpReq{Op: "replicas", App: "excalidraw", Replicas: intp(0)})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	log := e.gh.CommitLog()
	if len(log) != 1 || log[0].Message != "switchboard: excalidraw replicas 1→0" || log[0].Path != "apps/excalidraw/deployment.yaml" {
		t.Errorf("%+v", log)
	}
	if !strings.Contains(e.file(t, "apps/excalidraw/deployment.yaml"), "replicas: 0") {
		t.Error("replicas not 0")
	}
	if code, _ := e.plan(t, OpReq{Op: "replicas", App: "excalidraw", Replicas: intp(0)}); code != 422 {
		t.Errorf("no-op must be refused: %d", code)
	}
	for _, op := range []OpReq{
		{Op: "replicas", App: "excalidraw", Replicas: intp(2)},
		{Op: "replicas", App: "switchboard", Replicas: intp(0)}, // protected
		{Op: "replicas", App: "db", Replicas: intp(0)},          // not a standard app
		{Op: "replicas", App: "ghost", Replicas: intp(0)},
	} {
		if code, pr := e.plan(t, op); code == 200 || pr.OK {
			t.Errorf("%+v must be refused", op)
		}
	}
}

func TestImageRollbackPinAndResume(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) { e.cl.set("portfolio", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, evs := e.apply(t, OpReq{Op: "image", App: "portfolio", Tag: "20261029101500-a1b2c3d"})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	dep := e.file(t, "apps/portfolio/deployment.yaml")
	if !strings.Contains(dep, "image: registry.example.com/portfolio:20261029101500-a1b2c3d # pinned by switchboard") || strings.Contains(dep, "$imagepolicy") {
		t.Errorf("pin:\n%s", dep)
	}
	var d Detail
	e.s.invalidate()
	e.get(t, "/api/apps/portfolio", &d)
	if !d.Pinned || d.Tag != "20261029101500-a1b2c3d" {
		t.Errorf("%+v", d.AppView)
	}
	if code, _ := e.plan(t, OpReq{Op: "image", App: "portfolio", Tag: "bad tag!"}); code != 422 {
		t.Errorf("bad tag: %d", code)
	}
	code, evs = e.apply(t, OpReq{Op: "resume", App: "portfolio"})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	if !strings.Contains(e.file(t, "apps/portfolio/deployment.yaml"), `20261029101500-a1b2c3d # {"$imagepolicy": "flux-system:portfolio"}`) {
		t.Error("marker not restored")
	}
	if code, _ := e.plan(t, OpReq{Op: "resume", App: "portfolio"}); code != 422 {
		t.Error("resume when not pinned must be refused")
	}
	if code, _ := e.plan(t, OpReq{Op: "image", App: "excalidraw", Tag: "1.0"}); code != 422 {
		t.Error("public image has no tag rollback")
	}
}

func TestDeleteApp(t *testing.T) {
	e := newEnv(t)
	e.cl.set("feedback", KStatus{Ready: "True"})
	e.cl.onNote = func(string) { e.cl.del("feedback") }
	code, pr := e.plan(t, OpReq{Op: "delete", App: "feedback"})
	if code != 200 || !pr.OK || len(pr.Danger) < 2 {
		t.Fatalf("%d %+v", code, pr)
	}
	if first := pr.Files[0].Path; first != "clusters/keep/apps/kustomization.yaml" {
		t.Errorf("unregister must be first, got %s", first)
	}
	req := OpReq{Op: "delete", App: "feedback", Hash: pr.Hash}
	if r, _ := e.post(t, "/api/apply", mustJSON(req)); r.StatusCode != 400 {
		t.Errorf("without confirm: %d", r.StatusCode)
	}
	req.Confirm = "feedback"
	if r, _ := e.post(t, "/api/apply", mustJSON(req)); r.StatusCode != 400 {
		t.Errorf("without data-loss ack: %d", r.StatusCode)
	}
	req.AckDataLoss = true
	r, m := e.post(t, "/api/apply", mustJSON(req))
	if r.StatusCode != 202 {
		t.Fatal(r.StatusCode, m)
	}
	if evs := e.events(t, m["job"].(string)); lastEvent(evs).State != "done" {
		t.Fatalf("%+v", evs)
	}
	for _, p := range e.gh.Paths() {
		if strings.Contains(p, "feedback") {
			t.Errorf("%s survived", p)
		}
	}
	if strings.Contains(e.file(t, "clusters/keep/apps/kustomization.yaml"), "feedback.yaml") {
		t.Error("still registered")
	}
	if !strings.Contains(e.file(t, "apps/homepage/configmap.yaml"), "https://draw.example.com") {
		t.Error("other homepage entries must stay")
	}
	if strings.Contains(e.file(t, "apps/homepage/configmap.yaml"), "play.example.com") {
		t.Error("homepage entry not removed")
	}
	log := e.gh.CommitLog()
	if log[0].Path != "clusters/keep/apps/kustomization.yaml" {
		t.Errorf("first commit %+v", log[0])
	}
}

func TestDeleteRefusals(t *testing.T) {
	e := newEnv(t)
	for _, app := range []string{"switchboard", "db", "ghost"} {
		if code, pr := e.plan(t, OpReq{Op: "delete", App: app}); code == 200 || pr.OK {
			t.Errorf("%s must be refused", app)
		}
	}
	// dependency: planka dependsOn db, and a stub app that depends on excalidraw
	e.gh.SetFile("clusters/keep/apps/homepage.yaml", []byte(strings.Replace(e.file(t, "clusters/keep/apps/homepage.yaml"), "    - name: infra-configs\n", "    - name: infra-configs\n    - name: excalidraw\n", 1)))
	code, pr := e.plan(t, OpReq{Op: "delete", App: "excalidraw"})
	if code != 422 || !strings.Contains(pr.Errors[0].Msg, "depends on") {
		t.Errorf("%d %+v", code, pr)
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("nothing may be committed")
	}
}

func TestPendingRefused(t *testing.T) {
	e := newEnv(t)
	_, pr := e.plan(t, OpReq{Op: "replicas", App: "excalidraw", Replicas: intp(0)})
	req := OpReq{Op: "replicas", App: "excalidraw", Replicas: intp(0), Hash: pr.Hash}
	e.cl.onNote = nil
	r, m := e.post(t, "/api/apply", mustJSON(req))
	if r.StatusCode != 202 {
		t.Fatal(r.StatusCode, m)
	}
	// The job now waits for Flux (3s timeout): a second request for the same app is refused.
	for i := 0; i < 100 && len(e.gh.CommitLog()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	_, pr2 := e.plan(t, OpReq{Op: "replicas", App: "excalidraw", Replicas: intp(1)})
	req2 := OpReq{Op: "replicas", App: "excalidraw", Replicas: intp(1), Hash: pr2.Hash}
	r2, m2 := e.post(t, "/api/apply", mustJSON(req2))
	if r2.StatusCode != 409 || !strings.Contains(str(m2["error"]), "pending") {
		t.Errorf("%d %v", r2.StatusCode, m2)
	}
}
