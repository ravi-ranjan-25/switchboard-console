package main

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// DomainReq is the "Add domain" form: a public site on a new apex domain.
type DomainReq struct {
	Domain string `json:"domain"`
	WWW    bool   `json:"www"`
	App    string `json:"app"` // optional: serve this app on the domain
}

// SetHost rewrites postBuild.substitute.HOST in a Flux Kustomization file.
func SetHost(content []byte, host string) ([]byte, string, error) {
	if !hostRe.MatchString(host) {
		return nil, "", errors.New("invalid host")
	}
	root, err := parseRoot(content)
	if err != nil {
		return nil, "", err
	}
	n := findNode(root, "spec", "postBuild", "substitute", "HOST")
	if n == nil || n.Value == "" {
		return nil, "", errors.New("postBuild.substitute.HOST not found")
	}
	if n.Value == host {
		return nil, n.Value, errors.New("no change")
	}
	ed := newLineEditor(content)
	ln := ed.lines[n.Line-1]
	s := byteOffset(ln, n.Column)
	e := scalarEnd(ln, s, false)
	ed.lines[n.Line-1] = ln[:s] + host + ln[e:]
	out := ed.bytes()
	if sp, err := ParseApp(out); err != nil || sp.Host != host {
		return nil, "", errors.New("HOST edit failed verification")
	}
	return out, n.Value, nil
}

func wwwRedirectYAML(app *AppInfo, domain string) string {
	re := strings.ReplaceAll(domain, ".", `\.`)
	return fmt.Sprintf(`# www.%[1]s -> apex. The tunnel sends the www host to Traefik; a separate Ingress plus a redirectregex
# Middleware keeps the apex Ingress (owned by the expose-* component) untouched.
# $${1} is Flux's escape for a literal ${1} (Flux would otherwise try to substitute it).
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: www-redirect
  namespace: %[3]s
  labels:
    citadel.dev/app: %[2]s
spec:
  redirectRegex:
    regex: ^https?://www\.%[4]s/(.*)
    replacement: https://%[1]s/$${1}
    permanent: true
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: %[2]s-www
  namespace: %[3]s
  labels:
    citadel.dev/app: %[2]s
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: web,private
    traefik.ingress.kubernetes.io/router.middlewares: %[3]s-www-redirect@kubernetescrd
spec:
  ingressClassName: traefik
  rules:
    - host: www.%[1]s
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: %[5]s
                port:
                  number: %[6]d
`, domain, app.Name, app.Namespace, re, app.ServiceName, app.ServicePort)
}

func addResource(kust []byte, file string) ([]byte, error) {
	res, err := kustResources(kust)
	if err != nil {
		return nil, err
	}
	if contains(res, file) {
		return nil, errors.New(file + " already listed")
	}
	ed := newLineEditor(kust)
	last := -1
	for i, ln := range ed.lines {
		if resLineRe.MatchString(ln) {
			last = i
		}
	}
	if last < 0 {
		return nil, errors.New("no resources list")
	}
	ed.lines = append(ed.lines[:last+1], append([]string{"  - " + file}, ed.lines[last+1:]...)...)
	out := ed.bytes()
	if r, err := kustResources(out); err != nil || !contains(r, file) {
		return nil, errors.New("resource edit failed verification")
	}
	return out, nil
}

var apexRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*\.[a-z]{2,63}$`)

func (d *DomainReq) normalize(cfg *Config) []FieldError {
	scopedSuffix := cfg.PrimaryDomain
	d.Domain = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d.Domain), ".")))
	var errs []FieldError
	switch {
	case !apexRe.MatchString(d.Domain) || len(d.Domain) > 253:
		errs = append(errs, FieldError{"domain", "a plain domain such as example.com (no scheme, path or wildcard)"})
	case strings.HasPrefix(d.Domain, "www."):
		errs = append(errs, FieldError{"domain", "enter the apex without www; tick \"include www\" instead"})
	case d.Domain == scopedSuffix || strings.HasSuffix(d.Domain, "."+scopedSuffix):
		errs = append(errs, FieldError{"domain", scopedSuffix + " and its subdomains are already covered by the existing zone; use New app for subdomains"})
	}
	return errs
}

// PlanDomain edits var.zones and local.tunnel_hosts and, when an app is chosen, points that app at the domain.
// Nothing is applied: the notes are the manual checklist.
func PlanDomain(s *Snapshot, cfg *Config, d *DomainReq) (*Plan, []Check, []string, []FieldError) {
	if errs := d.normalize(cfg); errs != nil {
		return nil, nil, nil, errs
	}
	c := &checker{}
	varsPath, cfPath := cfg.TofuDir+"/variables.tf", cfg.TofuDir+"/cloudflare.tf"
	apps := loadApps(s, cfg.AppsDir)
	var errs []FieldError
	bad := func(f, m string) { errs = append(errs, FieldError{f, m}) }

	c.ok("domain-syntax", "%s is a valid domain", d.Domain)
	c.ok("public-only", "public sites only: the wildcard cert, private DNS and the oauth2-proxy cookie cover .%s only, so private, tailnet and Google-gated hosts cannot live on %s", cfg.PrimaryDomain, d.Domain)

	varsB, okV := s.Files[varsPath]
	cfB, okC := s.Files[cfPath]
	if !okV || !okC {
		bad("", fmt.Sprintf("%s or %s not found in the repo", varsPath, cfPath))
		return nil, nil, nil, errs
	}
	zones, zerr := TofuZones(varsB)
	hosts, herr := TunnelHosts(cfB)
	if zerr != nil || herr != nil {
		bad("", fmt.Sprintf("cannot read the tofu files: %v %v", zerr, herr))
		return nil, nil, nil, errs
	}
	if contains(zones, d.Domain) {
		bad("domain", d.Domain+" is already in var.zones")
	}
	for h := range hosts {
		if h == d.Domain || h == "www."+d.Domain || hosts[h] == d.Domain {
			bad("domain", d.Domain+" is already in local.tunnel_hosts ("+h+")")
			break
		}
	}
	for _, a := range apps {
		if a.Spec.Host == d.Domain || a.Spec.Host == "www."+d.Domain {
			bad("domain", a.Name+" already serves "+a.Spec.Host)
		}
	}
	if len(errs) > 0 {
		return nil, nil, nil, errs
	}
	c.ok("no-duplicate", "%s is not in var.zones, local.tunnel_hosts or any app HOST", d.Domain)

	p := &Plan{Kind: "domain", App: "domain:" + d.Domain}
	msg := "switchboard: add domain " + d.Domain
	if d.WWW {
		msg += " (+www)"
	}
	p.Message = msg
	hostsToAdd := []string{d.Domain}
	if d.WWW {
		hostsToAdd = append(hostsToAdd, "www."+d.Domain)
	}
	add := func(pth string, f func([]byte) ([]byte, error)) bool {
		ch, err := newUpdate(s, pth, f)
		if err != nil {
			bad("", err.Error())
			return false
		}
		p.Changes = append(p.Changes, ch)
		return true
	}
	// Tofu files first: inert until the operator applies them.
	add(varsPath, func(b []byte) ([]byte, error) { return AddZone(b, d.Domain) })
	add(cfPath, func(b []byte) ([]byte, error) { return AddTunnelHosts(b, d.Domain, hostsToAdd...) })

	notes := []string{}
	var app *AppInfo
	if d.App != "" {
		var err error
		if app, err = manageable(s, cfg, d.App); err != nil {
			bad("app", err.Error())
		}
	}
	if app != nil {
		c.ok("app", "%s can be pointed at the domain", app.Name)
		if d.WWW {
			rf := app.Dir + "/www-redirect.yaml"
			if s.Has(rf) || strings.Contains(strings.Join(resourcesOf(s, app), " "), "www-redirect") {
				bad("www", app.Name+" already has a www redirect")
			} else if app.ServiceName == "" || app.ServicePort == 0 {
				bad("www", "cannot read the Service name/port of "+app.Name)
			} else {
				p.Changes = append(p.Changes, Change{Path: rf, Action: ActCreate, New: []byte(wwwRedirectYAML(app, d.Domain))})
				add(app.Dir+"/kustomization.yaml", func(b []byte) ([]byte, error) { return addResource(b, "www-redirect.yaml") })
			}
		}
		// The app's Kustomization LAST: from that commit on Flux serves the new host.
		old := app.Spec.Host
		add(app.KFile, func(b []byte) ([]byte, error) {
			out, _, err := SetHost(b, d.Domain)
			if err != nil {
				return nil, err
			}
			if sp, _ := ParseApp(out); sp != nil && sp.Mode != ModePublic {
				out, _, err = EditApp(out, ModePublic, "")
			}
			return out, err
		})
		if app.Spec.Mode != ModePublic {
			notes = append(notes, fmt.Sprintf("%s is %s today and becomes public: this is the only mode that works on a new domain.", app.Name, app.Spec.Mode))
		}
		if old != "" {
			notes = append(notes, fmt.Sprintf("%s moves from %s to %s. The old host stops working when Flux applies it.", app.Name, old, d.Domain))
		}
	} else if d.App == "" {
		c.ok("app", "no app chosen: only the zone and tunnel hostnames are added")
		notes = append(notes, "No app chosen: nothing serves this domain yet. Later, pick it with New app, or edit the app's HOST.")
	}
	if len(errs) > 0 {
		return nil, nil, nil, errs
	}
	after := p.After(s)
	if z, err := TofuZones(after.Files[varsPath]); err == nil && contains(z, d.Domain) {
		c.ok("tofu-zones", "var.zones lists %s", d.Domain)
	}
	if h, err := TunnelHosts(after.Files[cfPath]); err == nil {
		for _, x := range hostsToAdd {
			c.check(h[x] == d.Domain, "tofu-tunnel-hosts", x+" -> zone "+d.Domain, x+" missing from local.tunnel_hosts")
		}
	}
	if app != nil {
		c.out = append(c.out, CheckApp(after, cfg, app.Name)...)
	}
	notes = append([]string{
		"Nothing is applied. After the commit, do these yourself:",
		"1. In Cloudflare add the site " + d.Domain + " (Free plan) and change the nameservers at your registrar to the two Cloudflare gives you. Wait until the zone shows Active.",
		"2. Run: make tofu-apply STAGE=3 TOFU_ARGS=\"-var phase2=true\" CONFIRM=yes",
		"3. Wait for Flux to reconcile the app (Switchboard asks Flux to fetch right after the commit).",
		"Do not run step 2 before the zone is Active: the zone lookup fails and the apply stops.",
	}, notes...)
	return p, c.out, notes, nil
}

func resourcesOf(s *Snapshot, a *AppInfo) []string {
	r, _ := kustResources(s.Files[a.Dir+"/kustomization.yaml"])
	for i := range r {
		r[i] = path.Base(r[i])
	}
	return r
}
