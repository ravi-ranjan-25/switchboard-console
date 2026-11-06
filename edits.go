package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// All edits here are textual splices: they touch only the lines they name, and the result is parsed again
// to prove it says what was asked.

// ---- clusters/keep/apps/kustomization.yaml --------------------------------------------------------

var resLineRe = regexp.MustCompile(`^\s+-\s+([A-Za-z0-9._/-]+)\s*(#.*)?$`)

func kustResources(content []byte) ([]string, error) {
	docs, err := decodeDocs(content)
	if err != nil || len(docs) == 0 {
		return nil, fmt.Errorf("kustomization.yaml: %v", err)
	}
	return strList(dig(docs[0], "resources")), nil
}

// RegisterApp adds "  - <name>.yaml" to the end of the section whose comment header is "# <section>"
// (falls back to the end of the resources list).
func RegisterApp(content []byte, name, section string) ([]byte, error) {
	res, err := kustResources(content)
	if err != nil {
		return nil, err
	}
	if contains(res, name+".yaml") {
		return nil, errors.New(name + ".yaml is already registered")
	}
	ed := newLineEditor(content)
	lastRes, hdr := -1, -1
	for i, ln := range ed.lines {
		if resLineRe.MatchString(ln) {
			lastRes = i
		}
		if strings.TrimSpace(ln) == "# "+section {
			hdr = i
		}
	}
	at := lastRes
	if hdr >= 0 {
		at = hdr
		for i := hdr + 1; i < len(ed.lines); i++ {
			t := strings.TrimSpace(ed.lines[i])
			if strings.HasPrefix(t, "#") {
				break
			}
			if resLineRe.MatchString(ed.lines[i]) {
				at = i
			}
		}
	}
	if at < 0 {
		return nil, errors.New("no resources list found")
	}
	ins := "  - " + name + ".yaml"
	ed.lines = append(ed.lines[:at+1], append([]string{ins}, ed.lines[at+1:]...)...)
	out := ed.bytes()
	if r, err := kustResources(out); err != nil || !contains(r, name+".yaml") {
		return nil, errors.New("registration edit failed verification")
	}
	return out, nil
}

func UnregisterApp(content []byte, name string) ([]byte, error) {
	ed := newLineEditor(content)
	for i, ln := range ed.lines {
		if m := resLineRe.FindStringSubmatch(ln); m != nil && m[1] == name+".yaml" {
			ed.lines = append(ed.lines[:i], ed.lines[i+1:]...)
			out := ed.bytes()
			if r, err := kustResources(out); err != nil || contains(r, name+".yaml") {
				return nil, errors.New("unregister edit failed verification")
			}
			return out, nil
		}
	}
	return nil, errors.New(name + ".yaml is not registered")
}

// ---- clusters/keep/apps/image-automation.yaml ----------------------------------------------------

func AddImageAutomation(content []byte, n *NewApp) ([]byte, error) {
	s := string(content)
	if strings.Contains(s, "kind: ImageRepository\nmetadata:\n  name: "+n.Name+"\n") {
		return nil, errors.New("image automation for " + n.Name + " already exists")
	}
	k := strings.Index(s, "kind: ImageUpdateAutomation")
	if k < 0 {
		return nil, errors.New("no ImageUpdateAutomation document found")
	}
	cut := strings.LastIndex(s[:k], "\n---\n")
	if cut < 0 {
		return nil, errors.New("cannot locate document boundary")
	}
	out := s[:cut+5] + n.ImageAutomationDocs() + "---\n" + s[cut+5:]
	docs, err := decodeDocs([]byte(out))
	if err != nil {
		return nil, fmt.Errorf("image-automation produced invalid yaml: %w", err)
	}
	found := 0
	for _, d := range docs {
		if (kind(d) == "ImageRepository" || kind(d) == "ImagePolicy") && str(dig(d, "metadata", "name")) == n.Name {
			found++
		}
	}
	if found != 2 {
		return nil, errors.New("image-automation edit failed verification")
	}
	return []byte(out), nil
}

func RemoveImageAutomation(content []byte, app string) ([]byte, error) {
	parts := strings.Split(string(content), "\n---\n")
	var keep []string
	removed := 0
	for _, p := range parts {
		docs, err := decodeDocs([]byte(p))
		if err == nil && len(docs) == 1 && (kind(docs[0]) == "ImageRepository" || kind(docs[0]) == "ImagePolicy") &&
			str(dig(docs[0], "metadata", "name")) == app {
			removed++
			continue
		}
		keep = append(keep, p)
	}
	if removed == 0 {
		return nil, ErrNotFound
	}
	return []byte(strings.Join(keep, "\n---\n")), nil
}

