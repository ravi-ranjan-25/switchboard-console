package main

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a small unified diff (3 lines of context) between two texts using an LCS.
func unifiedDiff(oldPath, newPath, a, b string) string {
	al, bl := splitLines(a), splitLines(b)
	// LCS table; files are small (manifests), so O(n*m) is fine. Guard against absurd sizes.
	if len(al)*len(bl) > 4_000_000 {
		return "(diff too large to render)\n"
	}
	n, m := len(al), len(bl)
	t := make([][]int32, n+1)
	for i := range t {
		t[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else if t[i+1][j] >= t[i][j+1] {
				t[i][j] = t[i+1][j]
			} else {
				t[i][j] = t[i][j+1]
			}
		}
	}
	type op struct {
		c byte
		s string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			ops = append(ops, op{' ', al[i]})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			ops = append(ops, op{'-', al[i]})
			i++
		default:
			ops = append(ops, op{'+', bl[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', al[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', bl[j]})
	}
	const ctx = 3
	changed := make([]bool, len(ops))
	any := false
	for k, o := range ops {
		if o.c != ' ' {
			any = true
			for d := -ctx; d <= ctx; d++ {
				if k+d >= 0 && k+d < len(ops) {
					changed[k+d] = true
				}
			}
		}
	}
	if !any {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", oldPath, newPath)
	oldLn, newLn := 1, 1
	for k := 0; k < len(ops); {
		if !changed[k] {
			if ops[k].c != '+' {
				oldLn++
			}
			if ops[k].c != '-' {
				newLn++
			}
			k++
			continue
		}
		start := k
		oc, nc := 0, 0
		for e := k; e < len(ops) && changed[e]; e++ {
			if ops[e].c != '+' {
				oc++
			}
			if ops[e].c != '-' {
				nc++
			}
			k = e + 1
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldLn, oc, newLn, nc)
		for _, o := range ops[start:k] {
			sb.WriteByte(o.c)
			sb.WriteString(o.s)
			sb.WriteByte('\n')
		}
		oldLn += oc
		newLn += nc
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
