package main

import (
	"context"
	"testing"
)

func seedSnapshot(t *testing.T) (*Snapshot, *Config) {
	t.Helper()
	g, _ := newMockRepo(t)
	s, err := g.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s, testConfig("")
}

// The checks must accept the real, hand-written apps of the citadel repo (copied into testdata/seed).
func TestCheckAppAcceptsExistingApps(t *testing.T) {
	s, cfg := seedSnapshot(t)
	n := 0
	for _, a := range loadApps(s, cfg.AppsDir) {
		if !a.Managed {
			continue
		}
		n++
		for _, c := range CheckApp(s, cfg, a.Name) {
			if c.Status == "fail" {
				t.Errorf("%s: %s: %s", a.Name, c.Name, c.Detail)
			}
		}
	}
	if n < 5 {
		t.Errorf("only %d managed apps found", n)
	}
}

func mutate(t *testing.T, s *Snapshot, path, from, to string) *Snapshot {
	t.Helper()
	old, ok := s.Files[path]
	if !ok {
		t.Fatalf("no %s", path)
	}
	p := &Plan{Changes: []Change{{Path: path, Action: ActUpdate, New: []byte(replaceOnce(t, string(old), from, to))}}}
	return p.After(s)
}

func TestCheckAppCatchesProblems(t *testing.T) {
	s, cfg := seedSnapshot(t)
	cases := []struct{ name, path, from, to, check string }{
		{"root user", "apps/excalidraw/deployment.yaml", "runAsNonRoot: true", "runAsNonRoot: false", "pod-security"},
		{"priv esc", "apps/excalidraw/deployment.yaml", "allowPrivilegeEscalation: false", "allowPrivilegeEscalation: true", "pod-security"},
		{"no drop", "apps/excalidraw/deployment.yaml", "drop: [ALL]", "drop: [NET_RAW]", "pod-security"},
		{"latest tag", "apps/excalidraw/deployment.yaml", "image: docker.io/excalidraw/excalidraw@sha256:f7ee194addd607bf831d2af0f0a34463dd4225e426cf35199ef0b12a803398e9", "image: docker.io/excalidraw/excalidraw:latest", "image"},
		{"no probe", "apps/excalidraw/deployment.yaml", "readinessProbe:", "xreadiness:", "probes"},
		{"no limits", "apps/excalidraw/deployment.yaml", "limits: {cpu: 250m, memory: 128Mi}", "other: 1", "resources"},
		{"two exposures", "clusters/keep/apps/excalidraw.yaml", "    - ../../components/app-netpol", "    - ../../components/app-netpol\n    - ../../components/expose-public", "exposure"},
		{"unknown component", "clusters/keep/apps/excalidraw.yaml", "components/app-netpol", "components/nope", "components"},
		{"missing var", "clusters/keep/apps/excalidraw.yaml", "      PORT: \"80\"\n", "", "substitute"},
		{"wrong namespace", "clusters/keep/apps/excalidraw.yaml", "NAMESPACE: personal", "NAMESPACE: kube-system", "namespace"},
		{"no prune", "clusters/keep/apps/excalidraw.yaml", "prune: true", "prune: false", "prune"},
		{"selector", "apps/excalidraw/service.yaml", "citadel.dev/app: excalidraw", "citadel.dev/app: other", "service"},
		{"dup host", "clusters/keep/apps/excalidraw.yaml", "HOST: draw.example.com", "HOST: plan.example.com", "unique-host"},
	}
	for _, c := range cases {
		after := mutate(t, s, c.path, c.from, c.to)
		found := false
		for _, r := range CheckApp(after, cfg, "excalidraw") {
			if r.Status == "fail" && r.Name == c.check {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected a failing %q check", c.name, c.check)
		}
	}
}

func TestCheckAppSecretObjectAndNamespace(t *testing.T) {
	s, cfg := seedSnapshot(t)
	p := &Plan{Changes: []Change{
		{Path: "apps/excalidraw/secret.yaml", Action: ActCreate, New: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: x\n  namespace: personal\n")},
		{Path: "apps/excalidraw/kustomization.yaml", Action: ActUpdate, New: []byte(replaceOnce(t, string(s.Files["apps/excalidraw/kustomization.yaml"]), "  - service.yaml", "  - service.yaml\n  - secret.yaml"))},
	}}
	bad := false
	for _, c := range CheckApp(p.After(s), cfg, "excalidraw") {
		bad = bad || (c.Status == "fail" && c.Name == "no-secret-objects")
	}
	if !bad {
		t.Error("Secret object in git must fail")
	}
}
