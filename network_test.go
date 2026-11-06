package main

import (
	"strings"
	"testing"
)

func edgeSet(v *NetworkView) map[string]NetEdge {
	m := map[string]NetEdge{}
	for _, e := range v.Edges {
		m[e.Src+">"+e.Dst] = e
	}
	return m
}

func TestBuildNetworkFromTheRealApps(t *testing.T) {
	s, cfg := seedSnapshot(t)
	v := BuildNetwork(s, cfg)
	by := map[string]NetApp{}
	for _, a := range v.Apps {
		by[a.Name] = a
	}
	if by["excalidraw"].Port != 80 || by["portfolio"].Port != 3000 || by["planka"].Port != 1337 {
		t.Errorf("container ports: %+v", by)
	}
	// clientapp: Service port 80 but the pod listens on 8080; NetworkPolicy matches the pod port
	if by["clientapp"].Port != 8080 || !by["clientapp"].Isolated || by["clientapp"].Namespace != "client" {
		t.Errorf("clientapp %+v", by["clientapp"])
	}
	if _, ok := by["switchboard"]; ok {
		t.Error("unmanaged apps are not part of the matrix")
	}
	if e := edgeSet(v)["planka>db"]; e.Kind != "db" || e.Port != 5432 {
		t.Errorf("db-client edge missing: %+v", v.Edges)
	}
	for _, e := range v.Edges {
		if e.Kind != "db" {
			t.Errorf("no app-to-app edge exists in the seed: %+v", e)
		}
	}
}

func TestAllowCreatesOnePolicyPerDestinationAndRevokeRestoresTheRepo(t *testing.T) {
	s, cfg := seedSnapshot(t)
	p, err := PlanNetAllow(s, cfg, "feedback", "planka")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) != 2 || p.Changes[0].Path != "apps/planka/netpol-from-apps.yaml" || p.Changes[0].Action != ActCreate ||
		p.Changes[1].Path != "apps/planka/kustomization.yaml" || p.Changes[1].Action != ActUpdate {
		t.Fatalf("order: file first, listing last: %+v", p.Changes)
	}
	body := string(p.Changes[0].New)
	for _, w := range []string{"name: planka-from-apps", "namespace: personal", "citadel.dev/app: planka", "citadel.dev/app: feedback",
		"kubernetes.io/metadata.name: personal", "port: 1337", "- Ingress"} {
		if !strings.Contains(body, w) {
			t.Errorf("policy lacks %q:\n%s", w, body)
		}
	}
	after := p.After(s)
	v := BuildNetwork(after, cfg)
	if e := edgeSet(v)["feedback>planka"]; e.Kind != "managed" || e.Port != 1337 {
		t.Errorf("edge after: %+v", v.Edges)
	}
	if !ChecksOK(CheckApp(after, cfg, "planka")) || !ChecksOK(CheckNetwork(after, cfg, "planka")) {
		t.Errorf("end state invalid: %+v %+v", CheckApp(after, cfg, "planka"), CheckNetwork(after, cfg, "planka"))
	}
	// second caller updates the same file
	p2, err := PlanNetAllow(after, cfg, "portfolio", "planka")
	if err != nil || len(p2.Changes) != 1 || p2.Changes[0].Action != ActUpdate {
		t.Fatalf("%v %+v", err, p2)
	}
	after2 := p2.After(after)
	if got := managedCallers(after2, loadAppFrom(after2, cfg, "planka")); len(got) != 2 {
		t.Errorf("callers %v", got)
	}
	// revoke the second: file shrinks; revoke the last: unlisted first, then deleted
	r2, err := PlanNetRevoke(after2, cfg, "portfolio", "planka")
	if err != nil || len(r2.Changes) != 1 || string(r2.Changes[0].New) != body {
		t.Fatalf("revoke one must give back the single-caller file byte for byte: %v", err)
	}
	r1, err := PlanNetRevoke(after, cfg, "feedback", "planka")
	if err != nil || len(r1.Changes) != 2 || r1.Changes[0].Path != "apps/planka/kustomization.yaml" || r1.Changes[1].Action != ActDelete {
		t.Fatalf("last revoke order: %v %+v", err, r1)
	}
	back := r1.After(after)
	for _, f := range []string{"apps/planka/kustomization.yaml"} {
		if string(back.Files[f]) != string(s.Files[f]) {
			t.Errorf("%s differs from the original after allow+revoke", f)
		}
	}
	if back.Has("apps/planka/netpol-from-apps.yaml") {
		t.Error("policy file must be gone")
	}
}