// ---- apps/homepage/configmap.yaml ----------------------------------------------------------------

func indentOf(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

// servicesBlock returns [start,end) line indexes of the body of data."services.yaml".
func servicesBlock(lines []string) (int, int, error) {
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "services.yaml: |" {
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) != "" && indentOf(lines[j]) <= indentOf(ln) {
					end = j
					break
				}
			}
			for end > i+1 && strings.TrimSpace(lines[end-1]) == "" {
				end--
			}
			return i + 1, end, nil
		}
	}
	return 0, 0, errors.New("services.yaml block not found in the homepage ConfigMap")
}

// HomepageHrefs lists the hrefs in the embedded services.yaml.
func HomepageHrefs(content []byte) []string {
	docs, _ := decodeDocs(content)
	if len(docs) == 0 {
		return nil
	}
	var groups []map[string]any
	if yaml.Unmarshal([]byte(str(dig(docs[0], "data", "services.yaml"))), &groups) != nil {
		return nil
	}
	var out []string
	for _, g := range groups {
		for _, items := range g {
			for _, it := range list(items) {
				if m := asMap(it); m != nil {
					for _, v := range m {
						out = append(out, str(dig(v, "href")))
					}
				}
			}
		}
	}
	return out
}

func AddHomepage(content []byte, n *NewApp) ([]byte, error) {
	href := "https://" + n.Host
	if contains(HomepageHrefs(content), href) {
		return nil, errors.New("homepage already lists " + href)
	}
	ed := newLineEditor(content)
	a, b, err := servicesBlock(ed.lines)
	if err != nil {
		return nil, err
	}
	entry := strings.Split(strings.TrimSuffix(n.HomepageEntry(), "\n"), "\n")
	grp := "    - " + n.Group + ":"
	at := -1
	for i := a; i < b; i++ {
		if strings.TrimRight(ed.lines[i], " ") == grp {
			at = i
			for j := i + 1; j < b && (strings.TrimSpace(ed.lines[j]) == "" || indentOf(ed.lines[j]) > 4); j++ {
				if strings.TrimSpace(ed.lines[j]) != "" {
					at = j
				}
			}
		}
	}
	var ins []string
	if at < 0 {
		at = b - 1
		ins = append([]string{grp}, entry...)
	} else {
		ins = entry
	}
	ed.lines = append(ed.lines[:at+1], append(ins, ed.lines[at+1:]...)...)
	out := ed.bytes()
	if !contains(HomepageHrefs(out), href) {
		return nil, errors.New("homepage edit failed verification")
	}
	return out, nil
}

func RemoveHomepage(content []byte, host string) ([]byte, error) {
	ed := newLineEditor(content)
	a, b, err := servicesBlock(ed.lines)
	if err != nil {
		return nil, err
	}
	for i := a; i < b; i++ {
		if strings.TrimSpace(ed.lines[i]) != "href: https://"+host {
			continue
		}
		s := i - 1
		for s > a && !(indentOf(ed.lines[s]) == 8 && strings.HasPrefix(strings.TrimSpace(ed.lines[s]), "- ")) {
			s--
		}
		e := i + 1
		for e < b && (strings.TrimSpace(ed.lines[e]) == "" || indentOf(ed.lines[e]) > 8) {
			e++
		}
		ed.lines = append(ed.lines[:s], ed.lines[e:]...)
		out := ed.bytes()
		if contains(HomepageHrefs(out), "https://"+host) {
			return nil, errors.New("homepage removal failed verification")
		}
		return out, nil
	}
	return nil, ErrNotFound
}

// ---- Deployment edits ----------------------------------------------------------------------------

func deploymentRoot(content []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, err
	}
	if k := findNode(&root, "kind"); k == nil || k.Value != "Deployment" {
		return nil, errors.New("file is not a Deployment")
	}
	return &root, nil
}

// SetReplicas rewrites spec.replicas in place.
func SetReplicas(content []byte, replicas int) ([]byte, int, error) {
	root, err := deploymentRoot(content)
	if err != nil {
		return nil, 0, err
	}
	n := findNode(root, "spec", "replicas")
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil, 0, errors.New("spec.replicas not found in the Deployment")
	}
	old, err := strconv.Atoi(n.Value)
	if err != nil {
		return nil, 0, errors.New("spec.replicas is not a number")
	}
	if old == replicas {
		return nil, old, errors.New("no change")
	}
	ed := newLineEditor(content)
	ln := ed.lines[n.Line-1]
	s := byteOffset(ln, n.Column)
	e := scalarEnd(ln, s, false)
	ed.lines[n.Line-1] = ln[:s] + strconv.Itoa(replicas) + ln[e:]
	out := ed.bytes()
	if d, _ := decodeDocs(out); len(d) == 0 || dig(d[0], "spec", "replicas") != replicas {
		return nil, 0, errors.New("replicas edit failed verification")
	}
	return out, old, nil
}

