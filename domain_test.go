package main

import (
	"strings"
	"testing"
)

func TestAddZoneVariants(t *testing.T) {
	src := seedText(t, "infra/tofu/variables.tf")
	out, err := AddZone(src, "brand-new.io")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != strings.Replace(string(src), `default = ["example.com", "client.example.org"]`, `default = ["example.com", "client.example.org", "brand-new.io"]`, 1) {
		t.Errorf("single-line list must only grow by one element:\n%s", out)
	}
	if _, err := AddZone(out, "brand-new.io"); err == nil {
		t.Error("duplicate")
	}
	multi := "variable \"zones\" {\n  type = list(string)\n  default = [\n    \"a.com\", # first\n    \"b.com\"\n  ]\n}\n\nvariable \"other\" {\n  default = [\"x\"]\n}\n"
	out, err = AddZone([]byte(multi), "c.com")
	if err != nil {
		t.Fatal(err)
	}
	want := "    \"a.com\", # first\n    \"b.com\",\n    \"c.com\",\n  ]\n"
	if !strings.Contains(string(out), want) || !strings.Contains(string(out), `default = ["x"]`) {
		t.Errorf("multi-line:\n%s", out)
	}
	if z, _ := TofuZones(out); strings.Join(z, ",") != "a.com,b.com,c.com" {
		t.Errorf("zones %v", z)
	}
	if _, err := AddZone([]byte("variable \"x\" {}\n"), "c.com"); err == nil {
		t.Error("missing variable")
	}
}

func TestAddTunnelHostsKeepsFmtAlignment(t *testing.T) {
	src := seedText(t, "infra/tofu/cloudflare.tf")
	out, err := AddTunnelHosts(src, "brand-new.io", "brand-new.io", "www.brand-new.io")
	if err != nil {
		t.Fatal(err)
	}
	h, _ := TunnelHosts(out)
	if h["brand-new.io"] != "brand-new.io" || h["www.brand-new.io"] != "brand-new.io" || h["*.example.com"] != "example.com" || len(h) != 6 {
		t.Errorf("%v", h)
	}
	// every entry's "=" is in one column, like tofu fmt
	col := -1
	for _, ln := range strings.Split(string(out), "\n") {
		if m := hostEntryRe.FindStringSubmatch(ln); m != nil {
			c := strings.Index(ln, "=")
			if col >= 0 && c != col {
				t.Errorf("misaligned %q", ln)
			}
			col = c
		}
	}
	// outside the block nothing changes
	i := strings.Index(string(src), "tunnel_hosts = {")
	j := strings.Index(string(src), "data \"cloudflare_zone\"")
	if string(out[:i]) != string(src[:i]) || !strings.HasSuffix(string(out), string(src[j:])) {
		t.Error("text outside local.tunnel_hosts changed")
	}
	if _, err := AddTunnelHosts(out, "brand-new.io", "brand-new.io"); err == nil {
		t.Error("duplicate")
	}
	// a longer name re-pads the other lines but keeps their content
	out2, err := AddTunnelHosts(src, "a-very-long-domain-name.example", "www.a-very-long-domain-name.example")
	if err != nil {
		t.Fatal(err)
	}
	if h2, _ := TunnelHosts(out2); len(h2) != 5 || h2["example.com"] != "example.com" {
		t.Errorf("%v", h2)
	}
}

func domainReq(d string, www bool, app string) OpReq {
	return OpReq{Op: "domain", Domain: &DomainReq{Domain: d, WWW: www, App: app}}
}

func TestDomainPreviewWithApp(t *testing.T) {
	e := newEnv(t)
	code, pr := e.plan(t, domainReq("Brand-New.IO", true, "excalidraw"))
	if code != 200 || !pr.OK {
		t.Fatalf("%d %+v", code, pr)
	}
	var paths []string
	for _, f := range pr.Files {
		paths = append(paths, f.Action+" "+f.Path)
	}
	want := []string{"update infra/tofu/variables.tf", "update infra/tofu/cloudflare.tf", "create apps/excalidraw/www-redirect.yaml",
		"update apps/excalidraw/kustomization.yaml", "update clusters/keep/apps/excalidraw.yaml"}
	if strings.Join(paths, "|") != strings.Join(want, "|") {
		t.Errorf("order/files:\n%v", paths)
	}
	if !strings.Contains(pr.Files[2].Content, `regex: ^https?://www\.brand-new\.io/(.*)`) || !strings.Contains(pr.Files[2].Content, "replacement: https://brand-new.io/$${1}") ||
		!strings.Contains(pr.Files[2].Content, "name: excalidraw\n                port:\n                  number: 80") {
		t.Errorf("redirect:\n%s", pr.Files[2].Content)
	}
	d := pr.Files[4].Diff
	if !strings.Contains(d, "-      HOST: draw.example.com") || !strings.Contains(d, "+      HOST: brand-new.io") || !strings.Contains(d, "+    - ../../components/expose-public") {
		t.Errorf("app diff:\n%s", d)
	}
	notes := strings.Join(pr.Notes, "\n")
	for _, w := range []string{`make tofu-apply STAGE=3 TOFU_ARGS="-var phase2=true" CONFIRM=yes`, "Active", "nameservers", "becomes public", "old host stops"} {
		if !strings.Contains(notes, w) {
			t.Errorf("checklist lacks %q:\n%s", w, notes)
		}
	}
	reason := false
	for _, c := range pr.Checks {
		reason = reason || (c.Name == "public-only" && strings.Contains(c.Detail, "oauth2-proxy cookie"))
	}
	if !reason {
		t.Error("UI must explain why only public hosts work")
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("preview must not commit")
	}
}

