package main

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// NewApp is the form behind "New app". It carries no secret values: secrets are only the NAMES of env vars
// an ExternalSecret should map from the vault (values are entered with `make secret`, outside the UI).
type NewApp struct {
	Name          string      `json:"name"`
	ImageKind     string      `json:"image_kind"` // built | pinned
	Image         string      `json:"image"`      // built: repo name or full ghcr path; pinned: full reference with tag/digest
	Port          int         `json:"port"`
	Mode          string      `json:"mode"`
	Host          string      `json:"host"`
	AllowedEmails string      `json:"allowed_emails"`
	Namespace     string      `json:"namespace"`
	Replicas      *int        `json:"replicas"`
	PVCSize       string      `json:"pvc_size"` // "" = none, e.g. 2Gi
	DataPath      string      `json:"data_path"`
	DBClient      bool        `json:"db_client"` // network access to the shared Postgres only
	Database      bool        `json:"database"`  // a new database + role for this app, DATABASE_URL wired (implies db-client)
	UID           int         `json:"uid"`
	HealthPath    string      `json:"health_path"`
	Size          string      `json:"size"` // tiny | small | medium
	Secrets       []string    `json:"secrets"`
	Env           []EnvVar    `json:"env"`
	Title         string      `json:"title"`
	Description   string      `json:"description"`
	Group         string      `json:"group"`  // homepage group
	Source        *SourceSpec `json:"source"` // create a new private repo from a stack template
	_             struct{}    `json:"-"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

var (
	envNameRe     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,62}$`)
	secretNameRe  = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE|CREDENTIAL|AUTH_?KEY|DATABASE_URL|DSN)`)
	secretValueRe = regexp.MustCompile(`(?i)(-----BEGIN|ghp_[A-Za-z0-9]{20}|github_pat_|sk-[A-Za-z0-9]{20}|AKIA[0-9A-Z]{16}|xox[bap]-|eyJ[A-Za-z0-9_-]{20,}\.)`)
	pvcSizeRe     = regexp.MustCompile(`^[1-9][0-9]{0,3}(Mi|Gi)$`)
	hostRe        = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	repoNameRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
	pinnedImgRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?(/[a-z0-9._-]+)+(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[a-f0-9]{64})?$`)
	secretKeyRe   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,62}$`)
)

// reservedNames can never be used for a new app (platform components and namespaces).
var reservedNames = map[string]bool{"db": true, "db-vars": true, "auth": true, "oauth2-proxy": true, "switchboard": true,
	"headlamp": true, "flux-system": true, "kube-system": true, "configs": true, "watchtower": true, "personal": true, "client": true,
	"infra-configs": true, "traefik": true, "default": true}

type sizeSpec struct{ reqCPU, reqMem, limCPU, limMem string }

var sizes = map[string]sizeSpec{
	"tiny":   {"10m", "32Mi", "250m", "128Mi"},
	"small":  {"50m", "128Mi", "500m", "384Mi"},
	"medium": {"100m", "256Mi", "1", "768Mi"},
}

// FieldError ties a message to a form field so the UI can mark it.
type FieldError struct {
	Field string `json:"field"`
	Msg   string `json:"msg"`
}

func (e FieldError) Error() string { return e.Field + ": " + e.Msg }

// Normalize fills defaults and rejects bad input. It does not look at the repo (uniqueness is checked by the planner).
func (n *NewApp) Normalize(cfg *Config) []FieldError {
	var errs []FieldError
	bad := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	n.Name = strings.TrimSpace(n.Name)
	if n.Source != nil {
		n.applySource(cfg, bad)
		if len(errs) > 0 {
			return errs
		}
	}
	if !appNameRe.MatchString(n.Name) || len(n.Name) > 40 {
		bad("name", "lowercase letters, digits and dashes, at most 40 characters")
	} else if reservedNames[n.Name] || cfg.Protected[n.Name] != "" {
		bad("name", "reserved name")
	}
	if n.Namespace == "" {
		n.Namespace = cfg.AppNamespaces[0]
	}
	if !contains(cfg.AppNamespaces, n.Namespace) {
		bad("namespace", "must be one of "+strings.Join(cfg.AppNamespaces, ", "))
	}

	n.Image = strings.TrimSpace(n.Image)
	switch n.ImageKind {
	case "built":
		img := strings.TrimPrefix(n.Image, cfg.ImagePrefix)
		if !repoNameRe.MatchString(img) {
			bad("image", "GHCR repository name, e.g. my-service (image "+cfg.ImagePrefix+"my-service)")
		} else {
			n.Image = cfg.ImagePrefix + img
		}
	case "pinned":
		repo, tag, digest := splitImage(n.Image)
		switch {
		case !pinnedImgRe.MatchString(n.Image) || !strings.Contains(strings.SplitN(n.Image, "/", 2)[0], "."):
			bad("image", "full reference including the registry host, e.g. docker.io/library/nginx:1.27")
		case digest == "" && (tag == "" || tag == "latest"):
			bad("image", "pin a version tag or a @sha256 digest (no :latest)")
		}
		_ = repo
	default:
		bad("image_kind", "choose built (GHCR via CI) or pinned (public image)")
	}

	if n.Port < 1 || n.Port > 65535 {
		bad("port", "1-65535")
	}
	if n.Mode == "" {
		n.Mode = ModePrivate
	}
	if !validMode(n.Mode) {
		bad("mode", "invalid mode")
	}
	n.Host = strings.ToLower(strings.TrimSpace(n.Host))
	if len(n.Host) > 253 || !hostRe.MatchString(n.Host) {
		bad("host", "a hostname such as app.example.com")
	}
	if n.Mode == ModeGoogleRestricted {
		if em, err := NormalizeEmails(n.AllowedEmails); err != nil {
			bad("allowed_emails", err.Error())
		} else {
			n.AllowedEmails = em
		}
	} else {
		n.AllowedEmails = ""
	}
	if n.Replicas == nil {
		r := 1
		if n.ImageKind == "built" {
			r = 0 // no image exists until CI has run once
		}
		n.Replicas = &r
	} else if *n.Replicas != 0 && *n.Replicas != 1 {
		bad("replicas", "0 or 1")
	}
	n.PVCSize = strings.TrimSpace(n.PVCSize)
	if n.PVCSize != "" && !pvcSizeRe.MatchString(n.PVCSize) {
		bad("pvc_size", "e.g. 1Gi or 500Mi")
	}
	if n.DataPath == "" {
		n.DataPath = "/data"
	}
	if !strings.HasPrefix(n.DataPath, "/") || strings.ContainsAny(n.DataPath, " \"'\\:#{}") || n.DataPath == "/" {
		bad("data_path", "absolute path such as /data")
	}
	if n.UID == 0 {
		n.UID = 1000
	}
	if n.UID < 1 || n.UID > 65534 {
		bad("uid", "non-root user id 1-65534")
	}
	if n.HealthPath == "" {
		n.HealthPath = "/"
	}
	if !strings.HasPrefix(n.HealthPath, "/") || strings.ContainsAny(n.HealthPath, " \"'\\#{}") {
		bad("health_path", "path starting with /")
	}
	if n.Size == "" {
		n.Size = "small"
	}
	if _, ok := sizes[n.Size]; !ok {
		bad("size", "tiny, small or medium")
	}

	sort.Strings(n.Secrets)
	seen := map[string]bool{}
	var secs []string
	for _, s := range n.Secrets {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		if !secretKeyRe.MatchString(s) {
			bad("secrets", "env var names like API_KEY (names only, never values)")
			break
		}
		secs = append(secs, s)
	}
	n.Secrets = secs
	if n.Database {
		n.DBClient = true
		if contains(n.Secrets, "DATABASE_URL") || contains(n.Secrets, "DB_PASSWORD") {
			bad("secrets", "DATABASE_URL is built from the generated database password; remove it from the secret names")
		}
	}
	if len(n.Secrets) > 20 {
		bad("secrets", "at most 20")
	}
	if len(n.Env) > 12 {
		bad("env", "at most 12 variables")
	}
	for _, e := range n.Env {
		switch {
		case !envNameRe.MatchString(e.Name):
			bad("env", "bad variable name "+strconv.Quote(e.Name))
		case secretNameRe.MatchString(e.Name):
			bad("env", e.Name+" looks like a secret; list it under Secrets (names only) instead")
		case len(e.Value) > 200 || strings.ContainsAny(e.Value, "\n\r\x00"):
			bad("env", e.Name+": value too long or multi-line")
		case secretValueRe.MatchString(e.Value):
			bad("env", e.Name+": the value looks like a credential; secrets never go through this form")
		}
	}
	n.Title = strings.TrimSpace(n.Title)
	if n.Title == "" {
		n.Title = n.Name
	}
	if len(n.Title) > 40 || strings.ContainsAny(n.Title, ":#{}[]\n\"'`&*!|>%@") {
		bad("title", "plain text, up to 40 characters")
	}
	n.Description = strings.TrimSpace(n.Description)
	if len(n.Description) > 80 || strings.ContainsAny(n.Description, ":#{}[]\n\"'`&*!|>%@") {
		bad("description", "plain text, up to 80 characters")
	}
	if n.Group == "" {
		n.Group = "Apps"
	}
	if n.Group != "Apps" && n.Group != "Tools" && n.Group != "Platform" {
		bad("group", "Apps, Tools or Platform")
	}
	if n.DBClient && n.Namespace == "" {
		bad("db_client", "needs a namespace")
	}
	if net.ParseIP(n.Host) != nil {
		bad("host", "use a hostname")
	}
	return errs
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func (n *NewApp) Built() bool     { return n.ImageKind == "built" }
func (n *NewApp) PVCName() string { return n.Name + "-data" }

