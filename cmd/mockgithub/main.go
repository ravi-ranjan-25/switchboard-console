// Command mockgithub serves the seed repository from testdata/seed on a local port, speaking enough of the
// GitHub API for Switchboard (GITHUB_API_URL=http://127.0.0.1:9099). Development only.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/ravi-ranjan-25/switchboard-console/internal/mockgh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "listen address")
	seed := flag.String("seed", "testdata/seed", "directory with the fake repo contents")
	repo := flag.String("repo", "example-owner/citadel", "owner/name")
	flag.Parse()
	var owner, name string
	for i := range *repo {
		if (*repo)[i] == '/' {
			owner, name = (*repo)[:i], (*repo)[i+1:]
		}
	}
	s, err := mockgh.FromFS(owner, name, os.DirFS(*seed), ".")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("mock GitHub for %s on http://%s (seed %s, %d files)", *repo, *addr, *seed, len(s.Paths()))
	log.Fatal(http.ListenAndServe(*addr, s.Handler()))
}