func TestDomainApplyCommitsAndNeverTouchesCluster(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) { e.cl.set("excalidraw", KStatus{Ready: "True", Revision: "main@sha1:" + sha}) }
	code, evs := e.apply(t, domainReq("brand-new.io", false, "excalidraw"))
	if code != 202 || lastEvent(evs).State != "done" {
		t.Fatalf("%d %+v", code, evs)
	}
	log := e.gh.CommitLog()
	if len(log) != 3 || log[0].Path != "infra/tofu/variables.tf" || log[2].Path != "clusters/keep/apps/excalidraw.yaml" {
		t.Fatalf("%+v", log)
	}
	if !strings.Contains(log[0].Message, "switchboard: add domain brand-new.io") {
		t.Error(log[0].Message)
	}
	if !strings.Contains(e.file(t, "clusters/keep/apps/excalidraw.yaml"), "HOST: brand-new.io") || strings.Contains(e.file(t, "apps/excalidraw/kustomization.yaml"), "www-redirect") {
		t.Error("app files")
	}
}

func TestDomainWithoutAppOnlyEditsTofu(t *testing.T) {
	e := newEnv(t)
	code, evs := e.apply(t, domainReq("other-new.io", true, ""))
	if code != 202 || lastEvent(evs).State != "done" || !strings.Contains(lastEvent(evs).Msg, "manual steps") {
		t.Fatalf("%d %+v", code, evs)
	}
	log := e.gh.CommitLog()
	if len(log) != 2 {
		t.Fatalf("%+v", log)
	}
	h, _ := TunnelHosts(e.gh.MustFile("infra/tofu/cloudflare.tf"))
	if h["www.other-new.io"] != "other-new.io" {
		t.Errorf("%v", h)
	}
}

func TestDomainRefusals(t *testing.T) {
	e := newEnv(t)
	cases := map[string]OpReq{
		"scheme":        domainReq("https://brand-new.io", false, ""),
		"wildcard":      domainReq("*.brand-new.io", false, ""),
		"www prefix":    domainReq("www.brand-new.io", false, ""),
		"single label":  domainReq("localhost", false, ""),
		"scoped":        domainReq("example.com", false, ""),
		"scoped sub":    domainReq("blog.example.com", false, ""),
		"dup zone":      domainReq("client.example.org", false, ""),
		"dup via www":   domainReq("www.client.example.org", false, ""),
		"protected app": domainReq("brand-new.io", false, "switchboard"),
		"unknown app":   domainReq("brand-new.io", false, "ghost"),
		"db app":        domainReq("brand-new.io", false, "db"),
		"existing www":  domainReq("brand-new.io", true, "clientapp"),
	}
	for name, op := range cases {
		if code, pr := e.plan(t, op); code == 200 || pr.OK || len(pr.Errors) == 0 {
			t.Errorf("%s must be refused: %d %+v", name, code, pr)
		}
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("nothing may be committed")
	}
	// the domain also cannot be used twice after a successful add
	if code, _ := e.apply(t, domainReq("brand-new.io", false, "")); code != 202 {
		t.Fatal("first add")
	}
	if code, _ := e.plan(t, domainReq("brand-new.io", false, "")); code == 200 {
		t.Error("duplicate after add")
	}
}

func TestSetHost(t *testing.T) {
	src := seedText(t, "clusters/keep/apps/excalidraw.yaml")
	out, old, err := SetHost(src, "brand-new.io")
	if err != nil || old != "draw.example.com" || string(out) != strings.Replace(string(src), "HOST: draw.example.com", "HOST: brand-new.io", 1) {
		t.Errorf("%v %q", err, old)
	}
	if _, _, err := SetHost(src, "draw.example.com"); err == nil {
		t.Error("no-op")
	}
	if _, _, err := SetHost(src, "bad host"); err == nil {
		t.Error("invalid")
	}
}
