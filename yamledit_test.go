package main

import (
	"regexp"
	"strings"
	"testing"
)

var fixtures = map[string]string{
	"block-list": `# n8n editor behind Google login.
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: n8n
  namespace: flux-system
spec:
  interval: 10m   # reconcile often
  prune: true
  sourceRef: {kind: GitRepository, name: flux-system}
  path: ./apps/n8n
  dependsOn:
    - name: infra-configs
  components:
    - ../../../components/expose-google   # <- Switchboard edits this
    - ../../../components/namespace-baseline
  postBuild:
    substitute:
      APP: n8n
      NAMESPACE: personal
      PORT: "5678"
      HOST: n8n.example.com
`,
	"flow-list-flow-map": `---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: homepage, namespace: flux-system}
spec:
  interval: 10m
  sourceRef: {kind: GitRepository, name: flux-system}
  path: ./apps/homepage
  components: [../../../components/expose-private, ../../../components/app-netpol]
  postBuild: { substitute: { APP: homepage, PORT: "3000", HOST: homepage.example.com } }
`,
	"quoted-restricted-crlf": "apiVersion: kustomize.toolkit.fluxcd.io/v1\r\nkind: Kustomization\r\nmetadata:\r\n  name: planka\r\nspec:\r\n  path: ./apps/planka\r\n  components:\r\n    - \"../../../components/expose-google-restricted\"\r\n  postBuild:\r\n    substitute:\r\n      APP: planka\r\n      HOST: 'planka.example.com'\r\n      ALLOWED_EMAILS: \"a@x.com,b@y.com\"  # who may enter\r\n      SERVICE: planka-web\r\n",
	"public-with-unicode": `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
    name: portfolio   # дом
spec:
    path: ./apps/portfolio
    components:
        - ../../../components/expose-public
    postBuild:
        substitute:
            APP:  portfolio
            HOST: example.com
            # keep last
            PORT: "80"
`,
}

func lines(s string) []string { return strings.Split(s, "\n") }

// changedLines returns lines that differ (positionally after removing pure insertions/deletions).
func assertOnlyAllowedChanges(t *testing.T, before, after string) {
	t.Helper()
	b, a := map[string]int{}, map[string]int{}
	flowIns := regexp.MustCompile(`, ALLOWED_EMAILS: "[^"]*"`)
	before, after = flowIns.ReplaceAllString(before, ""), flowIns.ReplaceAllString(after, "")
	for _, l := range lines(before) {
		b[l]++
	}
	for _, l := range lines(after) {
		a[l]++
	}
	for l, n := range b {
		if a[l] < n && !strings.Contains(l, "expose-") && !strings.Contains(l, "ALLOWED_EMAILS") {
			t.Errorf("line lost/changed outside the allowed set: %q", l)
		}
	}
	for l, n := range a {
		if b[l] < n && !strings.Contains(l, "expose-") && !strings.Contains(l, "ALLOWED_EMAILS") {
			t.Errorf("line added/changed outside the allowed set: %q", l)
		}
	}
}

func TestEditPreservesBytes(t *testing.T) {
	for name, src := range fixtures {
		for _, mode := range allModes {
			t.Run(name+"->"+mode, func(t *testing.T) {
				cur, err := ParseApp([]byte(src))
				if err != nil {
					t.Fatal(err)
				}
				emails := ""
				if mode == ModeGoogleRestricted {
					emails = "z@z.com,q@q.com"
				}
				if cur.Mode == mode && emails == cur.AllowedEmails {
					return
				}
				out, old, err := EditApp([]byte(src), mode, emails)
				if err != nil {
					t.Fatal(err)
				}
				if old != cur.Mode {
					t.Errorf("old mode %q want %q", old, cur.Mode)
				}
				got, _ := ParseApp(out)
				if got.Mode != mode || got.AllowedEmails != emails || got.Host != cur.Host || got.Path != cur.Path || got.Name != cur.Name {
					t.Errorf("parsed result wrong: %+v", got)
				}
				assertOnlyAllowedChanges(t, strings.ReplaceAll(src, "\r\n", "\n"), strings.ReplaceAll(string(out), "\r\n", "\n"))
				if strings.Contains(src, "\r\n") && strings.Contains(strings.ReplaceAll(string(out), "\r\n", ""), "\n") {
					t.Error("CRLF not preserved")
				}
				if !strings.Contains(string(out), "components/namespace-baseline") && strings.Contains(src, "components/namespace-baseline") {
					t.Error("other component lost")
				}
			})
		}
	}
}

