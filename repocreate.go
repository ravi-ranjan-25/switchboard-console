package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v75/github"
)

// RepoCreator creates new PRIVATE repos on the owner's personal account and fills them. It uses a separate,
// fine-grained PAT (Administration, Contents, Workflows: write): a GitHub App installation cannot create repos under
// a personal account. The token is read from a mounted file, never logged and never put in argv. It is not used for
// anything else, and Switchboard never deletes or changes a repo it did not just create.
type RepoCreator interface {
	Exists(ctx context.Context, owner, name string) (bool, error)
	Create(ctx context.Context, name, description string) error
	PutFile(ctx context.Context, owner, name, path, message string, content []byte) error
}

type ghCreator struct {
	c    *github.Client
	mail string
}

type bearer struct {
	tok string
	rt  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.rt.RoundTrip(r)
}

func NewRepoCreator(cfg *Config) (RepoCreator, error) {
	if len(cfg.RepoToken) == 0 {
		return nil, nil
	}
	c := github.NewClient(&http.Client{Transport: bearer{string(cfg.RepoToken), http.DefaultTransport}, Timeout: 30 * time.Second})
	if cfg.GitHubBaseURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.GitHubBaseURL, "/") + "/")
		if err != nil {
			return nil, err
		}
		c.BaseURL = u
	}
	return &ghCreator{c: c, mail: cfg.CommitterMail}, nil
}

func (g *ghCreator) Exists(ctx context.Context, owner, name string) (bool, error) {
	_, resp, err := g.c.Repositories.Get(ctx, owner, name)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (g *ghCreator) Create(ctx context.Context, name, description string) error {
	_, _, err := g.c.Repositories.Create(ctx, "", &github.Repository{
		Name: github.Ptr(name), Private: github.Ptr(true), Description: github.Ptr(description),
		HasWiki: github.Ptr(false), HasProjects: github.Ptr(false),
	})
	return err
}

func (g *ghCreator) PutFile(ctx context.Context, owner, name, path, message string, content []byte) error {
	who := &github.CommitAuthor{Name: github.Ptr("citadel-switchboard[bot]"), Email: github.Ptr(g.mail), Date: &github.Timestamp{Time: time.Now().UTC()}}
	_, _, err := g.c.Repositories.CreateFile(ctx, owner, name, path, &github.RepositoryContentFileOptions{
		Message: github.Ptr(message), Content: content, Branch: github.Ptr("main"), Committer: who, Author: who})
	return err
}

// createSourceRepo creates the private repo and pushes every file. The workflow is the last file, so only the final
// push starts a build. On failure the repo is left as it is (never deleted) and the message says so.
func createSourceRepo(ctx context.Context, rc RepoCreator, sp *SourcePlan, say Progress) error {
	full := sp.Owner + "/" + sp.Repo
	exists, err := rc.Exists(ctx, sp.Owner, sp.Repo)
	if err != nil {
		return fmt.Errorf("check %s: %w", full, err)
	}
	if exists {
		return fmt.Errorf("%s already exists", full)
	}
	if err := rc.Create(ctx, sp.Repo, sp.Description); err != nil {
		return fmt.Errorf("create %s: %w", full, err)
	}
	say("ok", "Created private repo "+full)
	msg := "Initial commit from Switchboard (" + sp.Stack + " template)"
	for i, f := range sp.Files {
		if err := rc.PutFile(ctx, sp.Owner, sp.Repo, f.Path, msg, []byte(f.Content)); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return fmt.Errorf("push %s (%d of %d) to the new repo %s failed: %w. The repo was created and left as it is; finish or delete it by hand", f.Path, i+1, len(sp.Files), full, err)
		}
	}
	say("ok", fmt.Sprintf("Pushed %d files to %s. CI starts from the last commit.", len(sp.Files), full))
	return nil
}
