package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type AppView struct {
	Name          string    `json:"name"`
	URL           string    `json:"url"`
	Host          string    `json:"host"`
	Mode          string    `json:"mode"`
	AllowedEmails string    `json:"allowed_emails"`
	LockedMode    string    `json:"locked_mode,omitempty"` // denylist rule, human text
	Disabled      []string  `json:"disabled_modes,omitempty"`
	Switchable    bool      `json:"switchable"`
	Protected     bool      `json:"protected"`
	Managed       bool      `json:"managed"`
	Pending       bool      `json:"pending"`
	Pill          string    `json:"pill"`
	Message       string    `json:"message"`
	Revision      string    `json:"revision"`
	LastChange    time.Time `json:"last_change"`
	Namespace     string    `json:"namespace"`
	Replicas      int       `json:"replicas"`
	Image         string    `json:"image"`
	Tag           string    `json:"tag"`
	Built         bool      `json:"built"`
	Pinned        bool      `json:"pinned"`
	HasPVC        bool      `json:"has_pvc"`
	SecretCount   int       `json:"secret_count"`
	DBClient      bool      `json:"db_client"` // network access to the shared Postgres
	DBRole        bool      `json:"db_role"`   // has its own database role on the shared cluster
}

type Event struct {
	State string `json:"state"` // info | ok | error | done
	Msg   string `json:"msg"`
	At    string `json:"at"`
}

type Job struct {
	ID, App string
	mu      sync.Mutex
	events  []Event
	done    bool
	wake    chan struct{}
}

func (j *Job) emit(state, msg string) {
	j.mu.Lock()
	j.events = append(j.events, Event{state, msg, time.Now().UTC().Format(time.RFC3339)})
	if state == "done" || state == "error" {
		j.done = true
	}
	close(j.wake)
	j.wake = make(chan struct{})
	j.mu.Unlock()
}

func (j *Job) snapshot(from int) (evs []Event, done bool, wake chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Event(nil), j.events[from:]...), j.done, j.wake
}

type Server struct {
	cfg     *Config
	repo    Repo
	cluster Cluster
	creator RepoCreator // nil: Create a new project is disabled
	vault   Vault       // nil: the Secrets screen is disabled
	revealM sync.Mutex
	reveals []time.Time
	log     *slog.Logger
	tmpl    *template.Template

	mu      sync.Mutex
	pending map[string]*Job // app (or "domain:<d>") -> running job
	jobs    map[string]*Job
	snap    *Snapshot
	snapAt  time.Time
}

func NewServer(cfg *Config, repo Repo, cl Cluster, log *slog.Logger) (*Server, error) {
	t, err := template.ParseFS(webFS, "web/index.html")
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, repo: repo, cluster: cl, log: log, tmpl: t,
		pending: map[string]*Job{}, jobs: map[string]*Job{}}
	if d, ok := cl.(*demoCluster); ok {
		d.names = s.appNames
	}
	return s, nil
}

// SetVault enables the Secrets screen.
func (s *Server) SetVault(v Vault) { s.vault = v }

// SetCreator enables "Create a new project".
func (s *Server) SetCreator(c RepoCreator) { s.creator = c }

func (s *Server) appNames() []string {
	sn, err := s.snapshot(context.Background(), false)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range loadApps(sn, s.cfg.AppsDir) {
		out = append(out, a.KName)
	}
	return out
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	for _, f := range []string{"/app.js", "/app.css", "/favicon.svg"} {
		mux.Handle("GET "+f, http.FileServerFS(static))
	}
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /api/apps", s.handleApps)
	mux.HandleFunc("GET /api/apps/{app}", s.handleDetail)
	mux.HandleFunc("GET /api/apps/{app}/logs", s.handleLogs)
	mux.HandleFunc("POST /api/switch", s.handleSwitch)
	mux.HandleFunc("POST /api/plan", s.handlePlan)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("GET /api/network", s.handleNetwork)
	mux.HandleFunc("GET /api/secrets", s.handleSecrets)
	mux.HandleFunc("POST /api/secrets/create", s.handleSecretWrite)
	mux.HandleFunc("POST /api/secrets/update", s.handleSecretWrite)
	mux.HandleFunc("POST /api/secrets/reveal", s.handleSecretReveal)
	mux.HandleFunc("GET /api/jobs/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/switch/{id}/events", s.handleEvents)
	return s.secure(mux)
}

