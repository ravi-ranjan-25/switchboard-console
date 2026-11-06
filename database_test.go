package main

import (
	"errors"
	"strings"
	"testing"
)

func TestDBEditsAreIdempotentAndChangeOnlyTheirLines(t *testing.T) {
	s, _ := seedSnapshot(t)
	app := "acme"
	for _, e := range dbPlatformEdits(app) {
		old := s.Files[e.path]
		out, err := e.fn(old)
		if err != nil {
			t.Fatalf("%s: %v", e.path, err)
		}
		if _, err := e.fn(out); !errors.Is(err, ErrAlready) {
			t.Errorf("%s: second apply must be ErrAlready, got %v", e.path, err)
		}
		// every original line survives, in order (only additions or the one dump-list line grows)
		if e.path != "apps/db/dump-cronjob.yaml" {
			oi := 0
			ol := strings.Split(string(old), "\n")
			for _, l := range strings.Split(string(out), "\n") {
				if oi < len(ol) && l == ol[oi] {
					oi++
				}
			}
			if oi != len(ol) {
				t.Errorf("%s: an original line changed or moved", e.path)
			}
		}
	}
	role, _ := AddDBRole(s.Files["apps/db/cluster.yaml"], app)
	if !strings.Contains(string(role), "      - name: acme\n        ensure: present\n        login: true\n        passwordSecret:\n          name: db-acme\n  env:") {
		t.Errorf("role placement:\n%s", role)
	}
	dump, _ := AddDumpDB(s.Files["apps/db/dump-cronjob.yaml"], app)
	if !strings.Contains(string(dump), "for d in n8n planka acme; do") {
		t.Error("dump list")
	}
	// dashed app names map to a safe database/role name
	r2, _ := AddDBRole(s.Files["apps/db/cluster.yaml"], "my-app")
	if !strings.Contains(string(r2), "- name: my_app") || !strings.Contains(string(r2), "name: db-my-app") {
		t.Errorf("dashed name:\n%s", r2)
	}
}

func TestBootstrapLinesMatchTheirNeighbours(t *testing.T) {
	s, _ := seedSnapshot(t)
	m, err := AddMappingLine(s.Files["bootstrap/vault/mapping.yaml"], "acme")
	if err != nil {
		t.Fatal(err)
	}
	var plank, acme string
	for _, l := range strings.Split(string(m), "\n") {
		if strings.Contains(l, "keep--planka-db-password:") {
			plank = l
		}
		if strings.Contains(l, "keep--acme-db-password:") {
			acme = l
		}
	}
	if strings.Index(plank, "{key:") != strings.Index(acme, "{key:") || strings.Index(plank, "source:") != strings.Index(acme, "source:") || !strings.HasSuffix(acme, "source: generated}") {
		t.Errorf("alignment:\n%s\n%s", plank, acme)
	}
	c, err := AddCatalogGenerated(s.Files["bootstrap/secrets.catalog.yaml"], "acme")
	if err != nil || !strings.Contains(string(c), "{ key: acme_db_password,          bytes: 32, encoding: base64 }") {
		t.Errorf("catalog: %v", err)
	}
}

func TestSetDatabaseURLReplacesTheHandEnteredOne(t *testing.T) {
	s, _ := seedSnapshot(t)
	hand := `apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: acme
  namespace: personal
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: oci-vault
  target:
    name: acme
    creationPolicy: Owner
  data:
    - secretKey: DATABASE_URL
      remoteRef:
        key: app-acme--database-url
    - secretKey: GOOGLE_CLIENT_ID
      remoteRef:
        key: app-acme--google-client-id
`
	out, err := SetDatabaseURL([]byte(hand), "acme")
	if err != nil {
		t.Fatal(err)
	}
	want := `  target:
    name: acme
    creationPolicy: Owner
    template:
      engineVersion: v2
      mergePolicy: Merge
      data:
        # One source of truth: the same generated password the db cluster gives the acme role.
        DATABASE_URL: "postgresql://acme:{{ .db_password | urlquery }}@db-rw.configs.svc.cluster.local:5432/acme"
  data:
    - secretKey: db_password
      remoteRef:
        key: keep--acme-db-password
    - secretKey: GOOGLE_CLIENT_ID
`
	if !strings.Contains(string(out), want) || strings.Contains(string(out), "app-acme--database-url") {
		t.Errorf("hand-entered entry must be replaced:\n%s", out)
	}
	if _, err := SetDatabaseURL(out, "acme"); !errors.Is(err, ErrAlready) {
		t.Errorf("idempotent: %v", err)
	}
	// no DATABASE_URL entry yet: the db_password entry is added, the rest stays
	pf := s.Files["apps/portfolio/externalsecret.yaml"]
	out2, err := SetDatabaseURL(pf, "portfolio")
	if err != nil || !strings.Contains(string(out2), "    - secretKey: db_password\n      remoteRef:\n        key: keep--portfolio-db-password\n") || strings.Contains(string(out2), "app-portfolio--database-url") {
		t.Fatalf("%v\n%s", err, out2)
	}
	if !strings.Contains(string(out2), "key: app-portfolio--groq-api-key") {
		t.Error("other secrets must stay")
	}
	if _, err := SetDatabaseURL([]byte(strings.Replace(want, "acme", "x", -1)), "x"); err == nil {
		t.Error("garbage must be refused")
	}
}

