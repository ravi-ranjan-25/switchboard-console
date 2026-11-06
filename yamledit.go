package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The four modes the UI offers. Each maps 1:1 to components/expose-<mode>;
// google-restricted additionally requires an ALLOWED_EMAILS substitution.
const (
	ModePublic           = "public"
	ModeGoogle           = "google"
	ModeGoogleRestricted = "google-restricted"
	ModePrivate          = "private"
)

var allModes = []string{ModePublic, ModeGoogle, ModeGoogleRestricted, ModePrivate}

func validMode(m string) bool {
	for _, x := range allModes {
		if x == m {
			return true
		}
	}
	return false
}

func componentFor(mode string) string { return mode }

var exposeRe = regexp.MustCompile(`expose-(public|google-restricted|google|private)\b`)

// AppSpec is what we read out of a Flux Kustomization file.
type AppSpec struct {
	Name          string // metadata.name
	Path          string // spec.path
	Component     string // the components entry matching components/expose-*
	Mode          string // derived; "" when no expose component is present
	Host          string
	AllowedEmails string
}

type ksDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Path       string   `yaml:"path"`
		Components []string `yaml:"components"`
		PostBuild  struct {
			Substitute map[string]string `yaml:"substitute"`
		} `yaml:"postBuild"`
	} `yaml:"spec"`
}