func TestNetRefusals(t *testing.T) {
	s, cfg := seedSnapshot(t)
	cases := map[string][2]string{
		"self":               {"planka", "planka"},
		"protected caller":   {"switchboard", "planka"},
		"protected callee":   {"planka", "switchboard"},
		"unknown":            {"ghost", "planka"},
		"db":                 {"planka", "db"},
		"client to personal": {"clientapp", "planka"},
		"personal to client": {"planka", "clientapp"},
	}
	for name, c := range cases {
		if _, err := PlanNetAllow(s, cfg, c[0], c[1]); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	// revoking what does not exist
	if _, err := PlanNetRevoke(s, cfg, "feedback", "planka"); err == nil {
		t.Error("revoke of a missing edge")
	}
	// duplicate allow
	p, _ := PlanNetAllow(s, cfg, "feedback", "planka")
	if _, err := PlanNetAllow(p.After(s), cfg, "feedback", "planka"); err == nil {
		t.Error("duplicate allow")
	}
	// egress-restricted cross-namespace caller is refused (needs an egress rule)
	s2 := mutate(t, s, "clusters/keep/apps/feedback.yaml", "    - ../../components/backup-pvc\n", "    - ../../components/backup-pvc\n    - ../../components/egress-restricted\n")
	if _, err := PlanNetAllow(s2, cfg, "feedback", "planka"); err != nil {
		t.Errorf("same-namespace egress-restricted caller is fine: %v", err)
	}
}

func TestHandWrittenRulesAreVisibleButNotRevocable(t *testing.T) {
	s, cfg := seedSnapshot(t)
	hand := `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: planka-from-feedback
  namespace: personal
spec:
  podSelector:
    matchLabels:
      citadel.dev/app: planka
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              citadel.dev/app: feedback
      ports:
        - {protocol: TCP, port: 1337}
`
	p := &Plan{Changes: []Change{
		{Path: "apps/planka/hand.yaml", Action: ActCreate, New: []byte(hand)},
		{Path: "apps/planka/kustomization.yaml", Action: ActUpdate, New: []byte(replaceOnce(t, string(s.Files["apps/planka/kustomization.yaml"]), "  - service.yaml", "  - service.yaml\n  - hand.yaml"))}}}
	s2 := p.After(s)
	if e := edgeSet(BuildNetwork(s2, cfg))["feedback>planka"]; e.Kind != "custom" {
		t.Errorf("custom edge not seen: %+v", e)
	}
	if _, err := PlanNetAllow(s2, cfg, "feedback", "planka"); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("allow over a hand-written rule: %v", err)
	}
	if _, err := PlanNetRevoke(s2, cfg, "feedback", "planka"); err == nil || !strings.Contains(err.Error(), "hand-written") {
		t.Errorf("revoke of a hand-written rule: %v", err)
	}
}

func TestNetworkHTTPFlow(t *testing.T) {
	e := newEnv(t)
	var v NetworkView
	if c := e.get(t, "/api/network", &v); c != 200 || len(v.Apps) < 5 {
		t.Fatalf("%d %+v", c, v)
	}
	e.cl.onNote = func(sha string) { e.cl.set("planka", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, evs := e.apply(t, OpReq{Op: "net-allow", Src: "portfolio", App: "planka"})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	log := e.gh.CommitLog()
	if len(log) != 2 || log[0].Path != "apps/planka/netpol-from-apps.yaml" || log[1].Path != "apps/planka/kustomization.yaml" || !strings.HasPrefix(log[0].Message, "switchboard: allow portfolio -> planka:1337") {
		t.Fatalf("%+v", log)
	}
	e.s.invalidate()
	e.get(t, "/api/network", &v)
	if e2 := edgeSet(&v)["portfolio>planka"]; e2.Kind != "managed" {
		t.Errorf("%+v", v.Edges)
	}
	code, evs = e.apply(t, OpReq{Op: "net-revoke", Src: "portfolio", App: "planka"})
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	if _, ok := e.gh.File("apps/planka/netpol-from-apps.yaml"); ok {
		t.Error("policy file must be deleted")
	}
	if c, _ := e.plan(t, OpReq{Op: "net-allow", Src: "clientapp", App: "planka"}); c != 422 {
		t.Errorf("client isolation: %d", c)
	}
	if c, _ := e.plan(t, OpReq{Op: "net-allow", Src: "switchboard", App: "planka"}); c != 422 {
		t.Errorf("protected: %d", c)
	}
}
