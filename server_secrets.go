package main

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The Secrets screen. App secrets only (vault names app-<app>--<key>), for apps that exist in citadel and are not protected.
// Values go in through POST bodies and come out ONLY through the explicit, rate-limited, audited Reveal.

type DeclRef struct {
	App    string `json:"app"`
	ES     string `json:"es"`
	Target string `json:"target"`
}

type SecretRow struct {
	SecretMeta
	Declared  []DeclRef `json:"declared"`
	Ready     string    `json:"ready,omitempty"` // True | False | Unknown | "" (not declared)
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
	ExpiresIn *int      `json:"expires_in,omitempty"` // days; negative = expired
}

type MissingSecret struct {
	App, Name, Key, ES string
}

type SecretsResp struct {
	Enabled bool            `json:"enabled"`
	Rows    []SecretRow     `json:"rows"`
	Missing []MissingSecret `json:"missing"`
	Apps    []string        `json:"apps"`
}

func (s *Server) eligibleApps(sn *Snapshot) map[string]*AppInfo {
	out := map[string]*AppInfo{}
	for _, a := range loadApps(sn, s.cfg.AppsDir) {
		if a.Managed && !protectedApp(s.cfg, a) {
			out[a.Name] = a
		}
	}
	return out
}

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		writeJSON(w, 200, SecretsResp{Rows: []SecretRow{}, Missing: []MissingSecret{}, Apps: []string{}})
		return
	}
	sn, err := s.snapshot(r.Context(), false)
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": "could not read the repo from GitHub"})
		return
	}
	metas, err := s.vault.List(r.Context())
	if err != nil {
		s.log.Error("vault list", "err", err.Error())
		writeJSON(w, 502, map[string]any{"error": "could not list the vault"})
		return
	}
	apps := s.eligibleApps(sn)
	resp := SecretsResp{Enabled: true, Rows: []SecretRow{}, Missing: []MissingSecret{}, Apps: []string{}}
	for n := range apps {
		resp.Apps = append(resp.Apps, n)
	}
	sort.Strings(resp.Apps)

	es := map[string]map[string]SecretStatus{}
	declared := map[string][]DeclRef{}
	for _, a := range apps {
		if _, ok := es[a.Namespace]; !ok && contains(s.cfg.PodNamespaces, a.Namespace) {
			m, _ := s.cluster.ExternalSecrets(r.Context(), a.Namespace)
			es[a.Namespace] = m
		}
		for _, si := range a.Secrets {
			for _, k := range si.Keys {
				declared[k] = append(declared[k], DeclRef{App: a.Name, ES: si.Name, Target: si.Target})
			}
		}
	}
	have := map[string]bool{}
	now := time.Now().UTC()
	for _, m := range metas {
		if _, ok := apps[m.App]; !ok {
			continue // not an eligible app: invisible here
		}
		have[m.Name] = true
		row := SecretRow{SecretMeta: m, Declared: declared[m.Name]}
		if row.Declared == nil {
			row.Declared = []DeclRef{}
		}
		if len(row.Declared) > 0 {
			d := row.Declared[0]
			if st, ok := es[apps[d.App].Namespace][d.ES]; ok {
				row.Ready, row.Reason, row.Message = st.Ready, st.Reason, st.Message
			} else {
				row.Ready = "Unknown"
			}
		}
		if m.Expires != "" {
			if t, err := time.Parse("2006-01-02", m.Expires); err == nil {
				d := int(t.Sub(now.Truncate(24*time.Hour)).Hours() / 24)
				row.ExpiresIn = &d
			}
		}
		resp.Rows = append(resp.Rows, row)
	}
	for name, refs := range declared {
		if !have[name] {
			if app, key, ok := ParseSecretName(name); ok {
				resp.Missing = append(resp.Missing, MissingSecret{App: app, Name: name, Key: key, ES: refs[0].ES})
			}
		}
	}
	sort.Slice(resp.Missing, func(i, j int) bool { return resp.Missing[i].Name < resp.Missing[j].Name })
	writeJSON(w, 200, resp)
}