func TestEnsureDBClient(t *testing.T) {
	s, _ := seedSnapshot(t)
	out, err := EnsureDBClient(s.Files["clusters/keep/apps/excalidraw.yaml"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    - ../../components/app-netpol\n    - ../../components/db-client\n") || !strings.Contains(string(out), "    - name: infra-configs\n    - name: db\n") {
		t.Errorf("%s", out)
	}
	if _, err := EnsureDBClient(out); !errors.Is(err, ErrAlready) {
		t.Error("idempotent")
	}
	// planka already has both
	if _, err := EnsureDBClient(s.Files["clusters/keep/apps/planka.yaml"]); !errors.Is(err, ErrAlready) {
		t.Errorf("planka: %v", err)
	}
}

func TestAddDatabaseToAnExistingApp(t *testing.T) {
	s, cfg := seedSnapshot(t)
	p, a, err := PlanAddDatabase(s, cfg, "portfolio")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, c := range p.Changes {
		paths = append(paths, c.Path)
	}
	want := []string{"bootstrap/secrets.catalog.yaml", "bootstrap/vault/mapping.yaml", "apps/db/externalsecret.yaml", "apps/db/cluster.yaml", "apps/db/databases.yaml",
		"apps/db/dump-cronjob.yaml", "apps/portfolio/externalsecret.yaml", "clusters/keep/apps/portfolio.yaml"}
	if strings.Join(paths, ",") != strings.Join(want, ",") || a.Name != "portfolio" {
		t.Fatalf("order (the app's own Kustomization last):\n%v", paths)
	}
	after := p.After(s)
	for _, c := range append(CheckApp(after, cfg, "portfolio"), CheckDatabase(after, cfg, "portfolio")...) {
		if c.Status == "fail" {
			t.Errorf("%s: %s", c.Name, c.Detail)
		}
	}
	if _, _, err := PlanAddDatabase(after, cfg, "portfolio"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second run must say it is already wired: %v", err)
	}
	// partially done states are completed, not duplicated
	partial := mutate(t, s, "apps/db/cluster.yaml", "          name: db-planka\n", "          name: db-planka\n      - name: portfolio\n        ensure: present\n        login: true\n        passwordSecret:\n          name: db-portfolio\n")
	p2, _, err := PlanAddDatabase(partial, cfg, "portfolio")
	if err != nil || len(p2.Changes) != 7 {
		t.Errorf("partial: %v %d", err, len(p2.Changes))
	}
	for _, bad := range []string{"excalidraw", "switchboard", "ghost", "db"} {
		if _, _, err := PlanAddDatabase(s, cfg, bad); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
}

func TestAddDatabaseHTTPFlowAndNotes(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) { e.cl.set("portfolio", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, pr := e.plan(t, OpReq{Op: "add-database", App: "portfolio"})
	if code != 200 || !pr.OK || len(pr.Files) != 8 {
		t.Fatalf("%d %+v", code, pr)
	}
	if !strings.Contains(strings.Join(pr.Notes, "\n"), "make vault-push") || !strings.Contains(strings.Join(pr.Notes, "\n"), "make secrets") {
		t.Errorf("notes must say what the user does next: %v", pr.Notes)
	}
	code, evs := e.apply(t, OpReq{Op: "add-database", App: "portfolio"})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	log := e.gh.CommitLog()
	if len(log) != 8 || log[7].Path != "clusters/keep/apps/portfolio.yaml" || !strings.HasPrefix(log[0].Message, "switchboard: add database for portfolio [1/8]") {
		t.Fatalf("%+v", log)
	}
	if !strings.Contains(e.file(t, "apps/db/databases.yaml"), "name: portfolio\n  namespace: configs") || !strings.Contains(e.file(t, "bootstrap/vault/mapping.yaml"), "keep--portfolio-db-password:") {
		t.Error("not written")
	}
	if code, _ := e.plan(t, OpReq{Op: "add-database", App: "portfolio"}); code != 422 {
		t.Error("second run must be refused")
	}
}

func TestNewAppWithDatabase(t *testing.T) {
	e := newEnv(t)
	n := &NewApp{Name: "orders", ImageKind: "pinned", Image: "docker.io/library/nginx:1.27.2", Port: 80, Host: "orders.example.com", Database: true, Secrets: []string{"API_KEY"}}
	code, pr := e.plan(t, OpReq{Op: "new-app", New: n})
	if code != 200 || !pr.OK {
		t.Fatalf("%d %+v", code, pr)
	}
	by := map[string]string{}
	for _, f := range pr.Files {
		by[f.Path] = f.Content
	}
	es := by["apps/orders/externalsecret.yaml"]
	for _, w := range []string{"DATABASE_URL: \"postgresql://orders:{{ .db_password | urlquery }}@db-rw.configs.svc.cluster.local:5432/orders\"", "secretKey: db_password", "key: keep--orders-db-password", "key: app-orders--api-key", "mergePolicy: Merge"} {
		if !strings.Contains(es, w) {
			t.Errorf("externalsecret lacks %q:\n%s", w, es)
		}
	}
	ks := by["clusters/keep/apps/orders.yaml"]
	if !strings.Contains(ks, "components/db-client") || !strings.Contains(ks, "    - name: db\n") {
		t.Errorf("flux kustomization:\n%s", ks)
	}
	if !strings.Contains(by["apps/orders/deployment.yaml"], "envFrom:") {
		t.Error("deployment must read the Secret")
	}
	for _, p := range []string{"apps/db/cluster.yaml", "apps/db/databases.yaml", "apps/db/externalsecret.yaml", "apps/db/dump-cronjob.yaml", "bootstrap/vault/mapping.yaml", "bootstrap/secrets.catalog.yaml"} {
		found := false
		for _, f := range pr.Files {
			found = found || f.Path == p
		}
		if !found {
			t.Errorf("preview lacks %s", p)
		}
	}
	// the registration is still the very last commit
	if last := pr.Files[len(pr.Files)-1].Path; last != "clusters/keep/apps/kustomization.yaml" {
		t.Errorf("last commit: %s", last)
	}
	bad := *n
	bad.Secrets = []string{"DATABASE_URL"}
	if code, _ := e.plan(t, OpReq{Op: "new-app", New: &bad}); code != 422 {
		t.Error("DATABASE_URL as a secret name must be refused when Switchboard builds it")
	}
	code, evs := e.apply(t, OpReq{Op: "new-app", New: n})
	if code != 202 || lastEvent(evs).State == "" {
		t.Fatalf("%d %+v", code, evs)
	}
	e.s.invalidate()
	snap, _ := e.s.snapshot(t.Context(), true)
	if !ChecksOK(CheckApp(snap, e.s.cfg, "orders")) || !ChecksOK(CheckDatabase(snap, e.s.cfg, "orders")) {
		t.Errorf("end state: %+v %+v", CheckApp(snap, e.s.cfg, "orders"), CheckDatabase(snap, e.s.cfg, "orders"))
	}
}

func TestSnapshotTracksBootstrapButNeverSopsFiles(t *testing.T) {
	g, gh := newMockRepo(t)
	gh.SetFile("bootstrap/secrets.sops.yaml", []byte("ghcr_token: ENC[AES256_GCM,data:x]\n"))
	s, err := g.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Files["bootstrap/vault/mapping.yaml"]; !ok {
		t.Error("mapping.yaml must be tracked")
	}
	if _, ok := s.Files["bootstrap/secrets.sops.yaml"]; ok {
		t.Error("encrypted secrets files must never be read")
	}
}
