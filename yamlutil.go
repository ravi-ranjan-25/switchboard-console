package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Doc is one decoded YAML document.
type Doc map[string]any

// decodeDocs splits a multi-document YAML file. Empty documents are skipped.
func decodeDocs(b []byte) ([]Doc, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var out []Doc
	for {
		var d Doc
		err := dec.Decode(&d)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

// dig walks maps (string keys) and lists (int indexes); a missing step returns nil.
func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			switch m := v.(type) {
			case Doc:
				v = m[k]
			case map[string]any:
				v = m[k]
			default:
				return nil
			}
		case int:
			l, ok := v.([]any)
			if !ok || k < 0 || k >= len(l) {
				return nil
			}
			v = l[k]
		}
	}
	return v
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int, int64, float64, bool:
		return fmt.Sprint(x)
	}
	return ""
}

func list(v any) []any { l, _ := v.([]any); return l }

func strList(v any) []string {
	var out []string
	for _, x := range list(v) {
		out = append(out, str(x))
	}
	return out
}

func kind(d Doc) string { return str(d["kind"]) }

// findNode walks a yaml.Node mapping/sequence tree.
func findNode(n *yaml.Node, path ...any) *yaml.Node {
	for _, p := range path {
		if n == nil {
			return nil
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			n = n.Content[0]
		}
		switch k := p.(type) {
		case string:
			_, n = mapGet(n, k)
		case int:
			if n.Kind != yaml.SequenceNode || k >= len(n.Content) {
				return nil
			}
			n = n.Content[k]
		}
	}
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	return n
}

func hasPrefixAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func parseRoot(content []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func asMap(v any) map[string]any {
	switch m := v.(type) {
	case Doc:
		return m
	case map[string]any:
		return m
	}
	return nil
}
