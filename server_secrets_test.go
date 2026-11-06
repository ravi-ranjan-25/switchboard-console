package main

import (
	"strings"
	"testing"
	"time"
)

const sensitive = "S3cr3t-VALUE-do-not-leak"

func TestParseSecretName(t *testing.T) {
	for name, ok := range map[string]bool{"app-portfolio--groq-api-key": true, "app-a--b": true, "app-x--": false, "app--k": false, "keep--zot-admin-password": false,
		"shared--restic-password": false, "app-portfolio--Bad": false, "app-portfolio": false, "watchtower--ntfy-token": false, "app-a--b--c": false} {
		if _, _, got := ParseSecretName(name); got != ok {
			t.Errorf("%s: %v, want %v", name, got, ok)
		}
	}
	if a, k, _ := ParseSecretName("app-my-app--db-password"); a != "my-app" || k != "db-password" {
		t.Errorf("%s %s", a, k)
	}
}

func TestSecretsListShowsStatusDeclaredMissingAndHidesOthers(t *testing.T) {
	e := newEnv(t)
	e.cl.es["portfolio"] = SecretStatus{Ready: "True", Reason: "SecretSynced"}
	ctx := t.Context()
	e.vault.Create(ctx, "app-portfolio--groq-api-key", []byte(sensitive), "")
	e.vault.Create(ctx, "app-portfolio--unused-thing", []byte("x"), time.Now().AddDate(0, 0, 3).Format("2006-01-02"))
	e.vault.Create(ctx, "app-switchboard--anything", []byte("x"), "") // protected app: invisible
	e.vault.Create(ctx, "app-ghost--k", []byte("x"), "")              // unknown app: invisible
	e.vault.data["keep--zot-admin-password"] = &memSecret{versions: [][]byte{[]byte("x")}, meta: SecretMeta{Name: "keep--zot-admin-password"}}
	var resp SecretsResp
	if c := e.get(t, "/api/secrets", &resp); c != 200 || !resp.Enabled {
		t.Fatal(c, resp)
	}
	by := map[string]SecretRow{}
	for _, r := range resp.Rows {
		by[r.Name] = r
	}
	if len(by) != 2 {
		t.Errorf("only the two eligible secrets must show: %v", keysOf(by))
	}
	g := by["app-portfolio--groq-api-key"]
	if len(g.Declared) != 1 || g.Declared[0].ES != "portfolio" || g.Ready != "True" || g.Version != 1 {
		t.Errorf("declared secret: %+v", g)
	}
	u := by["app-portfolio--unused-thing"]
	if len(u.Declared) != 0 || u.ExpiresIn == nil || *u.ExpiresIn > 3 || *u.ExpiresIn < 2 {
		t.Errorf("orphan with expiry: %+v", u)
	}
	// portfolio's ExternalSecret expects 10 names; all but one are missing from the vault
	if len(resp.Missing) != 9 {
		t.Errorf("missing %d: %+v", len(resp.Missing), resp.Missing)
	}
	if len(resp.Apps) == 0 || contains(resp.Apps, "switchboard") {
		t.Errorf("apps %v", resp.Apps)
	}
	// the list must never contain a value
	if raw := e.rawGet(t, "/api/secrets"); strings.Contains(raw, sensitive) {
		t.Error("value leaked into the list")
	}
}

func keysOf[V any](m map[string]V) []string {
	var o []string
	for k := range m {
		o = append(o, k)
	}
	return o
}

