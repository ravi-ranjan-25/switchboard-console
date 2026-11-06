package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppID          int64
	InstallationID int64
	PrivateKey     []byte
	Owner, Repo    string
	Branch         string
	AppsDir        string
	Listen         string
	FluxNamespace  string
	GitRepoName    string
	// Protected is the denylist: "mode" pins an app to exactly that mode, "<=mode" lets it be at most
	// that open (so "<=google-restricted" allows google-restricted and private).
	Protected map[string]string
	// X-Auth-Request-Email (set by oauth2-proxy through the Traefik forwardAuth) must be one of these.
	// Always enforced; the only way off is DevNoAuth for local development.
	AllowedEmails []string
	DevNoAuth     bool
	PollTimeout   time.Duration
	PollInterval  time.Duration
	CommitterMail string
	NoCluster     bool // dev only
	GitHubBaseURL string
	// New-app form
	AppNamespaces []string
	ImagePrefix   string
	DemoCluster   bool // dev only: canned cluster data
	PodNamespaces []string
	TofuDir       string
	// Repo creation (Create a new project). The token is a fine-grained PAT: a GitHub App cannot create repos
	// under a personal account. Empty disables the feature.
	RepoToken []byte
	RepoOwner string
	// Secrets screen (OCI Vault through keep's instance principal). Empty VaultID disables it.
	CompartmentID, VaultID, KeyID string
	// Namespaces whose apps may not be connected to apps of other namespaces from the Network tab.
	IsolatedNamespaces []string
	// The domain whose wildcard certificate, private DNS and sign-in cookie the private and Google-gated modes rely on.
	PrimaryDomain string
}

func loadConfig(get func(string) string, readFile func(string) ([]byte, error)) (*Config, error) {
	def := func(k, d string) string {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v
		}
		return d
	}
	c := &Config{
		Branch:        def("BRANCH", "main"),
		AppsDir:       strings.Trim(def("APPS_DIR", "clusters/keep/apps"), "/"),
		Listen:        def("LISTEN", ":8080"),
		FluxNamespace: def("FLUX_NAMESPACE", "flux-system"),
		GitRepoName:   def("GITREPOSITORY_NAME", "flux-system"),
		CommitterMail: def("COMMITTER_EMAIL", "citadel-switchboard[bot]@users.noreply.github.com"),
		PollTimeout:   3 * time.Minute,
		PollInterval:  3 * time.Second,
		NoCluster:     get("DEV_NO_CLUSTER") == "1",
		GitHubBaseURL: get("GITHUB_API_URL"),
		ImagePrefix:   def("IMAGE_PREFIX", ""),
		TofuDir:       strings.Trim(def("TOFU_DIR", "infra/tofu"), "/"),
		DemoCluster:   get("DEV_DEMO_CLUSTER") == "1",
	}
	c.RepoOwner = def("REPO_OWNER", "")
	c.IsolatedNamespaces = splitList(def("NETWORK_ISOLATED_NAMESPACES", "client"))
	c.CompartmentID, c.VaultID, c.KeyID = strings.TrimSpace(get("OCI_COMPARTMENT_ID")), strings.TrimSpace(get("OCI_VAULT_ID")), strings.TrimSpace(get("OCI_KEY_ID"))
	c.PrimaryDomain = def("PRIMARY_DOMAIN", "example.com")
	c.AppNamespaces = splitList(def("APP_NAMESPACES", "personal,client"))
	c.PodNamespaces = c.AppNamespaces
	repo := strings.TrimSpace(get("REPO"))
	if repo == "" {
		return nil, errors.New("REPO is required: owner/name of the GitOps repository")
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("REPO must be owner/name, got %q", repo)
	}
	c.Owner, c.Repo = parts[0], parts[1]
	if c.ImagePrefix == "" {
		c.ImagePrefix = "ghcr.io/" + c.Owner + "/"
	}
	if !strings.HasSuffix(c.ImagePrefix, "/") {
		c.ImagePrefix += "/"
	}

	var err error
	if c.AppID, err = strconv.ParseInt(get("GITHUB_APP_ID"), 10, 64); err != nil {
		return nil, errors.New("GITHUB_APP_ID missing or not a number")
	}
	if c.InstallationID, err = strconv.ParseInt(get("GITHUB_INSTALLATION_ID"), 10, 64); err != nil {
		return nil, errors.New("GITHUB_INSTALLATION_ID missing or not a number")
	}
	if k := get("GITHUB_PRIVATE_KEY"); k != "" {
		c.PrivateKey = []byte(k)
	} else if p := get("GITHUB_PRIVATE_KEY_PATH"); p != "" {
		if c.PrivateKey, err = readFile(p); err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
	} else {
		return nil, errors.New("set GITHUB_PRIVATE_KEY or GITHUB_PRIVATE_KEY_PATH")
	}

	if k := strings.TrimSpace(get("GITHUB_REPO_TOKEN")); k != "" {
		c.RepoToken = []byte(k)
	} else if p := get("GITHUB_REPO_TOKEN_PATH"); p != "" {
		b, err := readFile(p)
		if err != nil {
			return nil, fmt.Errorf("read repo token: %w", err)
		}
		c.RepoToken = []byte(strings.TrimSpace(string(b)))
	}
	if c.RepoOwner == "" {
		c.RepoOwner = c.Owner
	}

	// PROTECTED_APPS="switchboard<=google-restricted,auth=public,oauth2-proxy=public"
	c.Protected = map[string]string{}
	for _, kv := range strings.Split(def("PROTECTED_APPS", "switchboard<=google-restricted,auth=public,oauth2-proxy=public,headlamp<=google-restricted"), ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		n, m, ok := strings.Cut(kv, "<=")
		rule := "<=" + m
		if !ok {
			if n, m, ok = strings.Cut(kv, "="); ok {
				rule = m
			}
		}
		if !ok || n == "" || !validMode(m) {
			return nil, fmt.Errorf("bad PROTECTED_APPS entry %q", kv)
		}
		c.Protected[n] = rule
	}

	// Identity is default-on: without ALLOWED_EMAILS the app refuses to start, so a missing env var can
	// never silently open the page to anyone who reaches the pod.
	c.DevNoAuth = get("DEV_NO_AUTH") == "1"
	if raw := strings.TrimSpace(get("ALLOWED_EMAILS")); raw != "" {
		norm, err := NormalizeEmails(raw)
		if err != nil {
			return nil, fmt.Errorf("ALLOWED_EMAILS: %w", err)
		}
		c.AllowedEmails = strings.Split(norm, ",")
	} else if !c.DevNoAuth {
		return nil, errors.New("ALLOWED_EMAILS is required (set DEV_NO_AUTH=1 only for local development)")
	}
	return c, nil
}

func osGet(k string) string { return os.Getenv(k) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
