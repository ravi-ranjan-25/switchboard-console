package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ravi-ranjan-25/switchboard-console/internal/mockgh"
)

func newMockRepo(t *testing.T) (*GitHubRepo, *mockgh.Server) {
	t.Helper()
	gh, err := mockgh.FromFS("example-owner", "citadel", os.DirFS("testdata/seed"), ".")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gh.Handler())
	t.Cleanup(ts.Close)
	g, err := NewGitHubRepo(ts.Client(), testConfig(ts.URL))
	if err != nil {
		t.Fatal(err)
	}
	return g, gh
}

func TestSnapshotReadsTrackedFilesAndCachesBlobs(t *testing.T) {
	g, _ := newMockRepo(t)
	s, err := g.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Files["clusters/keep/apps/excalidraw.yaml"]; !ok {
		t.Fatal("app file missing")
	}
	if _, ok := s.Files["infra/tofu/variables.tf"]; !ok {
		t.Fatal("tofu file not tracked")
	}
	if !s.Has("components/expose-public/kustomization.yaml") || len(s.Files["components/expose-public/kustomization.yaml"]) != 0 {
		t.Error("components are listed as paths but their content is not fetched")
	}
	if got := s.Children("apps/excalidraw"); len(got) != 3 {
		t.Errorf("children %v", got)
	}
	if !s.HasDir("apps/planka") || s.HasDir("apps/nope") {
		t.Error("HasDir")
	}
	n := len(g.blobs)
	if _, err := g.Snapshot(context.Background()); err != nil || len(g.blobs) != n {
		t.Error("blobs not reused")
	}
}

func TestPutFileCreateUpdateConflictDelete(t *testing.T) {
	g, gh := newMockRepo(t)
	ctx := context.Background()
	sha, err := g.PutFile(ctx, "apps/new/a.yaml", "", "msg", []byte("a: 1\n"))
	if err != nil || sha == "" {
		t.Fatal(sha, err)
	}
	if _, err := g.PutFile(ctx, "apps/new/a.yaml", "", "again", []byte("a: 2\n")); !errors.Is(err, ErrExists) {
		t.Errorf("create over existing: %v", err)
	}
	f, err := g.ReadFile(ctx, "apps/new/a.yaml")
	if err != nil || string(f.Content) != "a: 1\n" {
		t.Fatal(f, err)
	}
	if _, err := g.PutFile(ctx, "apps/new/a.yaml", "0000", "x", []byte("a: 3\n")); !errors.Is(err, ErrConflict) {
		t.Errorf("stale sha: %v", err)
	}
	if _, err := g.PutFile(ctx, "apps/new/a.yaml", f.SHA, "upd", []byte("a: 4\n")); err != nil {
		t.Fatal(err)
	}
	f, _ = g.ReadFile(ctx, "apps/new/a.yaml")
	if _, err := g.DeleteFile(ctx, "apps/new/a.yaml", f.SHA, "del"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.ReadFile(ctx, "apps/new/a.yaml"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after delete: %v", err)
	}
	log := gh.CommitLog()
	if len(log) != 3 || log[0].Action != "create" || log[2].Action != "delete" {
		t.Errorf("%+v", log)
	}
	if !strings.Contains(log[0].Message, "msg") {
		t.Error("message")
	}
}
