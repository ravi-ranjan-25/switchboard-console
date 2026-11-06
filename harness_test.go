package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravi-ranjan-25/switchboard-console/internal/mockgh"
)

type fakeCluster struct {
	mu         sync.Mutex
	reconciles int
	st         map[string]KStatus
	pods       map[string][]PodInfo
	logText    string
	images     map[string]*ImageStatus
	es         map[string]SecretStatus
	onNote     func(sha string)
}

func (c *fakeCluster) NoteCommit(sha string) {
	c.mu.Lock()
	f := c.onNote
	c.mu.Unlock()
	if f != nil {
		f(sha)
	}
}
func (c *fakeCluster) set(name string, k KStatus) { c.mu.Lock(); c.st[name] = k; c.mu.Unlock() }
func (c *fakeCluster) del(name string)            { c.mu.Lock(); delete(c.st, name); c.mu.Unlock() }
func (c *fakeCluster) Statuses(context.Context) (map[string]KStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := map[string]KStatus{}
	for k, v := range c.st {
		cp[k] = v
	}
	return cp, nil
}
func (c *fakeCluster) RequestReconcile(context.Context) error {
	c.mu.Lock()
	c.reconciles++
	c.mu.Unlock()
	return nil
}
func (c *fakeCluster) Pods(_ context.Context, _, app string) ([]PodInfo, error) {
	return c.pods[app], nil
}
func (c *fakeCluster) Logs(context.Context, string, string, string, int) (string, error) {
	return c.logText, nil
}
func (c *fakeCluster) ImageStatus(_ context.Context, n string) (*ImageStatus, error) {
	if s, ok := c.images[n]; ok {
		return s, nil
	}
	return nil, io.EOF
}
func (c *fakeCluster) ExternalSecrets(context.Context, string) (map[string]SecretStatus, error) {
	return c.es, nil
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type env struct {
	s     *Server
	gh    *mockgh.Server
	cl    *fakeCluster
	ts    *httptest.Server
	logs  *lockedBuf
	ck    *http.Cookie
	vault *memVault
}

func testConfig(apiURL string) *Config {
	return &Config{Owner: "example-owner", Repo: "citadel", Branch: "main", AppsDir: "clusters/keep/apps", TofuDir: "infra/tofu",
		Protected:     map[string]string{"switchboard": "<=google-restricted", "auth": "public", "headlamp": "<=google-restricted"},
		AllowedEmails: []string{"me@example.com"}, PollTimeout: 3 * time.Second, PollInterval: 20 * time.Millisecond,
		GitHubBaseURL: apiURL, CommitterMail: "bot@example.com", AppNamespaces: []string{"personal", "client"},
		PodNamespaces: []string{"personal", "client"}, ImagePrefix: "ghcr.io/example-owner/",
		RepoToken: []byte("test-pat"), RepoOwner: "example-owner", IsolatedNamespaces: []string{"client"}, PrimaryDomain: "example.com"}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	gh, err := mockgh.FromFS("example-owner", "citadel", os.DirFS("testdata/seed"), ".")
	if err != nil {
		t.Fatal(err)
	}
	gts := httptest.NewServer(gh.Handler())
	t.Cleanup(gts.Close)
	cfg := testConfig(gts.URL)
	repo, err := NewGitHubRepo(gts.Client(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cl := &fakeCluster{st: map[string]KStatus{"excalidraw": {Ready: "True", Revision: "main@sha1:old", LastChange: time.Now()}},
		pods: map[string][]PodInfo{}, images: map[string]*ImageStatus{}, es: map[string]SecretStatus{}}
	logs := &lockedBuf{}
	s, err := NewServer(cfg, repo, cl, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := NewRepoCreator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.SetCreator(rc)
	mv := NewMemVault()
	s.SetVault(mv)
	h := s.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Raw") == "" {
			r.Header.Set("X-Auth-Request-Email", "me@example.com")
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	e := &env{s: s, gh: gh, cl: cl, ts: ts, logs: logs, vault: mv}
	_, e.ck = e.index(t)
	return e
}

func (e *env) index(t *testing.T) (string, *http.Cookie) {
	t.Helper()
	r, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	for _, c := range r.Cookies() {
		if c.Name == "sb_csrf" {
			return string(b), c
		}
	}
	t.Fatal("no csrf cookie")
	return "", nil
}

func (e *env) postTo(t *testing.T, path string, ck *http.Cookie, tok, body string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("X-CSRF-Token", tok)
	}
	if ck != nil {
		req.AddCookie(ck)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	m := map[string]any{}
	json.NewDecoder(r.Body).Decode(&m)
	return r, m
}

func (e *env) post(t *testing.T, path, body string) (*http.Response, map[string]any) {
	return e.postTo(t, path, e.ck, e.ck.Value, body)
}

func (e *env) get(t *testing.T, path string, v any) int {
	t.Helper()
	r, err := http.Get(e.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if v != nil {
		json.NewDecoder(r.Body).Decode(v)
	}
	return r.StatusCode
}

// plan previews an op and returns the typed response.
func (e *env) plan(t *testing.T, op OpReq) (int, PlanResp) {
	t.Helper()
	b, _ := json.Marshal(op)
	r, err := e.rawPost("/api/plan", b)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var pr PlanResp
	json.NewDecoder(r.Body).Decode(&pr)
	return r.StatusCode, pr
}

func (e *env) rawPost(path string, b []byte) (*http.Response, error) {
	req, _ := http.NewRequest("POST", e.ts.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", e.ck.Value)
	req.AddCookie(e.ck)
	return http.DefaultClient.Do(req)
}

// apply previews, commits what the preview showed, and returns the job's events.
func (e *env) apply(t *testing.T, op OpReq) (int, []Event) {
	t.Helper()
	code, pr := e.plan(t, op)
	if code != 200 || !pr.OK {
		t.Fatalf("plan failed: %d %+v", code, pr)
	}
	op.Hash = pr.Hash
	b, _ := json.Marshal(op)
	r, err := e.rawPost("/api/apply", b)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	m := map[string]any{}
	json.NewDecoder(r.Body).Decode(&m)
	if r.StatusCode != 202 {
		return r.StatusCode, []Event{{State: "error", Msg: str(m["error"])}}
	}
	return 202, e.events(t, m["job"].(string))
}

func (e *env) events(t *testing.T, job string) []Event {
	resp, err := http.Get(e.ts.URL + "/api/jobs/" + job + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var evs []Event
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var ev Event
			json.Unmarshal([]byte(d), &ev)
			evs = append(evs, ev)
		}
	}
	return evs
}

func lastEvent(evs []Event) Event {
	if len(evs) == 0 {
		return Event{}
	}
	return evs[len(evs)-1]
}

func (e *env) file(t *testing.T, p string) string {
	t.Helper()
	b, ok := e.gh.File(p)
	if !ok {
		t.Fatalf("%s missing in mock repo", p)
	}
	return string(b)
}

func intp(i int) *int { return &i }

func replaceOnce(t *testing.T, s, from, to string) string {
	t.Helper()
	if strings.Count(s, from) != 1 {
		t.Fatalf("%q occurs %d times", from, strings.Count(s, from))
	}
	return strings.Replace(s, from, to, 1)
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

var errTest = errString("boom: internal detail")

type errString string

func (e errString) Error() string { return string(e) }

func (e *env) rawGet(t *testing.T, path string) string {
	t.Helper()
	r, err := http.Get(e.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}