// ParseApp reads the first YAML document of a Flux Kustomization file.
func ParseApp(content []byte) (*AppSpec, error) {
	var d ksDoc
	if err := yaml.Unmarshal(content, &d); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if d.Kind != "Kustomization" {
		return nil, fmt.Errorf("kind is %q, want Kustomization", d.Kind)
	}
	a := &AppSpec{
		Name:          d.Metadata.Name,
		Path:          d.Spec.Path,
		Host:          d.Spec.PostBuild.Substitute["HOST"],
		AllowedEmails: d.Spec.PostBuild.Substitute["ALLOWED_EMAILS"],
	}
	for _, c := range d.Spec.Components {
		if m := exposeRe.FindStringSubmatch(c); m != nil && strings.Contains(c, "components/expose-") {
			a.Component = c
			a.Mode = m[1]
			break
		}
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Targeted line edit. yaml.v3 gives us line/column for every node; we use that
// only to LOCATE the bytes to change and then splice them in the original text,
// so comments, quoting, indentation and ordering everywhere else survive.

type lineEditor struct {
	lines []string // without line terminators
	eol   string
}

func newLineEditor(b []byte) *lineEditor {
	s := string(b)
	eol := "\n"
	if strings.Contains(s, "\r\n") {
		eol = "\r\n"
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return &lineEditor{lines: strings.Split(s, "\n"), eol: eol}
}

func (e *lineEditor) bytes() []byte {
	return []byte(strings.Join(e.lines, e.eol))
}

// scalarEnd returns the end offset (exclusive, in bytes) of the scalar token
// that starts at byte offset start in line.
func scalarEnd(line string, start int, flow bool) int {
	if start >= len(line) {
		return start
	}
	switch line[start] {
	case '"':
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\\' {
				i++
			} else if line[i] == '"' {
				return i + 1
			}
		}
		return len(line)
	case '\'':
		for i := start + 1; i < len(line); i++ {
			if line[i] == '\'' {
				if i+1 < len(line) && line[i+1] == '\'' {
					i++
					continue
				}
				return i + 1
			}
		}
		return len(line)
	}
	end := len(line)
	for i := start; i < len(line); i++ {
		c := line[i]
		if c == '#' && i > start && (line[i-1] == ' ' || line[i-1] == '\t') {
			end = i
			break
		}
		if flow && (c == ',' || c == '}' || c == ']') {
			end = i
			break
		}
	}
	return start + len(strings.TrimRight(line[start:end], " \t"))
}

// byteOffset converts a yaml.v3 1-based column (counted in characters) to a byte offset.
func byteOffset(line string, col int) int {
	n := 0
	for i := range line {
		n++
		if n == col {
			return i
		}
	}
	return len(line)
}

func mapGet(m *yaml.Node, key string) (k, v *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}

var emailValueRe = regexp.MustCompile(`^[^"\\\s]*$`)

// EditApp returns content switched to mode. emails is only used for
// google-restricted. It returns the old mode too.
func EditApp(content []byte, mode, emails string) (out []byte, oldMode string, err error) {
	if !validMode(mode) {
		return nil, "", fmt.Errorf("invalid mode %q", mode)
	}
	cur, err := ParseApp(content)
	if err != nil {
		return nil, "", err
	}
	if cur.Component == "" {
		return nil, "", errors.New("no components/expose-* entry in file; not switchable")
	}
	if mode == ModeGoogleRestricted && !emailValueRe.MatchString(emails) {
		return nil, "", errors.New("emails contain unsafe characters")
	}

	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, "", err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, "", errors.New("empty document")
	}
	spec := func() *yaml.Node { _, v := mapGet(root.Content[0], "spec"); return v }()
	_, comps := mapGet(spec, "components")
	if comps == nil || comps.Kind != yaml.SequenceNode {
		return nil, "", errors.New("spec.components is not a list")
	}
	_, pb := mapGet(spec, "postBuild")
	_, sub := mapGet(pb, "substitute")

	ed := newLineEditor(content)

	// Each edit is a (line, fn) operation; apply bottom-up so earlier line numbers stay valid.
	type op struct {
		line int // 0-based
		fn   func()
	}
	var ops []op

	// 1. components line: swap only the expose-<x> word inside the scalar token.
	for _, c := range comps.Content {
		if c.Kind != yaml.ScalarNode || !strings.Contains(c.Value, "components/expose-") || !exposeRe.MatchString(c.Value) {
			continue
		}
		c := c
		ops = append(ops, op{c.Line - 1, func() {
			ln := ed.lines[c.Line-1]
			s := byteOffset(ln, c.Column)
			e := scalarEnd(ln, s, comps.Style&yaml.FlowStyle != 0)
			tok := exposeRe.ReplaceAllString(ln[s:e], "expose-"+componentFor(mode))
			ed.lines[c.Line-1] = ln[:s] + tok + ln[e:]
		}})
		break
	}

	// 2. ALLOWED_EMAILS.
	var kn, vn *yaml.Node
	if sub != nil {
		kn, vn = mapGet(sub, "ALLOWED_EMAILS")
	}
	flow := sub != nil && sub.Style&yaml.FlowStyle != 0
	switch {
	case mode == ModeGoogleRestricted && vn != nil:
		ops = append(ops, op{vn.Line - 1, func() {
			ln := ed.lines[vn.Line-1]
			s := byteOffset(ln, vn.Column)
			e := scalarEnd(ln, s, flow)
			ed.lines[vn.Line-1] = ln[:s] + `"` + emails + `"` + ln[e:]
		}})
	case mode == ModeGoogleRestricted:
		if sub == nil || sub.Kind != yaml.MappingNode || len(sub.Content) == 0 {
			return nil, "", errors.New("postBuild.substitute is missing or empty; cannot add ALLOWED_EMAILS")
		}
		lastK, lastV := sub.Content[len(sub.Content)-2], sub.Content[len(sub.Content)-1]
		if lastV.Kind != yaml.ScalarNode {
			return nil, "", errors.New("unsupported postBuild.substitute layout")
		}
		if flow {
			ops = append(ops, op{lastV.Line - 1, func() {
				ln := ed.lines[lastV.Line-1]
				s := byteOffset(ln, lastV.Column)
				e := scalarEnd(ln, s, true)
				rest := ln[e:]
				i := strings.Index(rest, "}")
				if i < 0 {
					return // multi-line flow map; handled by guard below
				}
				ed.lines[lastV.Line-1] = ln[:e] + `, ALLOWED_EMAILS: "` + emails + `"` + rest
			}})
		} else {
			ops = append(ops, op{lastV.Line - 1, func() {
				indent := strings.Repeat(" ", lastK.Column-1)
				idx := lastV.Line // insert after this (0-based idx = line number)
				ins := indent + `ALLOWED_EMAILS: "` + emails + `"`
				ed.lines = append(ed.lines[:idx], append([]string{ins}, ed.lines[idx:]...)...)
			}})
		}
	case vn != nil && flow:
		// Removing a key from a flow map in place is fragile; blank it instead
		// (empty means unrestricted).
		ops = append(ops, op{vn.Line - 1, func() {
			ln := ed.lines[vn.Line-1]
			s := byteOffset(ln, vn.Column)
			e := scalarEnd(ln, s, true)
			ed.lines[vn.Line-1] = ln[:s] + `""` + ln[e:]
		}})
	case vn != nil:
		if kn.Line != vn.Line {
			return nil, "", errors.New("unsupported ALLOWED_EMAILS layout")
		}
		ops = append(ops, op{kn.Line - 1, func() {
			ed.lines = append(ed.lines[:kn.Line-1], ed.lines[kn.Line:]...)
		}})
	}

	sort.SliceStable(ops, func(i, j int) bool { return ops[i].line > ops[j].line })
	for _, o := range ops {
		o.fn()
	}
	out = ed.bytes()

	// Verify: the result must parse and describe exactly what was asked.
	chk, err := ParseApp(out)
	if err != nil {
		return nil, "", fmt.Errorf("edit produced invalid yaml: %w", err)
	}
	if chk.Mode != mode {
		return nil, "", fmt.Errorf("edit verification failed: got mode %q want %q", chk.Mode, mode)
	}
	if mode == ModeGoogleRestricted && chk.AllowedEmails != emails {
		return nil, "", errors.New("edit verification failed: ALLOWED_EMAILS mismatch")
	}
	if bytes.Equal(out, content) {
		return nil, "", errors.New("no change")
	}
	return out, cur.Mode, nil
}
