package main

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func renderStack(t *testing.T, id string) ([]SourceFile, *NewApp) {
	t.Helper()
	cfg := testConfig("")
	n := &NewApp{Name: "my-app", Source: &SourceSpec{Stack: id, Description: "A demo"}, Host: "my-app.example.com", Title: "My App"}
	if errs := n.Normalize(cfg); len(errs) != 0 {
		t.Fatalf("%s: %v", id, errs)
	}
	files, err := n.SourceFiles(cfg)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return files, n
}

func TestEveryStackRendersACompleteProject(t *testing.T) {
	common := []string{"Dockerfile", ".dockerignore", ".gitignore", "README.md", "docker-compose.yml", ".env.example", ".editorconfig", ".github/dependabot.yml", ".github/workflows/build.yml"}
	for _, st := range stacks {
		files, n := renderStack(t, st.ID)
		by := map[string]string{}
		for _, f := range files {
			by[f.Path] = f.Content
			if strings.Contains(f.Content, "[[") || strings.Contains(f.Content, "]]") && strings.HasSuffix(f.Path, ".yml") {
				t.Errorf("%s/%s: unresolved template markers", st.ID, f.Path)
			}
		}
		for _, p := range common {
			if _, ok := by[p]; !ok {
				t.Errorf("%s lacks %s", st.ID, p)
			}
		}
		if files[len(files)-1].Path != ".github/workflows/build.yml" {
			t.Errorf("%s: workflow must be pushed last, got %s", st.ID, files[len(files)-1].Path)
		}
		df := by["Dockerfile"]
		if !strings.Contains(df, "USER ") || strings.Contains(df, "USER root") || !strings.Contains(df, "org.opencontainers.image.source=\"https://github.com/example-owner/"+n.Source.Repo+"\"") {
			t.Errorf("%s Dockerfile: non-root user or source label missing", st.ID)
		}
		if !strings.Contains(df, "USER "+itoa(st.UID)) {
			t.Errorf("%s Dockerfile must run as uid %d", st.ID, st.UID)
		}
		// Regression: `useradd --uid 1000` fails in temurin (uid 1000 is "ubuntu"). A numeric USER needs no account.
		if strings.Contains(df, "useradd") || strings.Contains(df, "adduser") {
			t.Errorf("%s Dockerfile must not create users", st.ID)
		}
		var wf map[string]any
		if err := yaml.Unmarshal([]byte(by[".github/workflows/build.yml"]), &wf); err != nil {
			t.Errorf("%s workflow: %v", st.ID, err)
		}
		jobs := asMap(wf["jobs"])
		if jobs["test"] == nil || jobs["image"] == nil {
			t.Errorf("%s workflow needs test and image jobs", st.ID)
		}
		w := by[".github/workflows/build.yml"]
		for _, want := range []string{"platforms: linux/arm64", "ghcr.io/${{ github.repository }}", "packages: write", "needs: test", "github.event_name != 'pull_request'"} {
			if !strings.Contains(w, want) {
				t.Errorf("%s workflow lacks %q", st.ID, want)
			}
		}
		var dep map[string]any
		if err := yaml.Unmarshal([]byte(by[".github/dependabot.yml"]), &dep); err != nil || len(list(dep["updates"])) != 3 {
			t.Errorf("%s dependabot: %v", st.ID, err)
		}
		var cp map[string]any
		if err := yaml.Unmarshal([]byte(by["docker-compose.yml"]), &cp); err != nil {
			t.Errorf("%s compose: %v", st.ID, err)
		}
		if !strings.Contains(by["README.md"], "ghcr.io/example-owner/my-app") || !strings.Contains(by["README.md"], itoa(st.Port)) {
			t.Errorf("%s README", st.ID)
		}
		switch st.ID {
		case "spring-boot":
			var x struct{ XMLName xml.Name }
			if err := xml.Unmarshal([]byte(by["pom.xml"]), &x); err != nil {
				t.Errorf("pom.xml: %v", err)
			}
			if _, ok := by["src/main/java/com/example/myapp/Application.java"]; !ok || !strings.Contains(by["src/main/java/com/example/myapp/Application.java"], "package com.example.myapp;") {
				t.Errorf("java package path: %v", keys(by))
			}
		case "nextjs", "react":
			var pj map[string]any
			if err := json.Unmarshal([]byte(by["package.json"]), &pj); err != nil || pj["name"] != "my-app" {
				t.Errorf("package.json: %v", err)
			}
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func keys(m map[string]string) []string {
	var o []string
	for k := range m {
		o = append(o, k)
	}
	return o
}

func TestStackDefaultsDriveTheCitadelEntry(t *testing.T) {
	cfg := testConfig("")
	n := &NewApp{Name: "shop", Source: &SourceSpec{Stack: "django"}, Host: "Shop.Example.com", Port: 1, HealthPath: "/other", UID: 5}
	if errs := n.Normalize(cfg); len(errs) != 0 {
		t.Fatal(errs)
	}
	if n.Port != 8000 || n.HealthPath != "/healthz" || n.UID != 1000 || n.ImageKind != "built" || n.Image != "ghcr.io/example-owner/shop" || *n.Replicas != 0 {
		t.Errorf("stack must fix port, health, uid and image: %+v", n)
	}
	if !contains(n.Secrets, "DJANGO_SECRET_KEY") {
		t.Errorf("django needs its secret declared by name: %v", n.Secrets)
	}
	env := map[string]string{}
	for _, e := range n.Env {
		env[e.Name] = e.Value
	}
	if env["DJANGO_ALLOWED_HOSTS"] != "shop.example.com" || env["DJANGO_CSRF_TRUSTED_ORIGINS"] != "https://shop.example.com" || env["DJANGO_DEBUG"] != "0" {
		t.Errorf("env %v", env)
	}
	r := &NewApp{Name: "site", Source: &SourceSpec{Stack: "react"}, Host: "site.example.com"}
	r.Normalize(cfg)
	if r.UID != 101 || r.Size != "tiny" {
		t.Errorf("nginx runs as 101: %+v", r)
	}
	for name, bad := range map[string]*NewApp{
		"stack":  {Name: "a1", Source: &SourceSpec{Stack: "cobol"}, Host: "a1.example.com"},
		"repo":   {Name: "a2", Source: &SourceSpec{Stack: "django", Repo: "Bad Repo"}, Host: "a2.example.com"},
		"dotgit": {Name: "a3", Source: &SourceSpec{Stack: "django", Repo: "x.git"}, Host: "a3.example.com"},
	} {
		if errs := bad.Normalize(cfg); len(errs) == 0 {
			t.Errorf("%s must be refused", name)
		}
	}
}

func sourceApp(stack string) *NewApp {
	return &NewApp{Name: "notes", Source: &SourceSpec{Stack: stack, Description: "Notes service"}, Host: "notes.example.com", Mode: "private", Title: "Notes"}
}

func TestCreateProjectPreviewAndApply(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) { e.cl.set("notes", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, pr := e.plan(t, OpReq{Op: "new-app", New: sourceApp("spring-boot")})
	if code != 200 || !pr.OK {
		t.Fatalf("%d %+v", code, pr)
	}
	newRepo, citadel := 0, 0
	for _, f := range pr.Files {
		if f.Repo == "example-owner/notes" {
			newRepo++
			if citadel > 0 {
				t.Error("project files must be listed before citadel files")
			}
		} else {
			citadel++
		}
	}
	if newRepo < 12 || citadel != 7 {
		t.Errorf("project files %d, citadel files %d", newRepo, citadel)
	}
	if len(e.gh.RepoNames()) != 1 {
		t.Error("preview must not create anything")
	}
	// the real thing
	code, evs := e.apply(t, OpReq{Op: "new-app", New: sourceApp("spring-boot")})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	if !e.gh.RepoPrivate("example-owner/notes") {
		t.Fatal("the new repo must be private")
	}
	if e.gh.LastToken != "test-pat" {
		t.Error("repo creation must use the PAT")
	}
	paths := e.gh.RepoPaths("example-owner/notes")
	if len(paths) != newRepo {
		t.Errorf("%d files in the new repo, previewed %d", len(paths), newRepo)
	}
	var order []string
	for _, c := range e.gh.AllCommits() {
		if c.Repo == "example-owner/notes" {
			order = append(order, c.Path)
		}
	}
	if order[len(order)-1] != ".github/workflows/build.yml" {
		t.Errorf("workflow must be the last push: %v", order)
	}
	if !strings.Contains(e.file(t, "apps/notes/deployment.yaml"), `ghcr.io/example-owner/notes:20260101000000-0000000 # {"$imagepolicy": "flux-system:notes"}`) ||
		!strings.Contains(e.file(t, "apps/notes/deployment.yaml"), "port: http") {
		t.Error("citadel deployment")
	}
	if !strings.Contains(e.file(t, "apps/notes/deployment.yaml"), "path: /actuator/health") || !strings.Contains(e.file(t, "apps/notes/deployment.yaml"), "memory: 256Mi") {
		t.Errorf("spring probes and medium size:\n%s", e.file(t, "apps/notes/deployment.yaml"))
	}
	// order: project first, then citadel; and the commit log message
	all := e.gh.AllCommits()
	if all[0].Repo != "example-owner/notes" || all[len(all)-1].Path != "clusters/keep/apps/kustomization.yaml" {
		t.Errorf("order %+v", all)
	}
}

func TestCreateProjectRefusals(t *testing.T) {
	e := newEnv(t)
	// repo name taken
	if code, _ := e.apply(t, OpReq{Op: "new-app", New: sourceApp("fastapi")}); code != 202 {
		t.Fatal("first create")
	}
	n := sourceApp("fastapi")
	n.Name, n.Host = "notes2", "notes2.example.com"
	n.Source.Repo = "notes"
	if code, pr := e.plan(t, OpReq{Op: "new-app", New: n}); code != 422 || !strings.Contains(pr.Errors[0].Msg, "already exists") {
		t.Errorf("taken repo: %d %+v", code, pr)
	}
	// feature off
	e.s.SetCreator(nil)
	n2 := sourceApp("django")
	n2.Name, n2.Host = "shop", "shop.example.com"
	if code, pr := e.plan(t, OpReq{Op: "new-app", New: n2}); code != 422 || !strings.Contains(pr.Errors[0].Msg, "not configured") {
		t.Errorf("no creator: %d %+v", code, pr)
	}
}

func TestCreateProjectFailureLeavesCitadelUntouched(t *testing.T) {
	e := newEnv(t)
	op := OpReq{Op: "new-app", New: sourceApp("nextjs")}
	_, pr := e.plan(t, op)
	op.Hash = pr.Hash
	e.gh.FailNext = 500 // the repo creation call
	r, m := e.post(t, "/api/apply", mustJSON(op))
	if r.StatusCode != 202 {
		t.Fatal(r.StatusCode)
	}
	evs := e.events(t, m["job"].(string))
	if last := lastEvent(evs); last.State != "error" || !strings.Contains(last.Msg, "Nothing was committed to the citadel repo") {
		t.Errorf("%+v", evs)
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("citadel must be untouched")
	}
}

func TestDjangoProjectDeclaresItsSecretInCitadel(t *testing.T) {
	e := newEnv(t)
	n := sourceApp("django")
	n.Name, n.Host = "shop", "shop.example.com"
	_, pr := e.plan(t, OpReq{Op: "new-app", New: n})
	if !pr.OK {
		t.Fatalf("%+v", pr)
	}
	joined := strings.Join(pr.Notes, "\n")
	if !strings.Contains(joined, "app-shop--django-secret-key") {
		t.Errorf("notes must name the vault key:\n%s", joined)
	}
	found := false
	for _, f := range pr.Files {
		found = found || (f.Path == "apps/shop/externalsecret.yaml" && strings.Contains(f.Content, "key: app-shop--django-secret-key"))
	}
	if !found {
		t.Error("ExternalSecret missing")
	}
}

// Python and JSON templates must at least be syntactically valid; skipped when python3 is absent.
func TestPythonTemplatesCompile(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	for _, id := range []string{"fastapi", "django"} {
		files, _ := renderStack(t, id)
		dir := t.TempDir()
		var targets []string
		for _, f := range files {
			if strings.HasSuffix(f.Path, ".py") {
				p := filepath.Join(dir, f.Path)
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, []byte(f.Content), 0o644)
				targets = append(targets, p)
			}
		}
		out, err := exec.Command(py, append([]string{"-I", "-c", "import ast,sys\nfor p in sys.argv[1:]: ast.parse(open(p).read(), p)"}, targets...)...).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v\n%s", id, err, out)
		}
	}
}

// SB_DUMP_TEMPLATES=<dir> go test -run TestDumpTemplates . writes every rendered project to <dir>/<stack>
// so they can be built by hand (used to verify the templates against real toolchains).
func TestDumpTemplates(t *testing.T) {
	dir := os.Getenv("SB_DUMP_TEMPLATES")
	if dir == "" {
		t.Skip("set SB_DUMP_TEMPLATES")
	}
	for _, st := range stacks {
		files, _ := renderStack(t, st.ID)
		for _, f := range files {
			p := filepath.Join(dir, st.ID, f.Path)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
