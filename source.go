package main

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// Project templates for "Create a new project": a complete, buildable app repo (source, tests, Dockerfile, CI that
// builds the arm64 image into GHCR, docker-compose, README). Templates use [[ ]] delimiters so that GitHub Actions
// ${{ }} expressions pass through untouched.

//go:embed all:templates
var templatesFS embed.FS

type Stack struct {
	ID, Label  string
	Port       int
	Health     string
	UID        int
	Size       string
	Secrets    []string                   // secret NAMES the template needs (values live in the vault)
	Env        func(host string) []EnvVar // plain env the template needs
	TestSteps  string                     // indented workflow steps of the test job
	RunLocal   string
	Dependabot string
}

const nodeSteps = `      - uses: actions/setup-node@v4
        with:
          node-version: 22
      - run: npm install --no-audit --no-fund
      - run: npm test
      - run: npm run build`

const pySteps = `      - uses: actions/setup-python@v5
        with:
          python-version: "3.13"
`

var stacks = []Stack{
	{ID: "spring-boot", Label: "Java, Spring Boot 3 (Maven)", Port: 8080, Health: "/actuator/health", UID: 1000, Size: "medium",
		TestSteps: `      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21
          cache: maven
      - run: mvn -B verify`,
		RunLocal:   "```\nmvn spring-boot:run\n```\n\nTests: `mvn verify`. Needs JDK 21 and Maven.",
		Dependabot: "  - package-ecosystem: maven\n    directory: /\n    schedule:\n      interval: monthly\n    ignore:\n      - dependency-name: \"*\"\n        update-types: [\"version-update:semver-major\"]"},
	{ID: "nextjs", Label: "Node, Next.js 15 (React)", Port: 3000, Health: "/api/health", UID: 1000, Size: "small", TestSteps: nodeSteps,
		RunLocal:   "```\nnpm install\nnpm run dev\n```\n\nTests: `npm test`. Production build: `npm run build`. Needs Node 22.",
		Dependabot: "  - package-ecosystem: npm\n    directory: /\n    schedule:\n      interval: monthly\n    ignore:\n      - dependency-name: \"*\"\n        update-types: [\"version-update:semver-major\"]"},
	{ID: "react", Label: "Node, React (Vite) served by nginx", Port: 8080, Health: "/healthz", UID: 101, Size: "tiny", TestSteps: nodeSteps,
		RunLocal:   "```\nnpm install\nnpm run dev\n```\n\nTests: `npm test`. Production build: `npm run build` (static files in `dist/`). Needs Node 22.",
		Dependabot: "  - package-ecosystem: npm\n    directory: /\n    schedule:\n      interval: monthly\n    ignore:\n      - dependency-name: \"*\"\n        update-types: [\"version-update:semver-major\"]"},
	{ID: "fastapi", Label: "Python, FastAPI", Port: 8000, Health: "/health", UID: 1000, Size: "small",
		TestSteps:  pySteps + "      - run: pip install -r requirements-dev.txt\n      - run: pytest",
		RunLocal:   "```\npython -m venv .venv && . .venv/bin/activate\npip install -r requirements-dev.txt\nuvicorn app.main:app --reload\n```\n\nTests: `pytest`. Interactive docs at `/docs`.",
		Dependabot: "  - package-ecosystem: pip\n    directory: /\n    schedule:\n      interval: monthly\n    ignore:\n      - dependency-name: \"*\"\n        update-types: [\"version-update:semver-major\"]"},
	{ID: "django", Label: "Python, Django 5 (gunicorn)", Port: 8000, Health: "/healthz", UID: 1000, Size: "small",
		Secrets: []string{"DJANGO_SECRET_KEY"},
		Env: func(host string) []EnvVar {
			return []EnvVar{{"DJANGO_DEBUG", "0"}, {"DJANGO_ALLOWED_HOSTS", host}, {"DJANGO_CSRF_TRUSTED_ORIGINS", "https://" + host}}
		},
		TestSteps:  pySteps + "      - run: pip install -r requirements.txt\n      - run: DJANGO_DEBUG=1 python manage.py test",
		RunLocal:   "```\npython -m venv .venv && . .venv/bin/activate\npip install -r requirements.txt\nDJANGO_DEBUG=1 python manage.py runserver\n```\n\nTests: `DJANGO_DEBUG=1 python manage.py test`.\n`DJANGO_SECRET_KEY` is a secret: the console declares it by name and its value comes from the vault.",
		Dependabot: "  - package-ecosystem: pip\n    directory: /\n    schedule:\n      interval: monthly\n    ignore:\n      - dependency-name: \"*\"\n        update-types: [\"version-update:semver-major\"]"},
}

func stackByID(id string) (Stack, bool) {
	for _, s := range stacks {
		if s.ID == id {
			return s, true
		}
	}
	return Stack{}, false
}

// SourceSpec asks for a new private repo built from a stack template.
type SourceSpec struct {
	Stack       string `json:"stack"`
	Repo        string `json:"repo"` // defaults to the app name
	Description string `json:"description"`
}

type SourceFile struct {
	Path    string `json:"path"`
	Content string `json:"-"`
}

