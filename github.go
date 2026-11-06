package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v75/github"
)

var (
	ErrConflict = errors.New("file changed underneath (409)")
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("file already exists")
)

// RepoFile is a file read from the repo with the blob SHA used for optimistic locking.
type RepoFile struct {
	Name    string // base name without .yaml
	Path    string
	SHA     string
	Content []byte
}

// Snapshot is one cheap view of the whole branch: every path with its blob sha, plus the content of the
// YAML files Switchboard cares about. Blobs are content-addressed, so they are cached forever and a refresh
// costs a single tree request.
type Snapshot struct {
	Paths map[string]string // path -> blob sha
	Files map[string][]byte // path -> content (only text files under the tracked prefixes)
}

func (s *Snapshot) Has(path string) bool { _, ok := s.Paths[path]; return ok }

// Children lists the file names directly under dir (no recursion).
func (s *Snapshot) Children(dir string) []string {
	var out []string
	pre := strings.TrimSuffix(dir, "/") + "/"
	for p := range s.Paths {
		if rest, ok := strings.CutPrefix(p, pre); ok && !strings.Contains(rest, "/") {
			out = append(out, rest)
		}
	}
	return out
}

// HasDir reports whether any path lives under dir.
func (s *Snapshot) HasDir(dir string) bool {
	pre := strings.TrimSuffix(dir, "/") + "/"
	for p := range s.Paths {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

type Repo interface {
	Snapshot(ctx context.Context) (*Snapshot, error)
	// ReadFile reads a file fresh (never cached) for the sha precondition. ErrNotFound when missing.
	ReadFile(ctx context.Context, path string) (*RepoFile, error)
	// PutFile creates (blobSHA == "") or updates a file; returns the commit SHA. ErrConflict / ErrExists.
	PutFile(ctx context.Context, path, blobSHA, message string, content []byte) (commitSHA string, err error)
	DeleteFile(ctx context.Context, path, blobSHA, message string) (commitSHA string, err error)
}

type GitHubRepo struct {
	c             *github.Client
	owner, repo   string
	branch        string
	prefixes      []string
	committerName string
	committerMail string

	mu    sync.Mutex
	blobs map[string][]byte
}

func NewGitHubRepo(hc *http.Client, cfg *Config) (*GitHubRepo, error) {
	c := github.NewClient(hc)
	if cfg.GitHubBaseURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.GitHubBaseURL, "/") + "/")
		if err != nil {
			return nil, err
		}
		c.BaseURL = u
	}
	return &GitHubRepo{c: c, owner: cfg.Owner, repo: cfg.Repo, branch: cfg.Branch,
		prefixes:      []string{"apps/", cfg.AppsDir + "/", cfg.TofuDir + "/", "bootstrap/"},
		committerName: "citadel-switchboard[bot]", committerMail: cfg.CommitterMail,
		blobs: map[string][]byte{}}, nil
}

func (g *GitHubRepo) tracked(path string, size int) bool {
	if strings.HasSuffix(path, ".sops.yaml") {
		return false // encrypted secrets are never read
	}
	if size > 256<<10 || !(strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".tf")) {
		return false
	}
	for _, p := range g.prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (g *GitHubRepo) Snapshot(ctx context.Context) (*Snapshot, error) {
	t, _, err := g.c.Git.GetTree(ctx, g.owner, g.repo, g.branch, true)
	if err != nil {
		return nil, fmt.Errorf("read tree: %w", err)
	}
	if t.GetTruncated() {
		return nil, errors.New("repository tree is truncated; too large")
	}
	s := &Snapshot{Paths: map[string]string{}, Files: map[string][]byte{}}
	for _, e := range t.Entries {
		if e.GetType() != "blob" {
			continue
		}
		p, sha := e.GetPath(), e.GetSHA()
		s.Paths[p] = sha
		if !g.tracked(p, e.GetSize()) {
			continue
		}
		g.mu.Lock()
		b, ok := g.blobs[sha]
		g.mu.Unlock()
		if !ok {
			blob, _, err := g.c.Git.GetBlob(ctx, g.owner, g.repo, sha)
			if err != nil {
				return nil, fmt.Errorf("read blob %s: %w", p, err)
			}
			if b, err = decodeBlob(blob); err != nil {
				return nil, err
			}
			g.mu.Lock()
			if len(g.blobs) > 4000 {
				g.blobs = map[string][]byte{}
			}
			g.blobs[sha] = b
			g.mu.Unlock()
		}
		s.Files[p] = b
	}
	return s, nil
}

func decodeBlob(b *github.Blob) ([]byte, error) {
	if b.GetEncoding() != "base64" {
		return []byte(b.GetContent()), nil
	}
	fc := github.RepositoryContent{Encoding: b.Encoding, Content: b.Content}
	s, err := fc.GetContent()
	return []byte(s), err
}

func (g *GitHubRepo) ReadFile(ctx context.Context, path string) (*RepoFile, error) {
	fc, _, resp, err := g.c.Repositories.GetContents(ctx, g.owner, g.repo, path, &github.RepositoryContentGetOptions{Ref: g.branch})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get %s: %w", path, err)
	}
	if fc == nil {
		return nil, fmt.Errorf("%s is not a file", path)
	}
	s, err := fc.GetContent()
	if err != nil {
		return nil, err
	}
	base := path[strings.LastIndex(path, "/")+1:]
	return &RepoFile{Name: strings.TrimSuffix(base, ".yaml"), Path: path, SHA: fc.GetSHA(), Content: []byte(s)}, nil
}

func (g *GitHubRepo) who() *github.CommitAuthor {
	return &github.CommitAuthor{Name: github.Ptr(g.committerName), Email: github.Ptr(g.committerMail), Date: &github.Timestamp{Time: time.Now().UTC()}}
}

func mapWriteErr(err error) error {
	var er *github.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		switch er.Response.StatusCode {
		case http.StatusConflict:
			return ErrConflict
		case http.StatusUnprocessableEntity:
			if strings.Contains(er.Message, "sha") {
				return ErrExists
			}
		case http.StatusNotFound:
			return ErrNotFound
		}
	}
	return err
}

func (g *GitHubRepo) PutFile(ctx context.Context, path, blobSHA, message string, content []byte) (string, error) {
	who := g.who()
	o := &github.RepositoryContentFileOptions{Message: github.Ptr(message), Content: content,
		Branch: github.Ptr(g.branch), Committer: who, Author: who}
	if blobSHA != "" {
		o.SHA = github.Ptr(blobSHA)
	}
	res, _, err := g.c.Repositories.UpdateFile(ctx, g.owner, g.repo, path, o)
	if err != nil {
		return "", mapWriteErr(err)
	}
	return res.GetSHA(), nil
}

func (g *GitHubRepo) DeleteFile(ctx context.Context, path, blobSHA, message string) (string, error) {
	who := g.who()
	res, _, err := g.c.Repositories.DeleteFile(ctx, g.owner, g.repo, path, &github.RepositoryContentFileOptions{
		Message: github.Ptr(message), SHA: github.Ptr(blobSHA), Branch: github.Ptr(g.branch), Committer: who, Author: who})
	if err != nil {
		return "", mapWriteErr(err)
	}
	return res.GetSHA(), nil
}