// VaultKey is the vault secret name for an env var, following the app-<app>--<kebab> convention.
func VaultKey(app, env string) string {
	return "app-" + app + "--" + strings.ToLower(strings.ReplaceAll(env, "_", "-"))
}

// placeholderTag is committed until CI has pushed a real image and Flux image automation replaces it.
const placeholderTag = "20260101000000-0000000"

// Generate returns the app's files keyed by repo path (without the registration edits).
func (n *NewApp) Generate() map[string]string {
	out := map[string]string{}
	dir := "apps/" + n.Name + "/"
	res := []string{"deployment.yaml", "service.yaml"}
	if n.PVCSize != "" {
		res = append([]string{"pvc.yaml"}, res...)
	}
	if len(n.Secrets) > 0 || n.Database {
		res = append([]string{"externalsecret.yaml"}, res...)
	}
	var k strings.Builder
	k.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
	for _, r := range res {
		k.WriteString("  - " + r + "\n")
	}
	out[dir+"kustomization.yaml"] = k.String()
	out[dir+"deployment.yaml"] = n.deployment()
	out[dir+"service.yaml"] = fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  selector:
    citadel.dev/app: %[1]s
  ports:
    - name: http
      port: %[3]d
      targetPort: http
`, n.Name, n.Namespace, n.Port)
	if n.PVCSize != "" {
		out[dir+"pvc.yaml"] = fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: local-path
  resources:
    requests:
      storage: %s
`, n.PVCName(), n.Namespace, n.PVCSize)
	}
	if len(n.Secrets) > 0 || n.Database {
		var sb strings.Builder
		fmt.Fprintf(&sb, `# Values live in the OCI Vault, never in git. Enter them with `+"`make secret`"+` (see the README of this repo).
# ESO's Oracle provider cannot list secrets, so every key is named here.
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: %[1]s
  namespace: %[2]s
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: oci-vault
  target:
    name: %[1]s
    creationPolicy: Owner
`, n.Name, n.Namespace)
		if n.Database {
			fmt.Fprintf(&sb, `    template:
      engineVersion: v2
      mergePolicy: Merge
      data:
        # One source of truth: the same generated password the db cluster gives the %[1]s role.
        DATABASE_URL: "postgresql://%[1]s:{{ .db_password | urlquery }}@db-rw.configs.svc.cluster.local:5432/%[1]s"
`, dbName(n.Name))
		}
		sb.WriteString("  data:\n")
		if n.Database {
			fmt.Fprintf(&sb, "    - secretKey: db_password\n      remoteRef:\n        key: %s\n", dbVaultKey(n.Name))
		}
		for _, s := range n.Secrets {
			fmt.Fprintf(&sb, "    - secretKey: %s\n      remoteRef:\n        key: %s\n", s, VaultKey(n.Name, s))
		}
		out[dir+"externalsecret.yaml"] = sb.String()
	}
	out["clusters/keep/apps/"+n.Name+".yaml"] = n.fluxKustomization()
	return out
}