func TestCreateUpdateRevealAndNeverLogValues(t *testing.T) {
	e := newEnv(t)
	r, m := e.post(t, "/api/secrets/create", mustJSON(secretReq{App: "portfolio", Key: "NEW_API_KEY", Value: sensitive, Expires: "2099-01-01"}))
	if r.StatusCode != 201 || m["name"] != "app-portfolio--new-api-key" || m["declared"] != false {
		t.Fatalf("%d %v", r.StatusCode, m)
	}
	if e.vault.value("app-portfolio--new-api-key") != sensitive {
		t.Fatal("value not stored")
	}
	if r, m := e.post(t, "/api/secrets/create", mustJSON(secretReq{App: "portfolio", Key: "new-api-key", Value: "x"})); r.StatusCode != 409 {
		t.Errorf("duplicate create: %d %v", r.StatusCode, m)
	}
	r, m = e.post(t, "/api/secrets/update", mustJSON(secretReq{Name: "app-portfolio--new-api-key", Value: sensitive + "-v2"}))
	if r.StatusCode != 200 || m["version"] != float64(2) || !strings.Contains(str(m["note"]), "restart") {
		t.Fatalf("%d %v", r.StatusCode, m)
	}
	// reveal returns the value, once per call, no-store
	r, m = e.post(t, "/api/secrets/reveal", mustJSON(secretReq{Name: "app-portfolio--new-api-key"}))
	if r.StatusCode != 200 || m["value"] != sensitive+"-v2" || m["version"] != float64(2) || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", r.StatusCode, m)
	}
	// logs: who/what/version, never a value
	logs := e.logs.String()
	for _, want := range []string{`"op":"secret-create"`, `"op":"secret-update"`, `"op":"secret-reveal"`, `"app":"app-portfolio--new-api-key"`, `"user"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("audit lacks %s", want)
		}
	}
	if strings.Contains(logs, sensitive) || strings.Contains(logs, "S3cr3t") {
		t.Fatal("a secret value reached the log")
	}
	// and nothing in the mock GitHub either
	for _, p := range e.gh.Paths() {
		if b, _ := e.gh.File(p); strings.Contains(string(b), sensitive) {
			t.Fatalf("value written to the repo: %s", p)
		}
	}
}

func TestSecretRefusals(t *testing.T) {
	e := newEnv(t)
	cases := map[string]struct {
		path string
		req  secretReq
		code int
	}{
		"empty value":      {"/api/secrets/create", secretReq{App: "portfolio", Key: "k", Value: ""}, 400},
		"huge value":       {"/api/secrets/create", secretReq{App: "portfolio", Key: "k", Value: strings.Repeat("a", 17<<10)}, 400},
		"nul":              {"/api/secrets/create", secretReq{App: "portfolio", Key: "k", Value: "a\x00b"}, 400},
		"bad key":          {"/api/secrets/create", secretReq{App: "portfolio", Key: "bad key!", Value: "x"}, 400},
		"traversal app":    {"/api/secrets/create", secretReq{App: "portfolio--x", Key: "k", Value: "x"}, 400},
		"protected app":    {"/api/secrets/create", secretReq{App: "switchboard", Key: "k", Value: "x"}, 403},
		"unknown app":      {"/api/secrets/create", secretReq{App: "ghost", Key: "k", Value: "x"}, 403},
		"infra name":       {"/api/secrets/update", secretReq{Name: "keep--zot-admin-password", Value: "x"}, 400},
		"shared name":      {"/api/secrets/update", secretReq{Name: "shared--restic-password", Value: "x"}, 400},
		"missing secret":   {"/api/secrets/update", secretReq{Name: "app-portfolio--nope", Value: "x"}, 404},
		"past expiry":      {"/api/secrets/create", secretReq{App: "portfolio", Key: "k", Value: "x", Expires: "2001-01-01"}, 400},
		"bad expiry":       {"/api/secrets/create", secretReq{App: "portfolio", Key: "k", Value: "x", Expires: "tomorrow"}, 400},
		"reveal infra":     {"/api/secrets/reveal", secretReq{Name: "keep--zot-admin-password"}, 400},
		"reveal protected": {"/api/secrets/reveal", secretReq{Name: "app-switchboard--k"}, 403},
		"reveal missing":   {"/api/secrets/reveal", secretReq{Name: "app-portfolio--nope"}, 404},
	}
	for name, c := range cases {
		if r, m := e.post(t, c.path, mustJSON(c.req)); r.StatusCode != c.code {
			t.Errorf("%s: %d %v, want %d", name, r.StatusCode, m, c.code)
		}
	}
	if len(e.vault.data) != 0 {
		t.Errorf("nothing may be written: %v", keysOf(e.vault.data))
	}
	if r, _ := e.postTo(t, "/api/secrets/create", nil, "", mustJSON(secretReq{App: "portfolio", Key: "k", Value: "x"})); r.StatusCode != 403 {
		t.Errorf("csrf: %d", r.StatusCode)
	}
	// a vault failure is reported without leaking details
	e.vault.Fail = errTest
	if r, m := e.post(t, "/api/secrets/create", mustJSON(secretReq{App: "portfolio", Key: "k", Value: "x"})); r.StatusCode != 502 || strings.Contains(str(m["error"]), "boom") {
		t.Errorf("%d %v", r.StatusCode, m)
	}
}

func TestRevealIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.vault.Create(t.Context(), "app-portfolio--k", []byte("v"), "")
	got429 := false
	for i := 0; i < 12; i++ {
		r, _ := e.post(t, "/api/secrets/reveal", mustJSON(secretReq{Name: "app-portfolio--k"}))
		if r.StatusCode == 429 {
			got429 = true
		}
	}
	if !got429 {
		t.Error("reveals must be rate limited")
	}
}

func TestSecretsDisabledWithoutVault(t *testing.T) {
	e := newEnv(t)
	e.s.SetVault(nil)
	var resp SecretsResp
	e.get(t, "/api/secrets", &resp)
	if resp.Enabled {
		t.Error("must report disabled")
	}
	if r, _ := e.post(t, "/api/secrets/create", mustJSON(secretReq{App: "portfolio", Key: "k", Value: "x"})); r.StatusCode != 404 {
		t.Errorf("%d", r.StatusCode)
	}
}

func TestDeclareSecretInExternalSecret(t *testing.T) {
	e := newEnv(t)
	e.vault.Create(t.Context(), "app-portfolio--new-api-key", []byte("v"), "")
	e.cl.onNote = func(sha string) { e.cl.set("portfolio", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, evs := e.apply(t, OpReq{Op: "secret-declare", App: "portfolio", SecretName: "app-portfolio--new-api-key"})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	es := e.file(t, "apps/portfolio/externalsecret.yaml")
	if !strings.Contains(es, "    - secretKey: NEW_API_KEY\n      remoteRef:\n        key: app-portfolio--new-api-key\n") {
		t.Errorf("not declared:\n%s", es)
	}
	if code, _ := e.plan(t, OpReq{Op: "secret-declare", App: "portfolio", SecretName: "app-portfolio--new-api-key"}); code != 422 {
		t.Error("declaring twice must be refused")
	}
	for _, bad := range []OpReq{{Op: "secret-declare", App: "portfolio", SecretName: "keep--zot-admin-password"}, {Op: "secret-declare", App: "excalidraw", SecretName: "app-excalidraw--k"}, {Op: "secret-declare", App: "switchboard", SecretName: "app-switchboard--k"}} {
		if code, _ := e.plan(t, bad); code != 422 {
			t.Errorf("%+v must be refused: %d", bad, code)
		}
	}
	// end of file / layout robustness
	src := "apiVersion: external-secrets.io/v1\nkind: ExternalSecret\nmetadata:\n  name: x\nspec:\n  data:\n    - secretKey: A\n      remoteRef:\n        key: app-x--a\n  target:\n    name: x\n"
	out, err := AddExternalSecretKey([]byte(src), "B", "app-x--b")
	if err != nil || !strings.Contains(string(out), "        key: app-x--b\n  target:") {
		t.Errorf("data not last: %v\n%s", err, out)
	}
}
