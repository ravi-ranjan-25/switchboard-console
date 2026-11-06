// Package mockgh is a small in-memory stand-in for the parts of the GitHub REST API that Switchboard
// uses (installation token, git trees/blobs, contents GET/PUT/DELETE). It is used by the tests and by
// cmd/mockgithub for running the app locally without touching a real repository.
package mockgh

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Commit struct {
	SHA, Message, Path, Action, Repo string
}

type repoData struct {
	files   map[string][]byte
	private bool
}

type Server struct {
	mu      sync.Mutex
	primary string // "owner/name" of the seeded repo
	owner   string
	repos   map[string]*repoData
	Commits []Commit
	n       int
	// FailNext makes the next write return this status (e.g. 409), then resets.
	FailNext int
	// LastToken is the bearer token of the last repo-creation request (tests assert it is the PAT, never logged).
	LastToken string
}

func New(owner, repo string, seed map[string][]byte) *Server {
	s := &Server{owner: owner, primary: owner + "/" + repo, repos: map[string]*repoData{}}
	rd := &repoData{files: map[string][]byte{}, private: true}
	for k, v := range seed {
		rd.files[k] = v
	}
	s.repos[s.primary] = rd
	return s
}

func (s *Server) files() map[string][]byte { return s.repos[s.primary].files }

// FromFS loads every file under root of fsys.
func FromFS(owner, repo string, fsys fs.FS, root string) (*Server, error) {
	seed := map[string][]byte{}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		seed[strings.TrimPrefix(strings.TrimPrefix(p, root), "/")] = b
		return nil
	})
	return New(owner, repo, seed), err
}

