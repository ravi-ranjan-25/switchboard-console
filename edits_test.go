package main

import (
	"strings"
	"testing"
)

func seedText(t *testing.T, p string) []byte {
	t.Helper()
	s, _ := seedSnapshot(t)
	b, ok := s.Files[p]
	if !ok {
		t.Fatalf("no seed file %s", p)
	}
	return b
}

func TestRegisterAndUnregister(t *testing.T) {
	src := seedText(t, "clusters/keep/apps/kustomization.yaml")
	out, err := RegisterApp(src, "notes", "personal")
	if err != nil {
		t.Fatal(err)
	}
	// lands at the end of the personal section, before "# client"
	i, j, k := strings.Index(string(out), "  - notes.yaml"), strings.Index(string(out), "# client"), strings.Index(string(out), "  - workbench.yaml")
	if !(k < i && i < j) {
		t.Errorf("position wrong:\n%s", out)
	}
	if _, err := RegisterApp(out, "notes", "personal"); err == nil {
		t.Error("double registration")
	}
	out2, err := RegisterApp(src, "acme", "client")
	if err != nil || strings.Index(string(out2), "  - acme.yaml") < strings.Index(string(out2), "  - clientapp.yaml") || strings.Index(string(out2), "  - acme.yaml") > strings.Index(string(out2), "# image automation") {
		t.Errorf("client section: %v\n%s", err, out2)
	}
	back, err := UnregisterApp(out, "notes")
	if err != nil || string(back) != string(src) {
		t.Errorf("round trip differs: %v", err)
	}
	if _, err := UnregisterApp(src, "ghost"); err == nil {
		t.Error("unregister unknown")
	}
	// unknown section falls back to the end of the list
	out3, err := RegisterApp(src, "zeta", "nosuch")
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out3)), "- zeta.yaml") {
		t.Errorf("fallback: %v\n%s", err, out3)
	}
}

func TestImageAutomationAddRemove(t *testing.T) {
	src := seedText(t, "clusters/keep/apps/image-automation.yaml")
	n := builtApp()
	n.Normalize(testConfig(""))
	out, err := AddImageAutomation(src, n)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), string(src[:200])) {
		t.Error("header must stay untouched")
	}
	if i, j := strings.Index(string(out), "name: notes"), strings.Index(string(out), "kind: ImageUpdateAutomation"); i < 0 || i > j {
		t.Error("new docs must precede ImageUpdateAutomation")
	}
	if _, err := AddImageAutomation(out, n); err == nil {
		t.Error("duplicate must be refused")
	}
	back, err := RemoveImageAutomation(out, "notes")
	if err != nil || string(back) != string(src) {
		t.Errorf("round trip differs: %v\n%s", err, back)
	}
	if _, err := RemoveImageAutomation(src, "ghost"); err == nil {
		t.Error("remove unknown")
	}
}

func TestHomepageAddRemove(t *testing.T) {
	src := seedText(t, "apps/homepage/configmap.yaml")
	n := builtApp()
	n.Normalize(testConfig(""))
	out, err := AddHomepage(src, n)
	if err != nil {
		t.Fatal(err)
	}
	// inside the Apps group, after feedBack and before "- Tools:"
	if i, j, k := strings.Index(string(out), "feedBack"), strings.Index(string(out), "- notes:"), strings.Index(string(out), "    - Tools:"); !(i < j && j < k) {
		t.Errorf("group position:\n%s", out)
	}
	if !strings.Contains(strings.Join(HomepageHrefs(out), " "), "https://notes.example.com") {
		t.Error("href not parsed back")
	}
	if _, err := AddHomepage(out, n); err == nil {
		t.Error("duplicate")
	}
	back, err := RemoveHomepage(out, "notes.example.com")
	if err != nil || string(back) != string(src) {
		t.Errorf("round trip differs: %v", err)
	}
	n.Group, n.Title = "Labs", "Lab"
	n.Host = "lab.example.com"
	if out, err := AddHomepage(src, n); err != nil || !strings.Contains(strings.Join(HomepageHrefs(out), " "), "lab.example.com") {
		t.Errorf("new group: %v", err)
	}
}

func TestSetReplicasKeepsEverythingElse(t *testing.T) {
	src := seedText(t, "apps/excalidraw/deployment.yaml")
	out, old, err := SetReplicas(src, 0)
	if err != nil || old != 1 {
		t.Fatal(old, err)
	}
	if string(out) != strings.Replace(string(src), "replicas: 1", "replicas: 0", 1) {
		t.Error("only the replicas number may change")
	}
	if _, _, err := SetReplicas(out, 0); err == nil {
		t.Error("no-op")
	}
	if _, _, err := SetReplicas([]byte("kind: Service\n"), 0); err == nil {
		t.Error("non-deployment")
	}
}

func TestEditImage(t *testing.T) {
	src := seedText(t, "apps/portfolio/deployment.yaml")
	pin, oldTag, err := EditImage(src, "portfolio", "20261029101500-a1b2c3d", ImagePin)
	if err != nil || oldTag != "20260101000000-0000000" {
		t.Fatal(oldTag, err)
	}
	if !strings.Contains(string(pin), "registry.example.com/portfolio:20261029101500-a1b2c3d # pinned by switchboard") {
		t.Errorf("pin line:\n%s", pin)
	}
	only := strings.Replace(string(src), `20260101000000-0000000 # {"$imagepolicy": "flux-system:portfolio"}`, "20261029101500-a1b2c3d # "+pinnedComment, 1)
	if string(pin) != only {
		t.Error("pin must change only the image line")
	}
	res, _, err := EditImage(pin, "portfolio", "", ImageResume)
	if err != nil || !strings.Contains(string(res), `20261029101500-a1b2c3d # {"$imagepolicy": "flux-system:portfolio"}`) {
		t.Errorf("resume: %v\n%s", err, res)
	}
	if _, _, err := EditImage(seedText(t, "apps/excalidraw/deployment.yaml"), "x", "1.0", ImagePin); err == nil {
		t.Error("digest-pinned image must refuse tag edits")
	}
	if _, _, err := EditImage(src, "portfolio", "bad tag", ImagePin); err == nil {
		t.Error("bad tag")
	}
	set, _, err := EditImage(src, "portfolio", "2026", ImageSetTag)
	if err != nil || !strings.Contains(string(set), `portfolio:2026 # {"$imagepolicy"`) {
		t.Errorf("set keeps marker: %v\n%s", err, set)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a, b := "a\nb\nc\nd\ne\nf\ng\nh\n", "a\nb\nc\nX\ne\nf\ng\nh\n"
	d := unifiedDiff("f", "f", a, b)
	for _, w := range []string{"--- f", "+++ f", "@@ -1,7 +1,7 @@", "-d\n", "+X\n", " c\n"} {
		if !strings.Contains(d, w) {
			t.Errorf("diff lacks %q:\n%s", w, d)
		}
	}
	if unifiedDiff("f", "f", a, a) != "" {
		t.Error("identical texts have an empty diff")
	}
	if d := unifiedDiff("/dev/null", "n", "", "x\ny\n"); !strings.Contains(d, "+x\n+y\n") {
		t.Error(d)
	}
}