// secure wraps everything except /healthz (kubelet probes carry no identity).
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		// Identity: oauth2-proxy sets X-Auth-Request-Email on the forwardAuth response and Traefik copies it
		// onto the request (authResponseHeaders). Traefik overwrites any client-supplied value, and
		// app-netpol only lets Traefik and the tailnet proxies reach the pod. Always enforced.
		if !s.cfg.DevNoAuth && s.identity(r) == "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// identity returns the authenticated email if it is on the allowlist, else "".
func (s *Server) identity(r *http.Request) string {
	e := strings.TrimSpace(r.Header.Get("X-Auth-Request-Email"))
	for _, a := range s.cfg.AllowedEmails {
		if e != "" && strings.EqualFold(a, e) {
			return e
		}
	}
	return ""
}

func (s *Server) who(r *http.Request) string {
	if e := s.identity(r); e != "" {
		return e
	}
	if s.cfg.DevNoAuth {
		return "dev"
	}
	return ""
}

func (s *Server) snapshot(ctx context.Context, force bool) (*Snapshot, error) {
	s.mu.Lock()
	if !force && s.snap != nil && time.Since(s.snapAt) < 20*time.Second {
		c := s.snap
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	sn, err := s.repo.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.snap, s.snapAt = sn, time.Now()
	s.mu.Unlock()
	return sn, nil
}

func (s *Server) invalidate() { s.mu.Lock(); s.snap = nil; s.mu.Unlock() }

func (s *Server) lockedRule(a *AppInfo) string {
	if m, ok := s.cfg.Protected[a.Name]; ok {
		return m
	}
	return s.cfg.Protected[a.KName]
}

func (s *Server) views(ctx context.Context, force bool) ([]AppView, error) {
	sn, err := s.snapshot(ctx, force)
	if err != nil {
		return nil, err
	}
	st, serr := s.cluster.Statuses(ctx)
	if serr != nil {
		s.log.Warn("cluster status unavailable", "err", serr.Error())
	}
	apps := loadApps(sn, s.cfg.AppsDir)
	roles := map[string]bool{}
	if docs, _ := decodeDocs(sn.Files["apps/db/cluster.yaml"]); len(docs) == 1 {
		for _, r := range list(dig(docs[0], "spec", "managed", "roles")) {
			roles[str(dig(r, "name"))] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AppView{}
	for _, a := range apps {
		v := AppView{Name: a.Name, Host: a.Spec.Host, Mode: a.Spec.Mode, AllowedEmails: a.Spec.AllowedEmails,
			Switchable: a.Spec.Mode != "", Managed: a.Managed, Pending: s.pending[a.Name] != nil, Pill: "Unknown",
			Namespace: a.Namespace, Replicas: a.Replicas, Image: a.Image, Tag: a.Tag, Built: a.Built, Pinned: a.Pinned,
			HasPVC: a.HasPVC(), Protected: protectedApp(s.cfg, a),
			DBClient: containsAny(a.Components, "components/db-client"), DBRole: roles[dbName(a.Name)]}
		for _, si := range a.Secrets {
			v.SecretCount += len(si.Keys)
		}
		if rule := s.lockedRule(a); rule != "" {
			v.LockedMode = RuleLabel(rule)
			for _, m := range allModes {
				if !ModeAllowed(rule, m) {
					v.Disabled = append(v.Disabled, m)
				}
			}
		}
		if a.Spec.Host != "" {
			v.URL = "https://" + a.Spec.Host
		}
		if k, ok := st[a.KName]; ok {
			v.Pill, v.Message, v.Revision, v.LastChange = k.Pill(), k.Message, k.Revision, k.LastChange
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if c, err := r.Cookie("sb_csrf"); err == nil && len(c.Value) == 64 {
		tok = c.Value
	} else {
		tok = newToken()
		http.SetCookie(w, &http.Cookie{Name: "sb_csrf", Value: tok, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, map[string]any{"CSRF": tok}); err != nil {
		s.log.Error("render", "err", err.Error())
	}
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"email": s.who(r), "dev": s.cfg.DevNoAuth, "demo": s.cfg.DemoCluster,
		"cluster": !s.cfg.NoCluster || s.cfg.DemoCluster, "repo": s.cfg.Owner + "/" + s.cfg.Repo, "branch": s.cfg.Branch,
		"modes": allModes, "namespaces": s.cfg.AppNamespaces, "image_prefix": s.cfg.ImagePrefix,
		"repo_create": s.creator != nil, "secrets": s.vault != nil, "repo_owner": s.cfg.RepoOwner, "stacks": stackList()})
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	vs, err := s.views(r.Context(), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.log.Error("load apps", "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not read apps from GitHub"})
		return
	}
	writeJSON(w, http.StatusOK, vs)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) csrfOK(r *http.Request) bool {
	c, err := r.Cookie("sb_csrf")
	if err != nil || len(c.Value) != 64 {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

// decodePost applies the shared POST guards (CSRF, JSON only, body cap, no unknown fields).
func (s *Server) decodePost(w http.ResponseWriter, r *http.Request, v any) bool {
	fail := func(code int, msg string) bool { writeJSON(w, code, map[string]any{"error": msg}); return false }
	if !s.csrfOK(r) {
		return fail(http.StatusForbidden, "bad CSRF token; reload the page")
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return fail(http.StatusUnsupportedMediaType, "content-type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fail(http.StatusBadRequest, "invalid JSON body")
	}
	return true
}

// ---- details (read-only) ----------------------------------------------------------------------------

type SecretView struct {
	Name   string       `json:"name"`
	Keys   []string     `json:"keys"` // vault names only
	Status SecretStatus `json:"status"`
}

type Detail struct {
	AppView
	Files      []string     `json:"files"`
	Tags       *ImageStatus `json:"tags,omitempty"`
	TagsErr    string       `json:"tags_error,omitempty"`
	Pods       []PodInfo    `json:"pods"`
	PodsErr    string       `json:"pods_error,omitempty"`
	Secrets    []SecretView `json:"secrets"`
	DependsOn  []string     `json:"depends_on"`
	Components []string     `json:"components"`
}

func (s *Server) appByName(ctx context.Context, name string) (*AppInfo, *Snapshot, error) {
	sn, err := s.snapshot(ctx, false)
	if err != nil {
		return nil, nil, err
	}
	a := findApp(loadApps(sn, s.cfg.AppsDir), name)
	if a == nil {
		return nil, sn, ErrNotFound
	}
	return a, sn, nil
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	a, sn, err := s.appByName(r.Context(), r.PathValue("app"))
	if err != nil {
		code := 502
		if errors.Is(err, ErrNotFound) {
			code = 404
		}
		writeJSON(w, code, map[string]any{"error": "app not available"})
		return
	}
	vs, _ := s.views(r.Context(), false)
	d := Detail{Pods: []PodInfo{}, Secrets: []SecretView{}, DependsOn: a.DependsOn, Components: a.Components}
	for _, v := range vs {
		if v.Name == a.Name {
			d.AppView = v
		}
	}
	if a.Dir != "" {
		for p := range sn.Paths {
			if strings.HasPrefix(p, a.Dir+"/") {
				d.Files = append(d.Files, p)
			}
		}
		sort.Strings(d.Files)
	}
	ns := a.Namespace
	if contains(s.cfg.PodNamespaces, ns) {
		if pods, err := s.cluster.Pods(r.Context(), ns, a.Name); err != nil {
			d.PodsErr = "pods unavailable"
		} else if pods != nil {
			d.Pods = pods
		}
		if len(a.Secrets) > 0 {
			ess, err := s.cluster.ExternalSecrets(r.Context(), ns)
			for _, si := range a.Secrets {
				sv := SecretView{Name: si.Name, Keys: si.Keys, Status: SecretStatus{Ready: "Unknown", Message: "status unavailable"}}
				if st, ok := ess[si.Name]; ok && err == nil {
					sv.Status = st
				}
				d.Secrets = append(d.Secrets, sv)
			}
		}
	}
	if a.Built && a.Setter != "" || a.Pinned {
		name := a.Setter
		if name == "" {
			name = a.Name
		}
		if st, err := s.cluster.ImageStatus(r.Context(), name); err != nil {
			d.TagsErr = "image scan status unavailable"
		} else {
			d.Tags = st
		}
	}
	if len(a.Secrets) == 0 {
		d.Secrets = []SecretView{}
	}
	writeJSON(w, 200, d)
}

var secretLogRe = regexp.MustCompile(`(?i)((?:password|passwd|secret|token|api[_-]?key|cookie)["']?\s*[:=]\s*|authorization["']?\s*[:=]\s*(?:bearer\s+|basic\s+)?|bearer\s+)[^\s"',;&]+`)

func redactLog(s string) string { return secretLogRe.ReplaceAllString(s, "${1}[redacted]") }

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	a, _, err := s.appByName(r.Context(), r.PathValue("app"))
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "app not found"})
		return
	}
	if !contains(s.cfg.PodNamespaces, a.Namespace) {
		writeJSON(w, 403, map[string]any{"error": "namespace not readable"})
		return
	}
	pods, err := s.cluster.Pods(r.Context(), a.Namespace, a.Name)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "pods unavailable"})
		return
	}
	want, cont := r.URL.Query().Get("pod"), r.URL.Query().Get("container")
	var pod *PodInfo
	for i := range pods {
		if pods[i].Name == want {
			pod = &pods[i]
		}
	}
	if pod == nil {
		writeJSON(w, 404, map[string]any{"error": "pod not found for this app"})
		return
	}
	if cont == "" && len(pod.Containers) > 0 {
		cont = pod.Containers[0]
	}
	if !contains(pod.Containers, cont) {
		writeJSON(w, 404, map[string]any{"error": "container not found"})
		return
	}
	tail := 80
	if n := portNum(r.URL.Query().Get("tail")); n > 0 && n <= 300 {
		tail = n
	}
	txt, err := s.cluster.Logs(r.Context(), a.Namespace, pod.Name, cont, tail)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "could not read logs"})
		return
	}
	writeJSON(w, 200, map[string]any{"pod": pod.Name, "container": cont, "lines": strings.Split(strings.TrimRight(redactLog(txt), "\n"), "\n")})
}

