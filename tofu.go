package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Targeted line splices for the two OpenTofu files that list DNS zones and tunnel hostnames.

var (
	zonesDefaultRe = regexp.MustCompile(`^(\s*default\s*=\s*\[)(.*)(\]\s*)$`)
	quotedRe       = regexp.MustCompile(`"([^"]*)"`)
	hostEntryRe    = regexp.MustCompile(`^(\s*)"([^"]+)"\s*=\s*"([^"]+)"\s*(#.*)?$`)
)

// blockRange finds the line range of a block opened by a line containing opener and ended by the matching
// closing brace/bracket (by counting on lines, fine for the flat blocks used here).
func blockRange(lines []string, startAt int, opener string) (int, int, error) {
	for i := startAt; i < len(lines); i++ {
		if strings.Contains(lines[i], opener) {
			depth := 0
			for j := i; j < len(lines); j++ {
				t := stripHCLComment(lines[j])
				depth += strings.Count(t, "{") + strings.Count(t, "[") - strings.Count(t, "}") - strings.Count(t, "]")
				if j > i && depth <= 0 || (j == i && depth <= 0) {
					return i, j, nil
				}
			}
		}
	}
	return 0, 0, errors.New(opener + " not found")
}

func stripHCLComment(s string) string {
	inq := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inq = !inq
		case !inq && (s[i] == '#' || (s[i] == '/' && i+1 < len(s) && s[i+1] == '/')):
			return s[:i]
		}
	}
	return s
}

// TofuZones returns var.zones' default list.
func TofuZones(variablesTf []byte) ([]string, error) {
	lines := strings.Split(string(variablesTf), "\n")
	a, b, err := blockRange(lines, 0, `variable "zones"`)
	if err != nil {
		return nil, err
	}
	body := stripHCLComments(strings.Join(lines[a:b+1], "\n"))
	i := strings.Index(body, "default")
	if i < 0 {
		return nil, errors.New("variable zones has no default list")
	}
	lb := strings.Index(body[i:], "[")
	rb := strings.Index(body[i:], "]")
	if lb < 0 || rb < lb {
		return nil, errors.New("variable zones default is not a list")
	}
	var out []string
	for _, m := range quotedRe.FindAllStringSubmatch(body[i+lb:i+rb], -1) {
		out = append(out, m[1])
	}
	return out, nil
}

func stripHCLComments(s string) string {
	ls := strings.Split(s, "\n")
	for i := range ls {
		ls[i] = stripHCLComment(ls[i])
	}
	return strings.Join(ls, "\n")
}

// AddZone appends domain to var.zones' default list, keeping its single-line or multi-line shape.
func AddZone(variablesTf []byte, domain string) ([]byte, error) {
	zs, err := TofuZones(variablesTf)
	if err != nil {
		return nil, err
	}
	if contains(zs, domain) {
		return nil, errors.New(domain + " is already in var.zones")
	}
	lines := strings.Split(string(variablesTf), "\n")
	a, b, _ := blockRange(lines, 0, `variable "zones"`)
	done := false
	for i := a; i <= b && !done; i++ {
		if m := zonesDefaultRe.FindStringSubmatch(lines[i]); m != nil { // default = ["a", "b"]
			sep := ", "
			if strings.TrimSpace(m[2]) == "" {
				sep = ""
			}
			lines[i] = m[1] + strings.TrimRight(m[2], " ") + sep + `"` + domain + `"` + m[3]
			done = true
		} else if strings.Contains(stripHCLComment(lines[i]), "default") && strings.HasSuffix(strings.TrimSpace(stripHCLComment(lines[i])), "[") {
			// multi-line list: add before the closing bracket line
			for j := i + 1; j <= b; j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "]") {
					indent := indentOf(lines[j]) + 2
					if j-1 > i {
						indent = indentOf(lines[j-1])
						prev := strings.TrimRight(lines[j-1], " ")
						if !strings.HasSuffix(stripHCLComment(prev), ",") && !strings.HasSuffix(strings.TrimSpace(stripHCLComment(prev)), "[") {
							lines[j-1] = strings.TrimRight(stripHCLComment(prev), " ") + "," + prev[len(stripHCLComment(prev)):]
						}
					}
					ins := strings.Repeat(" ", indent) + `"` + domain + `",`
					lines = append(lines[:j], append([]string{ins}, lines[j:]...)...)
					done = true
					break
				}
			}
		}
	}
	if !done {
		return nil, errors.New("unsupported layout of var.zones default")
	}
	out := []byte(strings.Join(lines, "\n"))
	if zs2, err := TofuZones(out); err != nil || !contains(zs2, domain) || len(zs2) != len(zs)+1 {
		return nil, errors.New("zones edit failed verification")
	}
	return out, nil
}

// TunnelHosts returns hostname -> zone from local.tunnel_hosts.
func TunnelHosts(cloudflareTf []byte) (map[string]string, error) {
	lines := strings.Split(string(cloudflareTf), "\n")
	a, b, err := blockRange(lines, 0, "tunnel_hosts = {")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, ln := range lines[a+1 : b] {
		if m := hostEntryRe.FindStringSubmatch(ln); m != nil {
			out[m[2]] = m[3]
		}
	}
	return out, nil
}

// AddTunnelHosts appends hostname = zone entries and re-aligns the "=" signs of the block, as `tofu fmt` does.
func AddTunnelHosts(cloudflareTf []byte, zone string, hosts ...string) ([]byte, error) {
	cur, err := TunnelHosts(cloudflareTf)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if _, ok := cur[h]; ok {
			return nil, errors.New(h + " is already in local.tunnel_hosts")
		}
	}
	lines := strings.Split(string(cloudflareTf), "\n")
	a, b, _ := blockRange(lines, 0, "tunnel_hosts = {")
	indent := "    "
	last := a
	for i := a + 1; i < b; i++ {
		if m := hostEntryRe.FindStringSubmatch(lines[i]); m != nil {
			indent, last = m[1], i
		}
	}
	var add []string
	for _, h := range hosts {
		add = append(add, fmt.Sprintf(`%s"%s" = "%s"`, indent, h, zone))
	}
	lines = append(lines[:last+1], append(add, lines[last+1:]...)...)
	b += len(add)
	width := 0
	for i := a + 1; i < b; i++ {
		if m := hostEntryRe.FindStringSubmatch(lines[i]); m != nil && len(m[2])+2 > width {
			width = len(m[2]) + 2
		}
	}
	for i := a + 1; i < b; i++ {
		if m := hostEntryRe.FindStringSubmatch(lines[i]); m != nil {
			key := `"` + m[2] + `"`
			ln := m[1] + key + strings.Repeat(" ", width-len(key)) + ` = "` + m[3] + `"`
			if m[4] != "" {
				ln += " " + m[4]
			}
			lines[i] = ln
		}
	}
	out := []byte(strings.Join(lines, "\n"))
	chk, err := TunnelHosts(out)
	if err != nil || len(chk) != len(cur)+len(hosts) {
		return nil, errors.New("tunnel_hosts edit failed verification")
	}
	for _, h := range hosts {
		if chk[h] != zone {
			return nil, errors.New("tunnel_hosts edit failed verification")
		}
	}
	return out, nil
}