// SourcePlan is the repo Switchboard creates before it commits to citadel.
type SourcePlan struct {
	Owner, Repo, Description, Stack string
	Files                           []SourceFile
}

var identRe = regexp.MustCompile(`[^a-z0-9]`)

type srcVars struct {
	Name, Repo, Owner, Image, Health, Desc, Title, Pkg, Stack, StackLabel string
	Port, UID                                                             int
	TestSteps, RunLocal, Dependabot                                       string
}

// applySource fills the stack-driven fields. For generated projects the port, health path, uid and image are fixed
// by the template (they are baked into the Dockerfile and the manifests must agree).
func (n *NewApp) applySource(cfg *Config, bad func(f, m string)) {
	sp := n.Source
	st, ok := stackByID(sp.Stack)
	if !ok {
		bad("source", "unknown stack")
		return
	}
	sp.Repo = strings.TrimSpace(sp.Repo)
	if sp.Repo == "" {
		sp.Repo = n.Name
	}
	if !repoNameRe.MatchString(sp.Repo) || strings.HasSuffix(sp.Repo, ".git") || strings.HasPrefix(sp.Repo, ".") {
		bad("source_repo", "lowercase letters, digits, dashes, dots or underscores")
		return
	}
	n.ImageKind, n.Image = "built", cfg.ImagePrefix+sp.Repo
	n.Port, n.HealthPath, n.UID = st.Port, st.Health, st.UID
	if n.Size == "" {
		n.Size = st.Size
	}
	host := strings.ToLower(strings.TrimSpace(n.Host))
	for _, s := range st.Secrets {
		if !contains(n.Secrets, s) {
			n.Secrets = append(n.Secrets, s)
		}
	}
	if st.Env != nil {
		have := map[string]bool{}
		for _, e := range n.Env {
			have[e.Name] = true
		}
		for _, e := range st.Env(host) {
			if !have[e.Name] {
				n.Env = append(n.Env, e)
			}
		}
	}
	n.Description = strings.TrimSpace(n.Description)
	sp.Description = strings.TrimSpace(sp.Description)
	if sp.Description == "" {
		sp.Description = n.Description
	}
}

// SourceFiles renders the project. The CI workflow is returned LAST: the repo is filled by one contents-API commit
// per file, and a workflow that exists from the first push would start a build for every one of them.
func (n *NewApp) SourceFiles(cfg *Config) ([]SourceFile, error) {
	st, ok := stackByID(n.Source.Stack)
	if !ok {
		return nil, fmt.Errorf("unknown stack %q", n.Source.Stack)
	}
	desc := n.Source.Description
	if desc == "" {
		desc = "Created with Switchboard."
	}
	owner := cfg.RepoOwner
	if owner == "" {
		owner = cfg.Owner
	}
	ident := identRe.ReplaceAllString(strings.ToLower(n.Name), "")
	if ident == "" || (ident[0] >= '0' && ident[0] <= '9') {
		ident = "app" + ident
	}
	v := srcVars{Name: n.Name, Repo: n.Source.Repo, Owner: owner, Image: n.Image, Health: st.Health, Desc: desc, Title: n.Title,
		Pkg: "com.example." + ident, Stack: st.ID, StackLabel: st.Label, Port: st.Port, UID: st.UID,
		TestSteps: st.TestSteps, RunLocal: st.RunLocal, Dependabot: st.Dependabot}
	pkgPath := strings.ReplaceAll(v.Pkg, ".", "/")

	render := func(name string, src []byte) ([]byte, error) {
		t, err := template.New(name).Delims("[[", "]]").Option("missingkey=error").Parse(string(src))
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		if err := t.Execute(&b, v); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	var files []SourceFile
	add := func(root string, rename func(string) string) error {
		return fs.WalkDir(templatesFS, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := templatesFS.ReadFile(p)
			if err != nil {
				return err
			}
			out, err := render(p, b)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			rel := strings.TrimPrefix(p, root+"/")
			if rename != nil {
				rel = rename(rel)
			}
			if rel != "" {
				files = append(files, SourceFile{Path: strings.ReplaceAll(rel, "__pkg__", pkgPath), Content: string(out)})
			}
			return nil
		})
	}
	if err := add("templates/"+st.ID, nil); err != nil {
		return nil, err
	}
	if err := add("templates/_common", func(rel string) string {
		switch rel {
		case "build.yml":
			return ".github/workflows/build.yml"
		case "dependabot.yml":
			return ".github/dependabot.yml"
		}
		return rel
	}); err != nil {
		return nil, err
	}
	sort.SliceStable(files, func(i, j int) bool {
		wi, wj := files[i].Path == ".github/workflows/build.yml", files[j].Path == ".github/workflows/build.yml"
		if wi != wj {
			return wj // workflow last
		}
		return files[i].Path < files[j].Path
	})
	for i, f := range files {
		if path.IsAbs(f.Path) || strings.Contains(f.Path, "..") || strings.Contains(f.Content, "[[") && !strings.HasSuffix(f.Path, ".md") {
			return nil, fmt.Errorf("template output problem in %s", files[i].Path)
		}
	}
	return files, nil
}