// ---- v1: mode switch -----------------------------------------------------------------------------------

type switchReq struct {
	App           string `json:"app"`
	Mode          string `json:"mode"`
	AllowedEmails string `json:"allowed_emails"`
}

func (s *Server) claim(key string, j *Job) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[key] != nil {
		return false
	}
	s.pending[key] = j
	s.jobs[j.ID] = j
	return true
}

func (s *Server) releaseFn(key string) func() {
	return func() { s.mu.Lock(); delete(s.pending, key); s.mu.Unlock() }
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) { writeJSON(w, code, map[string]any{"error": msg}) }
	var req switchReq
	if !s.decodePost(w, r, &req) {
		return
	}
	login := s.who(r)
	if !appNameRe.MatchString(req.App) {
		fail(http.StatusBadRequest, "invalid app name")
		return
	}
	if !validMode(req.Mode) {
		fail(http.StatusBadRequest, "invalid mode")
		return
	}
	emails := ""
	if req.Mode == ModeGoogleRestricted {
		var err error
		if emails, err = NormalizeEmails(req.AllowedEmails); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := CheckProtected(s.cfg.Protected, req.App, req.Mode); err != nil {
		s.audit(login, "switch", req.App, "refused", err.Error())
		fail(http.StatusForbidden, err.Error())
		return
	}
	job := &Job{ID: newToken()[:16], App: req.App, wake: make(chan struct{})}
	if !s.claim(req.App, job) {
		fail(http.StatusConflict, "a change for "+req.App+" is already pending")
		return
	}
	release := s.releaseFn(req.App)

	// Read fresh (never the cache) so the sha precondition is as tight as possible.
	path := s.cfg.AppsDir + "/" + req.App + ".yaml"
	f, err := s.repo.ReadFile(r.Context(), path)
	if err != nil {
		release()
		fail(http.StatusNotFound, "app not found in repo")
		return
	}
	spec, perr := ParseApp(f.Content)
	if perr != nil || spec.Mode == "" {
		release()
		fail(http.StatusUnprocessableEntity, "app has no expose-* component; not switchable")
		return
	}
	if err := CheckProtected(s.cfg.Protected, spec.Name, req.Mode); err != nil {
		release()
		s.audit(login, "switch", req.App, "refused", err.Error())
		fail(http.StatusForbidden, err.Error())
		return
	}
	if spec.Mode == req.Mode && (req.Mode != ModeGoogleRestricted || spec.AllowedEmails == emails) {
		release()
		fail(http.StatusConflict, "already in that mode")
		return
	}
	out, old, err := EditApp(f.Content, req.Mode, emails)
	if err != nil {
		release()
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	sha, err := s.repo.PutFile(r.Context(), f.Path, f.SHA, CommitMessage(req.App, old, req.Mode), out)
	if errors.Is(err, ErrConflict) {
		release()
		s.audit(login, "switch", req.App, "conflict", "")
		s.invalidate()
		writeJSON(w, http.StatusConflict, map[string]any{"error": req.App + " changed underneath; reloading", "reload": true})
		return
	}
	if err != nil {
		release()
		s.audit(login, "switch", req.App, "error", err.Error())
		fail(http.StatusBadGateway, "commit failed")
		return
	}
	s.audit(login, "switch", req.App, "committed", old+"→"+req.Mode+" "+sha)
	s.invalidate()
	go s.follow(job, spec.Name, sha, "ready", release)
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job.ID, "commit": sha, "from": old, "to": req.Mode})
}

