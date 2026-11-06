package main

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Check is one validation result. Status is ok | warn | fail.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type checker struct{ out []Check }

func (c *checker) ok(name, d string, a ...any)   { c.add("ok", name, d, a...) }
func (c *checker) warn(name, d string, a ...any) { c.add("warn", name, d, a...) }
func (c *checker) fail(name, d string, a ...any) { c.add("fail", name, d, a...) }
func (c *checker) add(st, name, d string, a ...any) {
	c.out = append(c.out, Check{name, st, fmt.Sprintf(d, a...)})
}
func (c *checker) check(cond bool, name, okMsg, failMsg string) bool {
	if cond {
		c.ok(name, "%s", okMsg)
	} else {
		c.fail(name, "%s", failMsg)
	}
	return cond
}

// ChecksOK is true when nothing failed.
func ChecksOK(cs []Check) bool {
	for _, c := range cs {
		if c.Status == "fail" {
			return false
		}
	}
	return true
}

// componentVars lists the substitution variables each component needs without a default
// (components/README.md and the backup component headers).
var componentVars = map[string][]string{
	"expose-public":            {"APP", "NAMESPACE", "HOST", "PORT"},
	"expose-google":            {"APP", "NAMESPACE", "HOST", "PORT"},
	"expose-private":           {"APP", "NAMESPACE", "HOST", "PORT"},
	"expose-google-restricted": {"APP", "NAMESPACE", "HOST", "PORT", "ALLOWED_EMAILS"},
	"app-netpol":               {"APP", "NAMESPACE", "PORT"},
	"db-client":                {"APP", "NAMESPACE"},
	"egress-restricted":        {"APP", "NAMESPACE"},
	"backup-pvc":               {},
	"pvc-backup":               {"NAMESPACE", "PVC"},
	"restore-if-empty":         {"NAMESPACE", "PVC", "DATA_VOLUME"},
}

var substRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)([:]?[-=?+][^}]*)?\}`)

// CheckApp ports the checks of citadel's apps/tools/validate-apps.sh that do not need kustomize or
// kubeconform: wiring of the Flux Kustomization, components, substitution variables, exactly one Deployment
// and Service, no Namespace, restricted Pod Security, resources, probes, pinned image, backup wiring, secrets
// only as ExternalSecrets, registration and uniqueness. s is the repo state AFTER the planned change.
func CheckApp(s *Snapshot, cfg *Config, app string) []Check {
	c := &checker{}
	kfile := cfg.AppsDir + "/" + app + ".yaml"
	raw, ok := s.Files[kfile]
	if !c.check(ok, "flux-kustomization", kfile+" exists", kfile+" is missing") {
		return c.out
	}
	docs, err := decodeDocs(raw)
	if err != nil || len(docs) == 0 {
		c.fail("flux-kustomization", "does not parse: %v", err)
		return c.out
	}
	k := docs[0]
	spec, _ := ParseApp(raw)
	c.check(str(k["apiVersion"]) == "kustomize.toolkit.fluxcd.io/v1" && kind(k) == "Kustomization" &&
		str(dig(k, "metadata", "namespace")) == "flux-system" && spec != nil && spec.Name == app,
		"flux-kustomization", "kustomize.toolkit.fluxcd.io/v1 Kustomization "+app+" in flux-system", "must be a kustomize.toolkit.fluxcd.io/v1 Kustomization named "+app+" in flux-system")
	c.check(dig(k, "spec", "prune") == true, "prune", "prune: true", "spec.prune must be true so deleting the app cleans up")
	c.check(str(dig(k, "spec", "targetNamespace")) == "", "target-namespace", "no targetNamespace (components emit into other namespaces)", "spec.targetNamespace must not be set")
	var depNames []string
	for _, d := range list(dig(k, "spec", "dependsOn")) {
		depNames = append(depNames, str(dig(d, "name")))
	}
	c.check(contains(depNames, "infra-configs"), "depends-on", "dependsOn infra-configs", "dependsOn must include infra-configs")

	p := strings.TrimPrefix(str(dig(k, "spec", "path")), "./")
	if !c.check(strings.HasPrefix(p, "apps/") && s.Has(p+"/kustomization.yaml"), "app-path", "spec.path -> "+p+"/kustomization.yaml", "spec.path must point at ./apps/<dir> containing a kustomization.yaml") {
		return c.out
	}

	// components
	subst := map[string]string{}
	if m := asMap(dig(k, "spec", "postBuild", "substitute")); m != nil {
		for kk, v := range m {
			subst[kk] = str(v)
		}
	}
	fromCluster := len(list(dig(k, "spec", "postBuild", "substituteFrom"))) > 0
	expose := 0
	need := map[string]bool{}
	for _, comp := range strList(dig(k, "spec", "components")) {
		target := resolve(p, comp)
		base := path.Base(target)
		if strings.HasPrefix(base, "expose-") {
			expose++
		}
		if !s.Has(target + "/kustomization.yaml") {
			c.fail("components", "%s does not resolve to a component directory (%s)", comp, target)
			continue
		}
		vars, known := componentVars[base]
		if !known {
			c.warn("components", "%s: unknown component, variables not checked", base)
		}
		for _, v := range vars {
			need[v] = true
		}
	}
	c.check(expose == 1, "exposure", "exactly one expose-* component", fmt.Sprintf("need exactly one expose-* component, found %d", expose))
	var missing []string
	for v := range need {
		if _, ok := subst[v]; !ok && !fromCluster {
			missing = append(missing, v)
		}
	}
	c.check(len(missing) == 0, "substitute", "all component variables are set", "postBuild.substitute is missing "+strings.Join(sorted(missing), ", "))
	c.check(subst["APP"] == "" || subst["APP"] == app, "substitute", "APP matches", "APP must equal "+app)
	if pt := subst["PORT"]; pt != "" {
		c.check(portNum(pt) >= 1 && portNum(pt) <= 65535, "port", "PORT "+pt, "PORT must be a number 1-65535")
	}
	if h := subst["HOST"]; h != "" {
		c.check(hostRe.MatchString(h), "host", "HOST "+h, "HOST is not a valid hostname")
	}
	ns := subst["NAMESPACE"]
	c.check(contains(cfg.AppNamespaces, ns), "namespace", "NAMESPACE "+ns, "NAMESPACE must be one of "+strings.Join(cfg.AppNamespaces, ", "))
	if spec != nil && spec.Mode == ModeGoogleRestricted {
		_, err := NormalizeEmails(subst["ALLOWED_EMAILS"])
		c.check(err == nil, "allowed-emails", "ALLOWED_EMAILS valid", "ALLOWED_EMAILS missing or invalid for google-restricted")
	}

	// app directory
	kb, err := decodeDocs(s.Files[p+"/kustomization.yaml"])
	if err != nil || len(kb) == 0 {
		c.fail("app-kustomization", "does not parse")
		return c.out
	}
	var all []Doc
	files := map[string][]Doc{}
	for _, r := range strList(dig(kb[0], "resources")) {
		fp := resolve(p, r)
		b, ok := s.Files[fp]
		if !ok {
			if !s.Has(fp) {
				c.fail("app-kustomization", "resource %s listed but missing", r)
			}
			continue
		}
		ds, err := decodeDocs(b)
		if err != nil {
			c.fail("manifests", "%s does not parse: %v", r, err)
			continue
		}
		files[fp] = ds
		all = append(all, ds...)
		for _, m := range substRe.FindAllStringSubmatch(strings.ReplaceAll(string(b), "$${", ""), -1) {
			if m[2] == "" && subst[m[1]] == "" && !fromCluster {
				c.fail("substitute", "%s uses ${%s} which is not set in postBuild.substitute", r, m[1])
			}
		}
	}
	byKind := map[string][]Doc{}
	for _, d := range all {
		byKind[kind(d)] = append(byKind[kind(d)], d)
	}
	c.check(len(byKind["Deployment"]) == 1, "one-deployment", "exactly 1 Deployment", fmt.Sprintf("expected exactly 1 Deployment, found %d", len(byKind["Deployment"])))
	c.check(len(byKind["Service"]) == 1, "one-service", "exactly 1 Service", fmt.Sprintf("expected exactly 1 Service, found %d", len(byKind["Service"])))
	c.check(len(byKind["Namespace"]) == 0, "no-namespace", "no Namespace object", "app contains a Namespace; apps never create namespaces")
	c.check(len(byKind["Secret"]) == 0, "no-secret-objects", "no Secret objects in git", "a Secret object is committed; use an ExternalSecret")
	if len(byKind["Deployment"]) != 1 || len(byKind["Service"]) != 1 {
		return c.out
	}
	dep, svc := byKind["Deployment"][0], byKind["Service"][0]
	for _, d := range all {
		if m := str(dig(d, "metadata", "namespace")); m != "" && m != ns {
			c.fail("namespace", "%s/%s is in %s, expected %s", kind(d), str(dig(d, "metadata", "name")), m, ns)
		}
	}

	// labels and wiring
	lab := func(v any) string { return str(dig(v, "citadel.dev/app")) }
	c.check(lab(dig(dep, "spec", "selector", "matchLabels")) == app && lab(dig(dep, "spec", "template", "metadata", "labels")) == app,
		"labels", "selector and pod template carry citadel.dev/app="+app, "Deployment selector and pod template need citadel.dev/app="+app)
	c.check(lab(dig(svc, "spec", "selector")) == app, "service", "Service selects citadel.dev/app="+app, "Service selector must be citadel.dev/app="+app)
	pod := dig(dep, "spec", "template", "spec")
	ctrs := list(dig(pod, "containers"))
	if !c.check(len(ctrs) >= 1, "containers", fmt.Sprintf("%d container(s)", len(ctrs)), "no containers") {
		return c.out
	}
	if pt := subst["PORT"]; pt != "" {
		c.check(str(dig(svc, "spec", "ports", 0, "port")) == pt, "service-port", "Service port matches PORT", "Service port must equal PORT "+pt+" (app-netpol and Ingress use it)")
	}
	var cports []string
	for _, cp := range list(dig(ctrs[0], "ports")) {
		cports = append(cports, str(dig(cp, "name")), str(dig(cp, "containerPort")))
	}
	tp := str(dig(svc, "spec", "ports", 0, "targetPort"))
	c.check(contains(cports, tp), "target-port", "Service targetPort "+tp+" is a container port", "Service targetPort "+tp+" does not match a container port name/number")

	// Pod Security restricted
	podSC := dig(pod, "securityContext")
	all2 := append(append([]any{}, ctrs...), list(dig(pod, "initContainers"))...)
	psa := []string{}
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if dig(pod, f) == true {
			psa = append(psa, f+" is set")
		}
	}
	for _, v := range list(dig(pod, "volumes")) {
		if dig(v, "hostPath") != nil {
			psa = append(psa, "hostPath volume")
		}
	}
	for _, ct := range all2 {
		cn := str(dig(ct, "name"))
		sc := dig(ct, "securityContext")
		if !(dig(podSC, "runAsNonRoot") == true || dig(sc, "runAsNonRoot") == true) {
			psa = append(psa, cn+": runAsNonRoot not true")
		}
		for _, u := range []any{dig(podSC, "runAsUser"), dig(sc, "runAsUser")} {
			if u == 0 {
				psa = append(psa, cn+": runs as uid 0")
			}
		}
		okSec := false
		for _, t := range []string{str(dig(podSC, "seccompProfile", "type")), str(dig(sc, "seccompProfile", "type"))} {
			okSec = okSec || t == "RuntimeDefault" || t == "Localhost"
		}
		if !okSec {
			psa = append(psa, cn+": seccompProfile not RuntimeDefault")
		}
		if dig(sc, "allowPrivilegeEscalation") != false {
			psa = append(psa, cn+": allowPrivilegeEscalation must be false")
		}
		if !contains(strList(dig(sc, "capabilities", "drop")), "ALL") {
			psa = append(psa, cn+": capabilities.drop must include ALL")
		}
		if dig(sc, "privileged") == true {
			psa = append(psa, cn+": privileged")
		}
	}
	c.check(len(psa) == 0, "pod-security", "restricted PSA: runAsNonRoot, seccomp RuntimeDefault, no privilege escalation, drop ALL", strings.Join(psa, "; "))

	var res, probes []string
	for _, ct := range ctrs {
		cn := str(dig(ct, "name"))
		for _, q := range [][]string{{"requests", "cpu"}, {"requests", "memory"}, {"limits", "cpu"}, {"limits", "memory"}} {
			if str(dig(ct, "resources", q[0], q[1])) == "" {
				res = append(res, cn+" "+q[0]+"."+q[1])
			}
		}
		if dig(ct, "readinessProbe") == nil {
			probes = append(probes, cn+" readinessProbe")
		}
		if dig(ct, "livenessProbe") == nil {
			probes = append(probes, cn+" livenessProbe")
		}
	}
	c.check(len(res) == 0, "resources", "requests and limits set", "missing "+strings.Join(res, ", "))
	c.check(len(probes) == 0, "probes", "readiness and liveness probes", "missing "+strings.Join(probes, ", "))

	// image
	img := str(dig(ctrs[0], "image"))
	repo, tag, digest := splitImage(img)
	switch {
	case img == "":
		c.fail("image", "container has no image")
	case digest == "" && (tag == "" || tag == "latest"):
		c.fail("image", "%s is not pinned: use a version tag or digest, never latest", img)
	default:
		c.ok("image", "pinned: %s", img)
	}
	if a := loadAppFrom(s, cfg, app); a != nil && a.Built {
		checkBuilt(c, s, cfg, a, repo, pod)
	}

	// PVC / backup
	if pvcs := byKind["PersistentVolumeClaim"]; len(pvcs) > 0 {
		for _, pv := range pvcs {
			sz := str(dig(pv, "spec", "resources", "requests", "storage"))
			c.check(pvcSizeRe.MatchString(sz), "pvc", "PVC "+str(dig(pv, "metadata", "name"))+" "+sz, "PVC size "+sz+" is not like 2Gi")
		}
		comps := strings.Join(strList(dig(k, "spec", "components")), " ")
		c.check(strings.Contains(comps, "backup-pvc") && strings.Contains(comps, "pvc-backup"), "backup", "backup-pvc + pvc-backup components included", "PVC app must include backup-pvc and infrastructure/configs/backup/pvc-backup")
		claims := []string{}
		for _, v := range list(dig(pod, "volumes")) {
			claims = append(claims, str(dig(v, "persistentVolumeClaim", "claimName")))
		}
		c.check(contains(claims, subst["PVC"]), "backup", "PVC substitute matches the mounted claim", "substitute PVC must equal the claimName mounted by the Deployment")
	}
	vols := map[string]bool{}
	for _, v := range list(dig(pod, "volumes")) {
		vols[str(dig(v, "name"))] = true
	}
	for _, vm := range list(dig(ctrs[0], "volumeMounts")) {
		if n := str(dig(vm, "name")); !vols[n] {
			c.fail("volumes", "volumeMount %s has no volume", n)
		}
	}

	// secrets
	used := map[string]bool{}
	for _, ct := range ctrs {
		for _, ef := range list(dig(ct, "envFrom")) {
			used[str(dig(ef, "secretRef", "name"))] = true
		}
		for _, e := range list(dig(ct, "env")) {
			if sn := str(dig(e, "valueFrom", "secretKeyRef", "name")); sn != "" {
				used[sn] = true
			}
		}
	}
	targets := map[string]bool{}
	for _, es := range byKind["ExternalSecret"] {
		t := str(dig(es, "spec", "target", "name"))
		if t == "" {
			t = str(dig(es, "metadata", "name"))
		}
		targets[t] = true
		c.check(str(dig(es, "spec", "secretStoreRef", "name")) == "oci-vault", "externalsecret", "store oci-vault", "ExternalSecret must use ClusterSecretStore oci-vault")
		for _, d := range list(dig(es, "spec", "data")) {
			if str(dig(d, "remoteRef", "key")) == "" || str(dig(d, "secretKey")) == "" {
				c.fail("externalsecret", "every entry needs secretKey and remoteRef.key")
			}
		}
	}
	for u := range used {
		c.check(targets[u], "secret-ref", "secret "+u+" comes from an ExternalSecret", "pod references Secret "+u+" which no ExternalSecret in the app creates")
	}

	// registration + uniqueness
	if kb2, ok := s.Files[cfg.AppsDir+"/kustomization.yaml"]; ok {
		res, _ := kustResources(kb2)
		c.check(contains(res, app+".yaml"), "registered", app+".yaml listed in "+cfg.AppsDir+"/kustomization.yaml", app+".yaml is not listed in "+cfg.AppsDir+"/kustomization.yaml, Flux will not deploy it")
	}
	for _, o := range loadApps(s, cfg.AppsDir) {
		if o.Name != app && o.Spec.Host != "" && o.Spec.Host == subst["HOST"] {
			c.fail("unique-host", "%s already uses host %s", o.Name, o.Spec.Host)
		}
	}
	if hp, ok := s.Files["apps/homepage/configmap.yaml"]; ok && subst["HOST"] != "" {
		if contains(HomepageHrefs(hp), "https://"+subst["HOST"]) {
			c.ok("homepage", "listed on the homepage")
		} else {
			c.warn("homepage", "not listed on the homepage ConfigMap")
		}
	}
	return c.out
}

func checkBuilt(c *checker, s *Snapshot, cfg *Config, a *AppInfo, repo string, pod any) {
	if a.Pinned {
		c.ok("image-automation", "rolled back: Flux marker removed on purpose (Resume auto-update to hand it back)")
		return
	}
	var ir, ip Doc
	for _, d := range func() []Doc { ds, _ := decodeDocs(s.Files[cfg.AppsDir+"/image-automation.yaml"]); return ds }() {
		switch {
		case kind(d) == "ImageRepository" && str(dig(d, "metadata", "name")) == a.Setter:
			ir = d
		case kind(d) == "ImagePolicy" && str(dig(d, "metadata", "name")) == a.Setter:
			ip = d
		}
	}
	if !(ir != nil && ip != nil) {
		c.fail("image-automation", "marker flux-system:%s but image-automation.yaml has no ImageRepository+ImagePolicy named %s", a.Setter, a.Setter)
		return
	}
	if str(dig(ir, "spec", "image")) != repo {
		c.warn("image-automation", "ImageRepository scans %s but the Deployment uses %s", str(dig(ir, "spec", "image")), repo)
	} else {
		c.ok("image-automation", "ImageRepository + ImagePolicy %s scan %s", a.Setter, repo)
	}
	if strings.HasPrefix(repo, "ghcr.io/") {
		c.check(str(dig(ir, "spec", "secretRef", "name")) == "ghcr-pull", "ghcr-pull", "ImageRepository uses secretRef ghcr-pull", "ImageRepository for a GHCR image needs secretRef ghcr-pull")
		ips := false
		for _, x := range list(dig(pod, "imagePullSecrets")) {
			ips = ips || str(dig(x, "name")) == "ghcr-pull"
		}
		c.check(ips, "ghcr-pull", "pod pulls with ghcr-pull", "Deployment needs imagePullSecrets: [{name: ghcr-pull}] for a private GHCR image")
	}
}

func sorted(l []string) []string {
	out := append([]string(nil), l...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func loadAppFrom(s *Snapshot, cfg *Config, name string) *AppInfo {
	for _, a := range loadApps(s, cfg.AppsDir) {
		if a.Name == name {
			return a
		}
	}
	return nil
}