func (n *NewApp) imageLine() string {
	if n.Built() {
		return fmt.Sprintf(`%s:%s # {"$imagepolicy": "flux-system:%s"}`, n.Image, placeholderTag, n.Name)
	}
	return n.Image
}

func (n *NewApp) deployment() string {
	sz := sizes[n.Size]
	var sb strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&sb, f, a...) }
	if n.Built() {
		w("# Tag bumped by Flux image automation (clusters/keep/apps/image-automation.yaml). The first tag is a\n# placeholder until CI has pushed an image; the pod stays in ImagePullBackOff until then.\n")
	}
	w("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  replicas: %d\n  revisionHistoryLimit: 3\n", n.Name, n.Namespace, *n.Replicas)
	if n.PVCSize != "" {
		w("  strategy:\n    type: Recreate # RWO volume\n")
	}
	w("  selector:\n    matchLabels:\n      citadel.dev/app: %s\n  template:\n    metadata:\n      labels:\n        citadel.dev/app: %s\n        app.kubernetes.io/name: %s\n    spec:\n      automountServiceAccountToken: false\n", n.Name, n.Name, n.Name)
	if n.Built() {
		w("      imagePullSecrets:\n        - name: ghcr-pull\n")
	}
	w("      securityContext:\n        runAsNonRoot: true\n        runAsUser: %d\n        runAsGroup: %d\n", n.UID, n.UID)
	if n.PVCSize != "" {
		w("        fsGroup: %d\n", n.UID)
	}
	w("        seccompProfile:\n          type: RuntimeDefault\n")
	w("      containers:\n        - name: %s\n          image: %s\n          ports:\n            - name: http\n              containerPort: %d\n", n.Name, n.imageLine(), n.Port)
	if len(n.Env) > 0 {
		w("          env:\n")
		for _, e := range n.Env {
			w("            - {name: %s, value: %s}\n", e.Name, strconv.Quote(e.Value))
		}
	}
	if len(n.Secrets) > 0 || n.Database {
		w("          envFrom:\n            - secretRef:\n                name: %s\n", n.Name)
	}
	w("          securityContext:\n            allowPrivilegeEscalation: false\n            capabilities:\n              drop: [ALL]\n")
	w("          resources:\n            requests: {cpu: %s, memory: %s}\n            limits: {cpu: %q, memory: %s}\n", sz.reqCPU, sz.reqMem, sz.limCPU, sz.limMem)
	w("          readinessProbe:\n            httpGet: {path: %s, port: http}\n            periodSeconds: 10\n            timeoutSeconds: 3\n", n.HealthPath)
	w("          livenessProbe:\n            httpGet: {path: %s, port: http}\n            initialDelaySeconds: 20\n            periodSeconds: 20\n            timeoutSeconds: 5\n", n.HealthPath)
	w("          volumeMounts:\n")
	if n.PVCSize != "" {
		w("            - {name: data, mountPath: %s}\n", n.DataPath)
	}
	w("            - {name: tmp, mountPath: /tmp}\n      volumes:\n")
	if n.PVCSize != "" {
		w("        - name: data\n          persistentVolumeClaim:\n            claimName: %s\n", n.PVCName())
	}
	w("        - name: tmp\n          emptyDir: {sizeLimit: 256Mi}\n")
	return sb.String()
}