func BlobSHA(b []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (s *Server) File(path string) ([]byte, bool) { return s.RepoFile(s.primary, path) }
func (s *Server) Paths() []string                 { return s.RepoPaths(s.primary) }

// RepoFile reads a file of any repo ("owner/name").
func (s *Server) RepoFile(full, path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repos[full]
	if r == nil {
		return nil, false
	}
	b, ok := r.files[path]
	return b, ok
}

func (s *Server) RepoPaths(full string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	if r := s.repos[full]; r != nil {
		for p := range r.files {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// RepoNames lists every repo, created ones included.
func (s *Server) RepoNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.repos {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RepoPrivate reports the visibility of a repo.
func (s *Server) RepoPrivate(full string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repos[full] != nil && s.repos[full].private
}

func (s *Server) SetFile(path string, b []byte) {
	s.mu.Lock()
	s.files()[path] = b
	s.mu.Unlock()
}

// CommitLog returns the commits made to the primary repo.
func (s *Server) CommitLog() []Commit {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Commit
	for _, c := range s.Commits {
		if c.Repo == s.primary {
			out = append(out, c)
		}
	}
	return out
}

// AllCommits returns the commits of every repo.
func (s *Server) AllCommits() []Commit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Commit(nil), s.Commits...)
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) Handler() http.Handler {
	base := "/repos/{owner}/{repo}"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /user/repos", s.createRepo)
	mux.HandleFunc("GET "+base, s.getRepo)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		jsonOut(w, 201, map[string]any{"token": "mock-installation-token", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET "+base+"/git/trees/{ref}", s.tree)
	mux.HandleFunc("GET "+base+"/git/blobs/{sha}", s.blob)
	mux.HandleFunc("GET "+base+"/contents/{path...}", s.getContents)
	mux.HandleFunc("PUT "+base+"/contents/{path...}", s.putContents)
	mux.HandleFunc("DELETE "+base+"/contents/{path...}", s.putContents)
	return mux
}

func (s *Server) repoOf(r *http.Request) *repoData {
	return s.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rd := s.repoOf(r); rd != nil {
		jsonOut(w, 200, map[string]any{"name": r.PathValue("repo"), "full_name": r.PathValue("owner") + "/" + r.PathValue("repo"), "private": rd.private})
		return
	}
	jsonOut(w, 404, map[string]any{"message": "Not Found"})
}

func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var body struct {
		Name    string `json:"name"`
		Private bool   `json:"private"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	full := s.owner + "/" + body.Name
	if s.FailNext != 0 {
		code := s.FailNext
		s.FailNext = 0
		jsonOut(w, code, map[string]any{"message": "injected failure"})
		return
	}
	if body.Name == "" || s.repos[full] != nil {
		jsonOut(w, 422, map[string]any{"message": "name already exists on this account"})
		return
	}
	s.repos[full] = &repoData{files: map[string][]byte{}, private: body.Private}
	jsonOut(w, 201, map[string]any{"name": body.Name, "full_name": full, "private": body.Private, "html_url": "https://github.com/" + full})
}

func (s *Server) tree(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rd := s.repoOf(r)
	if rd == nil {
		jsonOut(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	var entries []map[string]any
	for p, b := range rd.files {
		entries = append(entries, map[string]any{"path": p, "mode": "100644", "type": "blob", "sha": BlobSHA(b), "size": len(b)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i]["path"].(string) < entries[j]["path"].(string) })
	jsonOut(w, 200, map[string]any{"sha": "treesha", "tree": entries, "truncated": false})
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := r.PathValue("sha")
	rd := s.repoOf(r)
	if rd == nil {
		jsonOut(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	for _, b := range rd.files {
		if BlobSHA(b) == want {
			jsonOut(w, 200, map[string]any{"sha": want, "encoding": "base64", "content": base64.StdEncoding.EncodeToString(b), "size": len(b)})
			return
		}
	}
	jsonOut(w, 404, map[string]any{"message": "Not Found"})
}

func (s *Server) getContents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.Trim(r.PathValue("path"), "/")
	rd := s.repoOf(r)
	if rd == nil {
		jsonOut(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	if b, ok := rd.files[p]; ok {
		jsonOut(w, 200, map[string]any{"type": "file", "name": p[strings.LastIndex(p, "/")+1:], "path": p, "sha": BlobSHA(b),
			"encoding": "base64", "content": base64.StdEncoding.EncodeToString(b)})
		return
	}
	var list []map[string]any
	seen := map[string]bool{}
	for fp := range rd.files {
		rest, ok := strings.CutPrefix(fp, p+"/")
		if !ok {
			continue
		}
		name, _, isDir := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		e := map[string]any{"name": name, "path": p + "/" + name, "type": "file"}
		if isDir {
			e["type"] = "dir"
		} else {
			e["sha"] = BlobSHA(rd.files[fp])
		}
		list = append(list, e)
	}
	if list == nil {
		jsonOut(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	jsonOut(w, 200, list)
}

func (s *Server) putContents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.Trim(r.PathValue("path"), "/")
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	if s.FailNext != 0 {
		code := s.FailNext
		s.FailNext = 0
		jsonOut(w, code, map[string]any{"message": "injected failure"})
		return
	}
	rd := s.repoOf(r)
	if rd == nil {
		jsonOut(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	cur, exists := rd.files[p]
	del := r.Method == http.MethodDelete
	switch {
	case exists && body.SHA == "":
		jsonOut(w, 422, map[string]any{"message": "Invalid request. \"sha\" wasn't supplied."})
		return
	case exists && body.SHA != BlobSHA(cur):
		jsonOut(w, 409, map[string]any{"message": p + " does not match " + body.SHA})
		return
	case !exists && (del || body.SHA != ""):
		jsonOut(w, 404, map[string]any{"message": "Not Found"})
		return
	}
	action := "update"
	if del {
		delete(rd.files, p)
		action = "delete"
	} else {
		dec, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			jsonOut(w, 422, map[string]any{"message": "bad content"})
			return
		}
		if !exists {
			action = "create"
		}
		rd.files[p] = dec
	}
	s.n++
	sha := fmt.Sprintf("%x", sha1.Sum([]byte(fmt.Sprintf("commit-%d-%s", s.n, p))))
	s.Commits = append(s.Commits, Commit{SHA: sha, Message: body.Message, Path: p, Action: action, Repo: r.PathValue("owner") + "/" + r.PathValue("repo")})
	jsonOut(w, 200, map[string]any{"commit": map[string]any{"sha": sha}, "content": map[string]any{"path": p}})
}

// MustFile returns a file's content or panics (tests).
func (s *Server) MustFile(path string) []byte {
	b, ok := s.File(path)
	if !ok {
		panic("mockgh: no file " + path)
	}
	return b
}
