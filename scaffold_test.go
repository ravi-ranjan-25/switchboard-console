package main

import (
	"strings"
	"testing"
)

func TestVaultKey(t *testing.T) {
	if got := VaultKey("notes", "API_KEY"); got != "app-notes--api-key" {
		t.Error(got)
	}
}

func TestNormalizeDefaults(t *testing.T) {
	n := &NewApp{Name: "x1", ImageKind: "built", Image: "ghcr.io/example-owner/x1", Port: 8080, Host: " X1.Example.com "}
	if errs := n.Normalize(testConfig("")); len(errs) != 0 {
		t.Fatal(errs)
	}
	if n.Mode != ModePrivate || n.Namespace != "personal" || *n.Replicas != 0 || n.Host != "x1.example.com" || n.UID != 1000 || n.Size != "small" || n.HealthPath != "/" {
		t.Errorf("%+v", n)
	}
	p := &NewApp{Name: "x2", ImageKind: "pinned", Image: "docker.io/library/nginx:1.27", Port: 80, Host: "x2.example.com"}
	p.Normalize(testConfig(""))
	if *p.Replicas != 1 {
		t.Error("public images start at 1 replica")
	}
}

func TestGeneratedFilesAreValidYAMLAndPassPortedChecks(t *testing.T) {
	for _, n := range []*NewApp{builtApp(), {Name: "plain", ImageKind: "pinned", Image: "docker.io/library/nginx:1.27", Port: 80, Host: "plain.example.com", Mode: "public", Namespace: "client"}} {
		if errs := n.Normalize(testConfig("")); len(errs) != 0 {
			t.Fatal(errs)
		}
		for p, c := range n.Generate() {
			if docs, err := decodeDocs([]byte(c)); err != nil || len(docs) == 0 {
				t.Errorf("%s: %v", p, err)
			}
			if strings.Contains(c, "\t") {
				t.Errorf("%s contains tabs", p)
			}
		}
	}
}