func (n *NewApp) fluxKustomization() string {
	var sb strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&sb, f, a...) }
	w(`apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  interval: 10m
  retryInterval: 2m
  timeout: 5m
  prune: true
  wait: false # replicas-0 or not-yet-built images must not hold up the rest
  sourceRef: {kind: GitRepository, name: flux-system}
  path: ./apps/%[1]s
  # No targetNamespace: db-client and the backup components emit objects into other namespaces.
  dependsOn:
    - name: infra-configs
`, n.Name)
	if n.DBClient {
		w("    - name: db\n")
	}
	w("  components:\n    - ../../components/expose-%s # <- the line Switchboard edits\n    - ../../components/app-netpol\n", n.Mode)
	if n.DBClient {
		w("    - ../../components/db-client\n")
	}
	if n.PVCSize != "" {
		w("    - ../../components/backup-pvc\n    - ../../infrastructure/configs/backup/pvc-backup\n    - ../../infrastructure/configs/backup/restore-if-empty\n")
	}
	w("  postBuild:\n    substitute:\n      APP: %s\n      NAMESPACE: %s\n      PORT: %q\n      HOST: %s\n", n.Name, n.Namespace, strconv.Itoa(n.Port), n.Host)
	if n.Mode == ModeGoogleRestricted {
		w("      ALLOWED_EMAILS: %q\n", n.AllowedEmails)
	}
	if n.PVCSize != "" {
		w("      PVC: %s\n      DATA_VOLUME: data\n      BACKUP_UID: %q\n", n.PVCName(), strconv.Itoa(n.UID))
	}
	return sb.String()
}

// ImageAutomationDocs is what gets appended to image-automation.yaml for a built app.
func (n *NewApp) ImageAutomationDocs() string {
	return fmt.Sprintf(`apiVersion: image.toolkit.fluxcd.io/v1
kind: ImageRepository
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  image: %[2]s
  interval: 5m
  secretRef:
    name: ghcr-pull
---
apiVersion: image.toolkit.fluxcd.io/v1
kind: ImagePolicy
metadata:
  name: %[1]s
  namespace: flux-system
spec:
  imageRepositoryRef:
    name: %[1]s
  filterTags:
    pattern: '^(?P<ts>[0-9]{14})-[a-f0-9]{7}$'
    extract: '$ts'
  policy:
    numerical:
      order: asc
`, n.Name, n.Image)
}

// HomepageEntry is the services.yaml item (8-space indented list item inside the group).
func (n *NewApp) HomepageEntry() string {
	desc := n.Description
	if desc == "" {
		desc = "Added with Switchboard"
	}
	return fmt.Sprintf("        - %s:\n            href: https://%s\n            description: %s\n            icon: mdi-application\n", n.Title, n.Host, desc)
}