func (s *Server) audit(login, op, app, result, detail string) {
	s.log.Info("op", "user", login, "op", op, "app", app, "result", result, "detail", detail)
}

// follow triggers the fetch and watches the Kustomization until it is Ready at the commit ("ready")
// or has disappeared ("gone").
func (s *Server) follow(j *Job, kname, sha, wait string, release func()) {
	defer release()
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	if n, ok := s.cluster.(interface{ NoteCommit(string) }); ok {
		n.NoteCommit(sha)
	}
	s.invalidate()
	if s.cfg.NoCluster && !s.cfg.DemoCluster {
		j.emit("done", "Committed "+short+" to "+s.cfg.Branch+". No cluster connected (dev), so nothing to wait for.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.PollTimeout)
	defer cancel()
	j.emit("ok", "Last commit "+short+" on "+s.cfg.Branch)
	if err := s.cluster.RequestReconcile(ctx); err != nil {
		s.log.Error("reconcile request", "err", err.Error())
		j.emit("info", "Could not trigger Flux fetch; waiting for its normal interval")
	} else {
		j.emit("info", "Asked Flux to fetch now")
	}
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	last := ""
	for {
		st, err := s.cluster.Statuses(ctx)
		if err == nil {
			k, ok := st[kname]
			if wait == "gone" && !ok {
				j.emit("done", kname+" is gone from the cluster; Flux pruned it")
				return
			}
			if ok {
				line := fmt.Sprintf("%s at %s", k.Pill(), k.Revision)
				if line != last {
					last = line
					j.emit("info", kname+": "+line)
				}
				if wait == "ready" && k.Ready == "True" && revisionHasCommit(k.Revision, sha) {
					j.emit("done", kname+" is Ready at the new revision")
					s.log.Info("change applied", "app", j.App, "commit", sha)
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			j.emit("error", "Timed out waiting for "+kname+". The commits are in git; Flux will still apply them.")
			s.log.Warn("follow timeout", "app", j.App, "commit", sha)
			return
		case <-t.C:
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.jobs[r.PathValue("id")]
	s.mu.Unlock()
	fl, ok := w.(http.Flusher)
	if j == nil || !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	i := 0
	for {
		evs, done, wake := j.snapshot(i)
		for _, e := range evs {
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		i += len(evs)
		fl.Flush()
		if done {
			return
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		}
	}
}

type stackInfo struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Port   int    `json:"port"`
	Health string `json:"health"`
}

func stackList() []stackInfo {
	var out []stackInfo
	for _, st := range stacks {
		out = append(out, stackInfo{st.ID, st.Label, st.Port, st.Health})
	}
	return out
}

func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	sn, err := s.snapshot(r.Context(), r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.log.Error("network snapshot", "err", err.Error())
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not read the repo from GitHub"})
		return
	}
	writeJSON(w, 200, BuildNetwork(sn, s.cfg))
}