type secretReq struct {
	App     string `json:"app"`
	Key     string `json:"key"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	Expires string `json:"expires"`
}

// normKey turns API_KEY or api-key into api-key.
func normKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), "_", "-"))
}

func validExpiry(e string) error {
	if e == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", e)
	if err != nil {
		return errors.New("expiry must be YYYY-MM-DD")
	}
	if t.Before(time.Now().UTC().Truncate(24 * time.Hour)) {
		return errors.New("expiry is in the past")
	}
	return nil
}

func (s *Server) resolveSecretTarget(ctx context.Context, req *secretReq, create bool) (string, *AppInfo, int, string) {
	sn, err := s.snapshot(ctx, false)
	if err != nil {
		return "", nil, http.StatusBadGateway, "could not read the repo from GitHub"
	}
	var name string
	if create {
		k := normKey(req.Key)
		name = "app-" + req.App + "--" + k
		if _, _, ok := ParseSecretName(name); !ok {
			return "", nil, http.StatusBadRequest, "key must be letters, digits and dashes (like api-key or API_KEY)"
		}
	} else {
		name = req.Name
	}
	app, _, ok := ParseSecretName(name)
	if !ok {
		return "", nil, http.StatusBadRequest, "not an app secret name (app-<app>--<key>)"
	}
	a := s.eligibleApps(sn)[app]
	if a == nil {
		return "", nil, http.StatusForbidden, "app " + app + " is unknown, not a standard app, or protected"
	}
	return name, a, 0, ""
}

func (s *Server) handleSecretWrite(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) { writeJSON(w, code, map[string]any{"error": msg}) }
	if s.vault == nil {
		fail(http.StatusNotFound, "the Secrets screen is not configured")
		return
	}
	var req secretReq
	if !s.decodePost(w, r, &req) {
		return
	}
	create := strings.HasSuffix(r.URL.Path, "/create")
	login := s.who(r)
	switch {
	case req.Value == "" || len(req.Value) > maxSecret || strings.ContainsRune(req.Value, 0):
		fail(http.StatusBadRequest, "value is empty, too long (16 KiB max) or contains a NUL byte")
		return
	}
	if err := validExpiry(req.Expires); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	name, a, code, msg := s.resolveSecretTarget(r.Context(), &req, create)
	if code != 0 {
		s.audit(login, "secret", name, "refused", msg)
		fail(code, msg)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if create {
		if err := s.vault.Create(ctx, name, []byte(req.Value), req.Expires); err != nil {
			if errors.Is(err, ErrSecretExists) {
				fail(http.StatusConflict, name+" already exists; use Update to add a new version")
				return
			}
			s.log.Error("vault create", "name", name, "err", err.Error())
			s.audit(login, "secret-create", name, "error", "")
			fail(http.StatusBadGateway, "the vault refused the write (check the IAM policy for app-* secrets)")
			return
		}
		s.audit(login, "secret-create", name, "ok", "v1")
		writeJSON(w, 201, map[string]any{"name": name, "version": 1, "declared": s.isDeclared(r.Context(), a, name)})
		return
	}
	v, err := s.vault.AddVersion(ctx, name, []byte(req.Value), req.Expires)
	if err != nil {
		if errors.Is(err, ErrSecretMissing) {
			fail(http.StatusNotFound, name+" does not exist; create it first")
			return
		}
		s.log.Error("vault update", "name", name, "err", err.Error())
		s.audit(login, "secret-update", name, "error", "")
		fail(http.StatusBadGateway, "the vault refused the write (check the IAM policy for app-* secrets)")
		return
	}
	s.audit(login, "secret-update", name, "ok", "v"+strconv.Itoa(int(v)))
	writeJSON(w, 200, map[string]any{"name": name, "version": v, "declared": s.isDeclared(r.Context(), a, name),
		"note": "External Secrets refreshes within an hour. Pods read the value at start: restart the app (stop and start) to pick it up now."})
}

func (s *Server) isDeclared(ctx context.Context, a *AppInfo, name string) bool {
	sn, err := s.snapshot(ctx, true)
	if err != nil {
		return false
	}
	if cur := findApp(loadApps(sn, s.cfg.AppsDir), a.Name); cur != nil {
		for _, si := range cur.Secrets {
			if contains(si.Keys, name) {
				return true
			}
		}
	}
	return false
}

// allowReveal: at most 10 reveals per minute, in total.
func (s *Server) allowReveal() bool {
	s.revealM.Lock()
	defer s.revealM.Unlock()
	cut := time.Now().Add(-time.Minute)
	keep := s.reveals[:0]
	for _, t := range s.reveals {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	s.reveals = keep
	if len(s.reveals) >= 10 {
		return false
	}
	s.reveals = append(s.reveals, time.Now())
	return true
}

func (s *Server) handleSecretReveal(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, msg string) { writeJSON(w, code, map[string]any{"error": msg}) }
	if s.vault == nil {
		fail(http.StatusNotFound, "the Secrets screen is not configured")
		return
	}
	var req secretReq
	if !s.decodePost(w, r, &req) {
		return
	}
	login := s.who(r)
	name, _, code, msg := s.resolveSecretTarget(r.Context(), &secretReq{Name: req.Name}, false)
	if code != 0 {
		s.audit(login, "secret-reveal", req.Name, "refused", msg)
		fail(code, msg)
		return
	}
	if !s.allowReveal() {
		s.audit(login, "secret-reveal", name, "rate-limited", "")
		fail(http.StatusTooManyRequests, "too many reveals; wait a minute")
		return
	}
	val, ver, err := s.vault.Reveal(r.Context(), name)
	if err != nil {
		if errors.Is(err, ErrSecretMissing) {
			fail(http.StatusNotFound, "secret not found")
			return
		}
		s.log.Error("vault reveal", "name", name, "err", err.Error())
		fail(http.StatusBadGateway, "could not read the secret")
		return
	}
	// The audit line records who revealed which secret and version, never the value.
	s.audit(login, "secret-reveal", name, "ok", "v"+strconv.Itoa(int(ver)))
	w.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"name": name, "version": ver, "seconds": 30}
	if utf8.Valid(val) {
		out["value"], out["encoding"] = string(val), "utf8"
	} else {
		out["value"], out["encoding"] = encodeB64(val), "base64"
	}
	writeJSON(w, 200, out)
}

func encodeB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