func TestExactOutput(t *testing.T) {
	out, _, err := EditApp([]byte(fixtures["block-list"]), ModePrivate, "")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(fixtures["block-list"], "expose-google   # <-", "expose-private   # <-", 1)
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	// flow style: only the word changes, and email appended inside the flow map.
	out, _, err = EditApp([]byte(fixtures["flow-list-flow-map"]), ModeGoogleRestricted, "a@b.co")
	if err != nil {
		t.Fatal(err)
	}
	want = strings.Replace(fixtures["flow-list-flow-map"], "expose-private,", "expose-google-restricted,", 1)
	want = strings.Replace(want, `HOST: homepage.example.com }`, `HOST: homepage.example.com, ALLOWED_EMAILS: "a@b.co" }`, 1)
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}

	// block: key line removed, trailing comment elsewhere intact; value replaced keeping the comment.
	src := fixtures["quoted-restricted-crlf"]
	out, _, err = EditApp([]byte(src), ModeGoogleRestricted, "n@n.io")
	if err != nil {
		t.Fatal(err)
	}
	want = strings.Replace(src, `"a@x.com,b@y.com"  # who may enter`, `"n@n.io"  # who may enter`, 1)
	if string(out) != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}
	out, _, _ = EditApp([]byte(src), ModeGoogle, "")
	want = strings.Replace(strings.Replace(src, "expose-google-restricted", "expose-google", 1), "      ALLOWED_EMAILS: \"a@x.com,b@y.com\"  # who may enter\r\n", "", 1)
	if string(out) != want {
		t.Errorf("got:\n%q\nwant:\n%q", out, want)
	}
}

func TestRoundTripRestoresOriginal(t *testing.T) {
	for _, name := range []string{"block-list", "flow-list-flow-map", "public-with-unicode"} {
		src := fixtures[name]
		a, _ := ParseApp([]byte(src))
		mid, _, err := EditApp([]byte(src), ModeGoogleRestricted, "r@r.org")
		if err != nil {
			t.Fatal(name, err)
		}
		back, _, err := EditApp(mid, a.Mode, "")
		if err != nil {
			t.Fatal(name, err)
		}
		// flow maps keep a blanked key by design; everything else must be identical.
		if name != "flow-list-flow-map" && string(back) != src {
			t.Errorf("%s: round trip differs:\n%s", name, back)
		}
	}
}

func TestEditErrors(t *testing.T) {
	if _, _, err := EditApp([]byte(fixtures["block-list"]), "weird", ""); err == nil {
		t.Error("bad mode accepted")
	}
	if _, _, err := EditApp([]byte(fixtures["block-list"]), ModeGoogle, ""); err == nil {
		t.Error("no-op should error")
	}
	noComp := "apiVersion: v1\nkind: Kustomization\nmetadata:\n  name: x\nspec:\n  path: ./x\n  components: [../../../components/app-netpol]\n"
	if _, _, err := EditApp([]byte(noComp), ModePublic, ""); err == nil {
		t.Error("file without expose component accepted")
	}
	if _, _, err := EditApp([]byte(fixtures["block-list"]), ModeGoogleRestricted, `a"@b.c`); err == nil {
		t.Error("unsafe email accepted")
	}
	if _, err := ParseApp([]byte("kind: ConfigMap\n")); err == nil {
		t.Error("non-Kustomization accepted")
	}
}

func TestParse(t *testing.T) {
	a, err := ParseApp([]byte(fixtures["quoted-restricted-crlf"]))
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != ModeGoogleRestricted || a.Host != "planka.example.com" || a.AllowedEmails != "a@x.com,b@y.com" || a.Name != "planka" {
		t.Errorf("%+v", a)
	}
}
