package main

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Marker comments on the Deployment image line.
var (
	setterRe      = regexp.MustCompile(`\{"\$imagepolicy":\s*"flux-system:([a-z0-9-]+)(?::[a-z]+)?"\}`)
	pinnedComment = "pinned by switchboard (rollback); Resume auto-update in the console to hand the tag back to Flux"
)

// AppInfo is everything Switchboard knows about one app from the repo (no cluster data).
type AppInfo struct {
	Name      string // clusters/keep/apps/<Name>.yaml
	KName     string // metadata.name
	KFile     string
	Spec      *AppSpec
	DependsOn []string
	Dir       string // apps/<dir>, "" when spec.path is outside ./apps
	Managed   bool   // has Deployment + Service under apps/<dir> and an expose-* component

	DeployPath    string
	Replicas      int // -1 unknown
	Image         string
	ImageRepo     string // image without tag/digest
	Tag           string
	Digest        string
	Setter        string // ImagePolicy name when the Flux marker is present
	Pinned        bool   // rolled back by Switchboard: marker removed
	Built         bool   // image is a Flux-tracked build (marker present or pinned)
	PVCs          []string
	Secrets       []SecretInfo
	Namespace     string
	Port          string
	Components    []string
	ServiceName   string
	ServicePort   int
	ContainerPort int // the pod port NetworkPolicies match (not the Service port)
}

type SecretInfo struct {
	Name   string   `json:"name"`
	Target string   `json:"target"`
	Keys   []string `json:"keys"` // vault names, never values
	File   string   `json:"file"`
}

func (a *AppInfo) HasPVC() bool { return len(a.PVCs) > 0 }

// splitImage splits "repo:tag@sha256:..." into parts.
func splitImage(img string) (repo, tag, digest string) {
	if i := strings.Index(img, "@"); i >= 0 {
		img, digest = img[:i], img[i+1:]
	}
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		img, tag = img[:i], img[i+1:]
	}
	return img, tag, digest
}

func resolve(base, rel string) string { return path.Clean(path.Join(base, rel)) }

// loadApps parses every Flux Kustomization under appsDir of the snapshot.
func loadApps(s *Snapshot, appsDir string) []*AppInfo {
	var out []*AppInfo
	for p, b := range s.Files {
		if path.Dir(p) != appsDir || !strings.HasSuffix(p, ".yaml") || path.Base(p) == "kustomization.yaml" {
			continue
		}
		spec, err := ParseApp(b)
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(path.Base(p), ".yaml")
		a := &AppInfo{Name: name, KName: spec.Name, KFile: p, Spec: spec, Replicas: -1}
		if a.KName == "" {
			a.KName = name
		}
		fillKustomization(a, b)
		fillWorkload(a, s)
		out = append(out, a)
	}
	return out
}

func fillKustomization(a *AppInfo, b []byte) {
	docs, _ := decodeDocs(b)
	if len(docs) == 0 {
		return
	}
	d := docs[0]
	for _, x := range list(dig(d, "spec", "dependsOn")) {
		a.DependsOn = append(a.DependsOn, str(dig(x, "name")))
	}
	a.Components = strList(dig(d, "spec", "components"))
	a.Namespace = str(dig(d, "spec", "postBuild", "substitute", "NAMESPACE"))
	a.Port = str(dig(d, "spec", "postBuild", "substitute", "PORT"))
	if p := strings.TrimPrefix(a.Spec.Path, "./"); strings.HasPrefix(p, "apps/") && strings.Count(p, "/") == 1 {
		a.Dir = p
	}
}

func fillWorkload(a *AppInfo, s *Snapshot) {
	if a.Dir == "" {
		return
	}
	kb, ok := s.Files[a.Dir+"/kustomization.yaml"]
	if !ok {
		return
	}
	kd, _ := decodeDocs(kb)
	if len(kd) == 0 {
		return
	}
	nDeploy, nSvc := 0, 0
	for _, r := range strList(dig(kd[0], "resources")) {
		fp := resolve(a.Dir, r)
		b, ok := s.Files[fp]
		if !ok {
			continue
		}
		docs, _ := decodeDocs(b)
		for _, d := range docs {
			switch kind(d) {
			case "Deployment":
				nDeploy++
				a.DeployPath = fp
				if r, ok := dig(d, "spec", "replicas").(int); ok {
					a.Replicas = r
				} else {
					a.Replicas = 1
				}
				a.Image = str(dig(d, "spec", "template", "spec", "containers", 0, "image"))
				for _, cp := range list(dig(d, "spec", "template", "spec", "containers", 0, "ports")) {
					if n := portNum(str(dig(cp, "containerPort"))); n > 0 && (a.ContainerPort == 0 || str(dig(cp, "name")) == "http") {
						a.ContainerPort = n
					}
				}
				a.ImageRepo, a.Tag, a.Digest = splitImage(a.Image)
				readImageMarker(a, b)
			case "Service":
				nSvc++
				a.ServiceName = str(dig(d, "metadata", "name"))
				a.ServicePort = portNum(str(dig(d, "spec", "ports", 0, "port")))
			case "PersistentVolumeClaim":
				a.PVCs = append(a.PVCs, str(dig(d, "metadata", "name")))
			case "ExternalSecret":
				si := SecretInfo{Name: str(dig(d, "metadata", "name")), Target: str(dig(d, "spec", "target", "name")), File: fp}
				if si.Target == "" {
					si.Target = str(dig(d, "metadata", "name"))
				}
				for _, e := range list(dig(d, "spec", "data")) {
					si.Keys = append(si.Keys, str(dig(e, "remoteRef", "key")))
				}
				for _, e := range list(dig(d, "spec", "dataFrom")) {
					if k := str(dig(e, "extract", "key")); k != "" {
						si.Keys = append(si.Keys, k)
					}
				}
				a.Secrets = append(a.Secrets, si)
			}
		}
	}
	a.Managed = nDeploy == 1 && nSvc == 1 && a.Spec.Mode != ""
}

func readImageMarker(a *AppInfo, content []byte) {
	var root yaml.Node
	if yaml.Unmarshal(content, &root) != nil {
		return
	}
	// multi-doc safety: only the first document is inspected by Unmarshal; deployments are single-doc files.
	img := findNode(&root, "spec", "template", "spec", "containers", 0, "image")
	if img == nil {
		return
	}
	if m := setterRe.FindStringSubmatch(img.LineComment); m != nil {
		a.Setter, a.Built = m[1], true
	} else if strings.Contains(img.LineComment, "pinned by switchboard") {
		a.Pinned, a.Built = true, true
	}
}

func portNum(s string) int { n, _ := strconv.Atoi(s); return n }
