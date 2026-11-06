package main

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Network tab: which app may call which app. Namespaces are default-deny for ingress; each app opens its own port to Traefik
// and the tailnet (components/app-netpol). An allowed app-to-app edge is ONE generated NetworkPolicy per destination app,
// apps/<dst>/netpol-from-apps.yaml, with one ingress rule per caller. NetworkPolicies are additive, so it only widens.
// The port is the destination's CONTAINER port (what NetworkPolicy matches), not the Service port.

const netFile = "netpol-from-apps.yaml"

type NetApp struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Mode      string `json:"mode"`
	Port      int    `json:"port"`
	Editable  bool   `json:"editable"` // standard, non-protected app
	Protected bool   `json:"protected"`
	Isolated  bool   `json:"isolated"`
	Egress    bool   `json:"egress_restricted"` // uses components/egress-restricted
}

type NetEdge struct {
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	Port int    `json:"port"`
	Kind string `json:"kind"` // managed (generated here) | custom (hand-written policy) | db (db-client component)
	File string `json:"file,omitempty"`
}

type NetworkView struct {
	Apps     []NetApp  `json:"apps"`
	Edges    []NetEdge `json:"edges"`
	Isolated []string  `json:"isolated_namespaces"`
}

func (s *Snapshot) netpolPath(a *AppInfo) string { return a.Dir + "/" + netFile }

// policyEdges extracts "src may reach dst" from the NetworkPolicy documents of one app.
func policyEdges(a *AppInfo, file string, d Doc, kind string) []NetEdge {
	if str(dig(d, "kind")) != "NetworkPolicy" || str(dig(d, "spec", "podSelector", "matchLabels", "citadel.dev/app")) != a.Name {
		return nil
	}
	if !contains(strList(dig(d, "spec", "policyTypes")), "Ingress") {
		return nil
	}
	var out []NetEdge
	for _, rule := range list(dig(d, "spec", "ingress")) {
		port := portNum(str(dig(rule, "ports", 0, "port")))
		for _, f := range list(dig(rule, "from")) {
			if src := str(dig(f, "podSelector", "matchLabels", "citadel.dev/app")); src != "" {
				out = append(out, NetEdge{Src: src, Dst: a.Name, Port: port, Kind: kind, File: file})
			}
		}
	}
	return out
}

// appPolicyDocs returns the documents of every file the app's kustomization lists.
func appPolicyDocs(s *Snapshot, a *AppInfo) map[string][]Doc {
	out := map[string][]Doc{}
	kd, _ := decodeDocs(s.Files[a.Dir+"/kustomization.yaml"])
	if len(kd) == 0 {
		return out
	}
	for _, r := range strList(dig(kd[0], "resources")) {
		fp := resolve(a.Dir, r)
		if ds, err := decodeDocs(s.Files[fp]); err == nil {
			out[fp] = ds
		}
	}
	return out
}

func isNetNamespaceIsolated(cfg *Config, ns string) bool { return contains(cfg.IsolatedNamespaces, ns) }

// BuildNetwork derives apps and edges from the repo snapshot (no cluster access).
func BuildNetwork(s *Snapshot, cfg *Config) *NetworkView {
	v := &NetworkView{Apps: []NetApp{}, Edges: []NetEdge{}, Isolated: cfg.IsolatedNamespaces}
	apps := loadApps(s, cfg.AppsDir)
	for _, a := range apps {
		if !a.Managed {
			continue
		}
		prot := protectedApp(cfg, a)
		v.Apps = append(v.Apps, NetApp{Name: a.Name, Namespace: a.Namespace, Mode: a.Spec.Mode, Port: a.ContainerPort,
			Editable: !prot && a.ContainerPort > 0, Protected: prot, Isolated: isNetNamespaceIsolated(cfg, a.Namespace),
			Egress: containsAny(a.Components, "egress-restricted")})
		for fp, docs := range appPolicyDocs(s, a) {
			kind := "custom"
			if fp == s.netpolPath(a) {
				kind = "managed"
			}
			for _, d := range docs {
				v.Edges = append(v.Edges, policyEdges(a, fp, d, kind)...)
			}
		}
		if containsAny(a.Components, "components/db-client") {
			v.Edges = append(v.Edges, NetEdge{Src: a.Name, Dst: "db", Port: 5432, Kind: "db"})
		}
	}
	sort.Slice(v.Apps, func(i, j int) bool { return v.Apps[i].Name < v.Apps[j].Name })
	sort.Slice(v.Edges, func(i, j int) bool {
		if v.Edges[i].Dst != v.Edges[j].Dst {
			return v.Edges[i].Dst < v.Edges[j].Dst
		}
		return v.Edges[i].Src < v.Edges[j].Src
	})
	return v
}

