package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// demoCluster serves canned, clearly fake cluster data for local UI work (DEV_NO_CLUSTER=1 DEV_DEMO_CLUSTER=1).
// Every Kustomization the repo lists is "Ready" at the last commit it was told about.
type demoCluster struct {
	mu    sync.Mutex
	names func() []string
	rev   string
	since time.Time
}

func newDemoCluster() *demoCluster {
	return &demoCluster{rev: "main@sha1:0000000000000000000000000000000000000000", since: time.Now().Add(-3 * time.Hour)}
}

func (d *demoCluster) NoteCommit(sha string) {
	d.mu.Lock()
	d.rev, d.since = "main@sha1:"+sha, time.Now()
	d.mu.Unlock()
}

func (d *demoCluster) Statuses(context.Context) (map[string]KStatus, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]KStatus{}
	if d.names == nil {
		return out, nil
	}
	for i, n := range d.names() {
		s := KStatus{Ready: "True", Reason: "ReconciliationSucceeded", Message: "Applied revision " + d.rev, Revision: d.rev, Attempted: d.rev, LastChange: d.since.Add(-time.Duration(i) * time.Hour)}
		switch n {
		case "planka":
			s = KStatus{Ready: "Unknown", Reason: "Progressing", Message: "Reconciliation in progress", Revision: d.rev}
		case "feedback":
			s = KStatus{Ready: "False", Reason: "HealthCheckFailed", Message: "Deployment feedback not ready", Revision: d.rev, LastChange: d.since}
		}
		out[n] = s
	}
	return out, nil
}

func (d *demoCluster) RequestReconcile(context.Context) error { return nil }

func (d *demoCluster) Pods(_ context.Context, _ string, app string) ([]PodInfo, error) {
	if app == "feedback" {
		return []PodInfo{{Name: app + "-7c9d8b6f5-x2k4p", Phase: "Running", Ready: "0/1", Restarts: 4, Reason: "CrashLoopBackOff", Image: "ghcr.io/example-owner/feedback:20261029101500-a1b2c3d", Started: time.Now().Add(-2 * time.Hour), Containers: []string{app}}}, nil
	}
	return []PodInfo{{Name: app + "-6d5f7c88b-9wq7z", Phase: "Running", Ready: "1/1", Image: "demo/" + app + ":1.0", Started: time.Now().Add(-26 * time.Hour), Containers: []string{app}}}, nil
}

func (d *demoCluster) Logs(_ context.Context, _, pod, _ string, tail int) (string, error) {
	var sb strings.Builder
	for i := 0; i < tail && i < 12; i++ {
		fmt.Fprintf(&sb, "2026-11-01T10:%02d:00Z INFO %s request handled path=/ status=200 ms=%d\n", i, pod[:strings.Index(pod, "-")], 3+i)
	}
	return sb.String(), nil
}

func (d *demoCluster) ImageStatus(_ context.Context, name string) (*ImageStatus, error) {
	if name == "excalidraw" {
		return nil, errors.New("not found")
	}
	return &ImageStatus{Image: "ghcr.io/example-owner/" + name, TagCount: 41, LastScan: time.Now().Add(-4 * time.Minute), PolicyTag: "20261030141200-9f8e7d6",
		Tags: []string{"20261030141200-9f8e7d6", "20261029101500-a1b2c3d", "20261027083000-0c0ffee", "20261024190000-deadbee", "20261020120000-1234abc", "20261015090000-bcd0123"}}, nil
}

func (d *demoCluster) ExternalSecrets(context.Context, string) (map[string]SecretStatus, error) {
	return map[string]SecretStatus{
		"portfolio": {Ready: "True", Reason: "SecretSynced", Message: "secret synced"},
		"planka":    {Ready: "False", Reason: "SecretSyncedError", Message: "could not get secret data from provider: secret not found"},
	}, nil
}

// newDemoVault is an in-memory vault with obviously fake values, for the local demo only.
func newDemoVault() Vault {
	v := NewMemVault()
	ctx := context.Background()
	soon := time.Now().UTC().AddDate(0, 0, 9).Format("2006-01-02")
	for name, ex := range map[string]string{
		"app-portfolio--groq-api-key": "", "app-portfolio--auth-secret": soon, "app-portfolio--database-url": "",
		"app-fitness--anthropic-api-key": "", "app-fitness--db-password": "", "app-portfolio--old-experiment": "2026-10-01",
	} {
		_ = v.Create(ctx, name, []byte("demo-value-not-a-real-secret"), ex)
	}
	_, _ = v.AddVersion(ctx, "app-portfolio--auth-secret", []byte("demo-value-v2"), soon)
	return v
}
