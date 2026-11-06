package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestIndexAndStaticAssets(t *testing.T) {
	e := newEnv(t)
	html, ck := e.index(t)
	for _, want := range []string{`name="csrf-token"`, ck.Value, `src="/app.js"`, `href="/app.css"`} {
		if !strings.Contains(html, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, bad := range []string{"<script>", "onclick", "alert(", "confirm(", "style=", "https://cdn", "googleapis"} {
		if strings.Contains(html, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	r, _ := http.Get(e.ts.URL + "/")
	csp := r.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp lacks %q: %s", want, csp)
		}
	}
	for _, p := range []string{"/app.js", "/app.css", "/healthz"} {
		if r, _ := http.Get(e.ts.URL + p); r.StatusCode != 200 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
}

func TestAppsListing(t *testing.T) {
	e := newEnv(t)
	var apps []AppView
	if c := e.get(t, "/api/apps", &apps); c != 200 {
		t.Fatal(c)
	}
	by := map[string]AppView{}
	for _, a := range apps {
		by[a.Name] = a
	}
	x := by["excalidraw"]
	if x.Mode != "google" || x.Host != "draw.example.com" || x.Pill != "Ready" || !x.Managed || x.Replicas != 1 {
		t.Errorf("%+v", x)
	}
	if p := by["portfolio"]; !p.Built || p.SecretCount != 10 || p.Tag != "20260101000000-0000000" {
		t.Errorf("portfolio %+v", p)
	}
	if f := by["feedback"]; !f.HasPVC {
		t.Error("feedback pvc")
	}
	sb := by["switchboard"]
	if !sb.Protected || sb.Managed || len(sb.Disabled) != 2 {
		t.Errorf("switchboard %+v", sb)
	}
	if by["db"].Switchable || by["db"].Managed {
		t.Error("db must not be switchable/managed")
	}
}

func TestSwitchRequiresCSRFAndRejects(t *testing.T) {
	e := newEnv(t)
	body := `{"app":"excalidraw","mode":"private"}`
	if r, _ := e.postTo(t, "/api/switch", nil, "", body); r.StatusCode != 403 {
		t.Errorf("no csrf: %d", r.StatusCode)
	}
	if r, _ := e.postTo(t, "/api/switch", e.ck, "wrong", body); r.StatusCode != 403 {
		t.Errorf("bad csrf: %d", r.StatusCode)
	}
	for body, code := range map[string]int{
		`{"app":"excalidraw","mode":"nuclear"}`:                                  400,
		`{"app":"../etc","mode":"public"}`:                                       400,
		`{"app":"excalidraw","mode":"google-restricted","allowed_emails":"bad"}`: 400,
		`{"app":"switchboard","mode":"public"}`:                                  403,
		`{"app":"switchboard","mode":"google"}`:                                  403,
		`{"app":"ghost","mode":"public"}`:                                        404,
		`{"app":"excalidraw","mode":"google"}`:                                   409,
		`{"app":"excalidraw","mode":"public","x":1}`:                             400,
	} {
		if r, m := e.post(t, "/api/switch", body); r.StatusCode != code {
			t.Errorf("%s -> %d %v, want %d", body, r.StatusCode, m, code)
		}
	}
	if len(e.gh.CommitLog()) != 0 {
		t.Error("committed on rejected request")
	}
}

func TestSwitchHappyPathAndSSE(t *testing.T) {
	e := newEnv(t)
	e.cl.onNote = func(sha string) {
		go func() {
			time.Sleep(60 * time.Millisecond)
			e.cl.set("excalidraw", KStatus{Ready: "True", Revision: "main@sha1:" + sha})
		}()
	}
	r, m := e.post(t, "/api/switch", `{"app":"excalidraw","mode":"google-restricted","allowed_emails":"A@x.com"}`)
	if r.StatusCode != 202 {
		t.Fatal(r.StatusCode, m)
	}
	log := e.gh.CommitLog()
	if len(log) != 1 || log[0].Message != "switchboard: excalidraw google→google-restricted" || log[0].Path != "clusters/keep/apps/excalidraw.yaml" {
		t.Fatalf("%+v", log)
	}
	got := e.file(t, "clusters/keep/apps/excalidraw.yaml")
	if !strings.Contains(got, "expose-google-restricted # <-") || !strings.Contains(got, `ALLOWED_EMAILS: "a@x.com"`) {
		t.Errorf("file not edited as asked:\n%s", got)
	}
	evs := e.events(t, m["job"].(string))
	if lastEvent(evs).State != "done" || len(evs) < 3 {
		t.Errorf("events %+v", evs)
	}
	if e.cl.reconciles != 1 {
		t.Errorf("reconcile requests: %d", e.cl.reconciles)
	}
	if l := e.logs.String(); !strings.Contains(l, `"result":"committed"`) || !strings.Contains(l, `"app":"excalidraw"`) {
		t.Errorf("no structured log: %s", l)
	}
}

func TestSwitchConflictReportsReloadAndFreesSlot(t *testing.T) {
	e := newEnv(t)
	e.gh.FailNext = 409
	r, m := e.post(t, "/api/switch", `{"app":"excalidraw","mode":"private"}`)
	if r.StatusCode != 409 || m["reload"] != true {
		t.Errorf("%d %v", r.StatusCode, m)
	}
	if r, _ := e.post(t, "/api/switch", `{"app":"excalidraw","mode":"private"}`); r.StatusCode != 202 {
		t.Errorf("slot not released: %d", r.StatusCode)
	}
}

func TestIdentityEnforcement(t *testing.T) {
	e := newEnv(t)
	get := func(path, hdr, val string) int {
		req, _ := http.NewRequest("GET", e.ts.URL+path, nil)
		req.Header.Set("X-Test-Raw", "1")
		if hdr != "" {
			req.Header.Set(hdr, val)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	for path, want := range map[string]int{"/": 403, "/api/apps": 403, "/api/me": 403, "/app.js": 403, "/healthz": 200} {
		if c := get(path, "", ""); c != want {
			t.Errorf("%s without identity: %d", path, c)
		}
	}
	if c := get("/", "X-Auth-Request-Email", "stranger@example.com"); c != 403 {
		t.Errorf("wrong identity: %d", c)
	}
	if c := get("/", "X-Tailscale-User-Login", "me@example.com"); c != 403 {
		t.Errorf("tailscale header must not count: %d", c)
	}
	if c := get("/", "X-Auth-Request-Email", "ME@example.com"); c != 200 {
		t.Errorf("good identity: %d", c)
	}
}

func TestPostsAreCSRFProtected(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/plan", "/api/apply", "/api/switch"} {
		if r, _ := e.postTo(t, p, nil, "", `{}`); r.StatusCode != 403 {
			t.Errorf("%s without csrf: %d", p, r.StatusCode)
		}
	}
}

func TestDetailPodsLogsSecretsTags(t *testing.T) {
	e := newEnv(t)
	e.cl.pods["portfolio"] = []PodInfo{{Name: "portfolio-abc", Phase: "Running", Ready: "0/1", Reason: "ImagePullBackOff", Containers: []string{"portfolio"}}}
	e.cl.logText = "ok line\npassword=hunter2 failed\nAuthorization: Bearer abc.def\nlast\n"
	e.cl.es["portfolio"] = SecretStatus{Ready: "False", Reason: "SecretSyncedError", Message: "not found"}
	e.cl.images["portfolio"] = &ImageStatus{Tags: []string{"b", "a"}}
	var d Detail
	if c := e.get(t, "/api/apps/portfolio", &d); c != 200 {
		t.Fatal(c)
	}
	if len(d.Pods) != 1 || d.Pods[0].Reason != "ImagePullBackOff" {
		t.Errorf("pods %+v", d.Pods)
	}
	if len(d.Secrets) != 1 || d.Secrets[0].Status.Ready != "False" || len(d.Secrets[0].Keys) != 10 || !strings.HasPrefix(d.Secrets[0].Keys[0], "app-portfolio--") {
		t.Errorf("secrets %+v", d.Secrets)
	}
	if d.Tags == nil || len(d.Tags.Tags) != 2 {
		t.Errorf("tags %+v", d.Tags)
	}
	var lg struct{ Lines []string }
	if c := e.get(t, "/api/apps/portfolio/logs?pod=portfolio-abc", &lg); c != 200 {
		t.Fatal(c)
	}
	joined := strings.Join(lg.Lines, "\n")
	if strings.Contains(joined, "hunter2") || strings.Contains(joined, "abc.def") || !strings.Contains(joined, "[redacted]") || !strings.Contains(joined, "ok line") {
		t.Errorf("log redaction: %q", joined)
	}
	if c := e.get(t, "/api/apps/portfolio/logs?pod=someone-elses-pod", nil); c != 404 {
		t.Errorf("foreign pod: %d", c)
	}
	if c := e.get(t, "/api/apps/portfolio/logs?pod=portfolio-abc&container=nope", nil); c != 404 {
		t.Errorf("foreign container: %d", c)
	}
	if c := e.get(t, "/api/apps/ghost", nil); c != 404 {
		t.Errorf("ghost: %d", c)
	}
}

func TestK8sClusterReadsAndOnlyPatchesGitRepo(t *testing.T) {
	ks := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "n8n", "namespace": "flux-system"},
		"status": map[string]any{"lastAppliedRevision": "main@sha1:aaa", "conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "BuildFailed", "message": "boom", "lastTransitionTime": "2026-10-30T10:00:00Z"}}},
	}}
	gr := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": "GitRepository",
		"metadata": map[string]any{"name": "flux-system", "namespace": "flux-system"}}}
	ir := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "image.toolkit.fluxcd.io/v1", "kind": "ImageRepository",
		"metadata": map[string]any{"name": "portfolio", "namespace": "flux-system"},
		"spec":     map[string]any{"image": "ghcr.io/x/portfolio"},
		"status":   map[string]any{"lastScanResult": map[string]any{"latestTags": []any{"t2", "t1"}, "tagCount": int64(7), "scanTime": "2026-10-30T10:00:00Z"}}}}
	ip := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "image.toolkit.fluxcd.io/v1", "kind": "ImagePolicy",
		"metadata": map[string]any{"name": "portfolio", "namespace": "flux-system"},
		"status":   map[string]any{"latestRef": map[string]any{"tag": "t2"}}}}
	es := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret",
		"metadata": map[string]any{"name": "portfolio", "namespace": "personal"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "SecretSynced"}}}}}
	d := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gvrKustomization: "KustomizationList", gvrGitRepository: "GitRepositoryList", gvrExternalSec: "ExternalSecretList",
		gvrImageRepo: "ImageRepositoryList", gvrImagePolicy: "ImagePolicyList"}, ks, gr, ir, ip, es)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "portfolio-1", Namespace: "personal", Labels: map[string]string{"citadel.dev/app": "portfolio"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "portfolio", Image: "img:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "portfolio", RestartCount: 2,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}}}
	kc := newK8sClusterWith(d, kfake.NewSimpleClientset(pod), "flux-system", "flux-system")
	ctx := context.Background()
	st, err := kc.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s := st["n8n"]; s.Pill() != "Failed" || s.Revision != "main@sha1:aaa" || s.Message != "boom" || s.LastChange.IsZero() {
		t.Errorf("%+v", s)
	}
	ps, err := kc.Pods(ctx, "personal", "portfolio")
	if err != nil || len(ps) != 1 || ps[0].Reason != "ImagePullBackOff" || ps[0].Restarts != 2 || ps[0].Ready != "0/1" {
		t.Errorf("pods %+v %v", ps, err)
	}
	is, err := kc.ImageStatus(ctx, "portfolio")
	if err != nil || is.PolicyTag != "t2" || len(is.Tags) != 2 || is.TagCount != 7 || is.Image != "ghcr.io/x/portfolio" {
		t.Errorf("image %+v %v", is, err)
	}
	ess, err := kc.ExternalSecrets(ctx, "personal")
	if err != nil || ess["portfolio"].Ready != "True" {
		t.Errorf("es %+v %v", ess, err)
	}
	if _, err := kc.Logs(ctx, "personal", "portfolio-1", "portfolio", 10); err != nil {
		t.Errorf("logs: %v", err)
	}
	if err := kc.RequestReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Actions() {
		switch a.GetVerb() {
		case "patch":
			pa := a.(k8stesting.PatchAction)
			if pa.GetResource() != gvrGitRepository || pa.GetName() != "flux-system" || !strings.Contains(string(pa.GetPatch()), "reconcile.fluxcd.io/requestedAt") {
				t.Errorf("unexpected patch %v %s", pa.GetResource(), pa.GetPatch())
			}
		case "list", "get":
		default:
			t.Errorf("unexpected verb %s", a.GetVerb())
		}
	}
}