type ImageEdit int

const (
	ImageSetTag ImageEdit = iota // new tag, marker untouched
	ImagePin                     // new tag, Flux marker replaced by the pinned note
	ImageResume                  // restore the Flux marker, tag untouched
)

// EditImage changes the first container's image line.
func EditImage(content []byte, app, tag string, how ImageEdit) (out []byte, oldTag string, err error) {
	root, err := deploymentRoot(content)
	if err != nil {
		return nil, "", err
	}
	n := findNode(root, "spec", "template", "spec", "containers", 0, "image")
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil, "", errors.New("container image not found")
	}
	repo, oldTag, digest := splitImage(n.Value)
	if digest != "" {
		return nil, "", errors.New("image is pinned by digest; rollback by tag is not possible")
	}
	if how != ImageResume && !tagRe.MatchString(tag) {
		return nil, "", errors.New("invalid tag")
	}
	if how == ImageResume {
		tag = oldTag
	}
	ed := newLineEditor(content)
	ln := ed.lines[n.Line-1]
	s := byteOffset(ln, n.Column)
	e := scalarEnd(ln, s, false)
	rest := ln[e:]
	switch how {
	case ImagePin:
		rest = " # " + pinnedComment
	case ImageResume:
		rest = fmt.Sprintf(` # {"$imagepolicy": "flux-system:%s"}`, app)
	}
	ed.lines[n.Line-1] = ln[:s] + repo + ":" + tag + rest
	out = ed.bytes()
	chk, err := deploymentRoot(out)
	if err != nil {
		return nil, "", err
	}
	if in := findNode(chk, "spec", "template", "spec", "containers", 0, "image"); in == nil || in.Value != repo+":"+tag {
		return nil, "", errors.New("image edit failed verification")
	}
	if string(out) == string(content) {
		return nil, oldTag, errors.New("no change")
	}
	return out, oldTag, nil
}

var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

// AddExternalSecretKey appends one `secretKey` / `remoteRef.key` pair to the data list of an ExternalSecret file
// (the pair format Switchboard and the hand-written files both use). Only the new lines are added.
func AddExternalSecretKey(content []byte, secretKey, vaultName string) ([]byte, error) {
	docs, err := decodeDocs(content)
	if err != nil || len(docs) != 1 || kind(docs[0]) != "ExternalSecret" {
		return nil, errors.New("not a single-document ExternalSecret")
	}
	for _, e := range list(dig(docs[0], "spec", "data")) {
		if str(dig(e, "remoteRef", "key")) == vaultName || str(dig(e, "secretKey")) == secretKey {
			return nil, errors.New("already declared in this ExternalSecret")
		}
	}
	ed := newLineEditor(content)
	start := -1
	for i, ln := range ed.lines {
		if strings.TrimRight(ln, " ") == "  data:" {
			start = i
		}
	}
	if start < 0 {
		return nil, errors.New("spec.data block not found (unsupported layout)")
	}
	end := len(ed.lines)
	for i := start + 1; i < len(ed.lines); i++ {
		t := strings.TrimSpace(ed.lines[i])
		if t != "" && !strings.HasPrefix(t, "#") && indentOf(ed.lines[i]) <= 2 {
			end = i
			break
		}
	}
	for end > start+1 && strings.TrimSpace(ed.lines[end-1]) == "" {
		end--
	}
	ins := []string{"    - secretKey: " + secretKey, "      remoteRef:", "        key: " + vaultName}
	ed.lines = append(ed.lines[:end], append(ins, ed.lines[end:]...)...)
	out := ed.bytes()
	chk, err := decodeDocs(out)
	if err != nil || len(chk) != 1 {
		return nil, errors.New("edit produced invalid yaml")
	}
	found := false
	for _, e := range list(dig(chk[0], "spec", "data")) {
		found = found || (str(dig(e, "secretKey")) == secretKey && str(dig(e, "remoteRef", "key")) == vaultName)
	}
	if !found {
		return nil, errors.New("declare edit failed verification")
	}
	return out, nil
}