func containsAny(l []string, sub string) bool {
	for _, x := range l {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

// managedCallers returns the callers recorded in the generated file of dst (src app -> its namespace).
func managedCallers(s *Snapshot, a *AppInfo) map[string]string {
	out := map[string]string{}
	ds, _ := decodeDocs(s.Files[s.netpolPath(a)])
	for _, d := range ds {
		for _, rule := range list(dig(d, "spec", "ingress")) {
			for _, f := range list(dig(rule, "from")) {
				if src := str(dig(f, "podSelector", "matchLabels", "citadel.dev/app")); src != "" {
					ns := str(dig(f, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"))
					if ns == "" {
						ns = a.Namespace
					}
					out[src] = ns
				}
			}
		}
	}
	return out
}

func genFromApps(dst *AppInfo, callers map[string]string) string {
	names := make([]string, 0, len(callers))
	for n := range callers {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	fmt.Fprintf(&sb, `# Generated by Switchboard (Network tab). One ingress rule per allowed caller; change it from the console, not by hand.
# NetworkPolicies are additive: this only widens access to %[1]s, it never narrows what other policies allow.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %[1]s-from-apps
  namespace: %[2]s
spec:
  podSelector:
    matchLabels:
      citadel.dev/app: %[1]s
  policyTypes:
    - Ingress
  ingress:
`, dst.Name, dst.Namespace)
	for _, n := range names {
		fmt.Fprintf(&sb, `    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: %s
          podSelector:
            matchLabels:
              citadel.dev/app: %s
      ports:
        - protocol: TCP
          port: %d
`, callers[n], n, dst.ContainerPort)
	}
	return sb.String()
}

// netEndpoints validates both ends of an edge.
func netEndpoints(s *Snapshot, cfg *Config, src, dst string) (*AppInfo, *AppInfo, error) {
	if src == dst {
		return nil, nil, errors.New("an app already reaches itself")
	}
	sa, err := manageable(s, cfg, src)
	if err != nil {
		return nil, nil, fmt.Errorf("caller: %w", err)
	}
	da, err := manageable(s, cfg, dst)
	if err != nil {
		return nil, nil, fmt.Errorf("callee: %w", err)
	}
	if da.ContainerPort <= 0 {
		return nil, nil, fmt.Errorf("cannot read the container port of %s", dst)
	}
	if sa.Namespace != da.Namespace && (isNetNamespaceIsolated(cfg, sa.Namespace) || isNetNamespaceIsolated(cfg, da.Namespace)) {
		return nil, nil, fmt.Errorf("%s and %s are in different namespaces and one of them (%s) is kept isolated", src, dst, join(cfg.IsolatedNamespaces))
	}
	if sa.Namespace != da.Namespace && containsAny(sa.Components, "egress-restricted") {
		return nil, nil, fmt.Errorf("%s restricts its egress and is in another namespace: not supported yet (it would also need an egress rule)", src)
	}
	return sa, da, nil
}

func PlanNetAllow(s *Snapshot, cfg *Config, src, dst string) (*Plan, error) {
	sa, da, err := netEndpoints(s, cfg, src, dst)
	if err != nil {
		return nil, err
	}
	for _, e := range BuildNetwork(s, cfg).Edges {
		if e.Src == src && e.Dst == dst {
			return nil, fmt.Errorf("%s may already call %s (%s rule)", src, dst, e.Kind)
		}
	}
	fp := s.netpolPath(da)
	p := &Plan{Kind: "net", App: dst, Message: fmt.Sprintf("switchboard: allow %s -> %s:%d", src, dst, da.ContainerPort)}
	if old, ok := s.Files[fp]; ok {
		c, err := newUpdate(s, fp, func(b []byte) ([]byte, error) {
			cur := callersFrom(b, da)
			cur[src] = sa.Namespace
			return []byte(genFromApps(da, cur)), nil
		})
		_ = old
		if err != nil {
			return nil, err
		}
		p.Changes = append(p.Changes, c)
		return p, nil
	}
	p.Changes = append(p.Changes, Change{Path: fp, Action: ActCreate, New: []byte(genFromApps(da, map[string]string{src: sa.Namespace}))})
	c, err := newUpdate(s, da.Dir+"/kustomization.yaml", func(b []byte) ([]byte, error) {
		if r, _ := kustResources(b); contains(r, netFile) {
			return b, nil
		}
		return addResource(b, netFile)
	})
	if err != nil {
		return nil, err
	}
	p.Changes = append(p.Changes, c) // listing LAST: only then does Flux apply the policy
	return p, nil
}

func callersFrom(b []byte, a *AppInfo) map[string]string {
	s := &Snapshot{Paths: map[string]string{}, Files: map[string][]byte{a.Dir + "/" + netFile: b}}
	return managedCallers(s, a)
}

func PlanNetRevoke(s *Snapshot, cfg *Config, src, dst string) (*Plan, error) {
	_, da, err := netEndpoints(s, cfg, src, dst)
	if err != nil {
		return nil, err
	}
	cur := managedCallers(s, da)
	if _, ok := cur[src]; !ok {
		for _, e := range BuildNetwork(s, cfg).Edges {
			if e.Src == src && e.Dst == dst {
				return nil, fmt.Errorf("the rule allowing %s -> %s is hand-written (%s); Switchboard only revokes rules it generated", src, dst, e.File)
			}
		}
		return nil, fmt.Errorf("%s is not allowed to call %s", src, dst)
	}
	fp := s.netpolPath(da)
	p := &Plan{Kind: "net", App: dst, Message: fmt.Sprintf("switchboard: revoke %s -> %s", src, dst)}
	if len(cur) == 1 {
		// last caller: unlist first (Flux prunes the policy), then delete the file
		c, err := newUpdate(s, da.Dir+"/kustomization.yaml", func(b []byte) ([]byte, error) { return removeResource(b, netFile) })
		if err != nil {
			return nil, err
		}
		p.Changes = append(p.Changes, c, Change{Path: fp, Action: ActDelete, Old: s.Files[fp]})
		return p, nil
	}
	c, err := newUpdate(s, fp, func(b []byte) ([]byte, error) {
		m := callersFrom(b, da)
		delete(m, src)
		return []byte(genFromApps(da, m)), nil
	})
	if err != nil {
		return nil, err
	}
	p.Changes = append(p.Changes, c)
	return p, nil
}

func removeResource(content []byte, file string) ([]byte, error) {
	ed := newLineEditor(content)
	for i, ln := range ed.lines {
		if m := resLineRe.FindStringSubmatch(ln); m != nil && path.Base(m[1]) == file {
			ed.lines = append(ed.lines[:i], ed.lines[i+1:]...)
			out := ed.bytes()
			if r, err := kustResources(out); err != nil || contains(r, file) {
				return nil, errors.New("resource removal failed verification")
			}
			return out, nil
		}
	}
	return content, nil // already unlisted
}

// CheckNetwork validates the generated policy of dst in the end state.
func CheckNetwork(s *Snapshot, cfg *Config, dst string) []Check {
	c := &checker{}
	a := loadAppFrom(s, cfg, dst)
	if a == nil || a.Dir == "" {
		return c.out
	}
	fp := s.netpolPath(a)
	raw, ok := s.Files[fp]
	if !ok {
		c.ok("network", "no generated policy for %s (nothing allowed through the console)", dst)
		return c.out
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		c.fail("network", "%s does not parse: %v", netFile, err)
		return c.out
	}
	docs, _ := decodeDocs(raw)
	if len(docs) != 1 || str(dig(docs[0], "kind")) != "NetworkPolicy" || str(dig(docs[0], "metadata", "namespace")) != a.Namespace ||
		str(dig(docs[0], "spec", "podSelector", "matchLabels", "citadel.dev/app")) != dst {
		c.fail("network", "%s must be one NetworkPolicy in %s selecting citadel.dev/app=%s", netFile, a.Namespace, dst)
		return c.out
	}
	r, _ := kustResources(s.Files[a.Dir+"/kustomization.yaml"])
	c.check(contains(r, netFile), "network", netFile+" is listed in the app kustomization", netFile+" exists but is not listed, Flux will not apply it")
	apps := map[string]*AppInfo{}
	for _, x := range loadApps(s, cfg.AppsDir) {
		apps[x.Name] = x
	}
	for _, rule := range list(dig(docs[0], "spec", "ingress")) {
		src := str(dig(rule, "from", 0, "podSelector", "matchLabels", "citadel.dev/app"))
		sa := apps[src]
		switch {
		case sa == nil || !sa.Managed:
			c.fail("network", "caller %q is not a standard app", src)
		case protectedApp(cfg, sa):
			c.fail("network", "caller %s is protected", src)
		case src == dst:
			c.fail("network", "self edge")
		case sa.Namespace != a.Namespace && (isNetNamespaceIsolated(cfg, sa.Namespace) || isNetNamespaceIsolated(cfg, a.Namespace)):
			c.fail("network", "%s -> %s crosses an isolated namespace", src, dst)
		case portNum(str(dig(rule, "ports", 0, "port"))) != a.ContainerPort:
			c.fail("network", "%s -> %s: port must be the container port %d", src, dst, a.ContainerPort)
		default:
			c.ok("network", "%s -> %s:%d", src, dst, a.ContainerPort)
		}
	}
	return c.out
}
