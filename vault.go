package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/secrets"
	ocivault "github.com/oracle/oci-go-sdk/v65/vault"
)

// The Secrets screen manages APP secrets only: vault names app-<app>--<kebab>. Infra secrets (keep--, shared--,
// watchtower--) are out of reach by construction: the name check below, plus the IAM policy (see deploy/README.md)
// that should only allow secret management for the app-* names. Values are never logged, never cached, never
// echoed back except by an explicit, audited Reveal.

const (
	tagSource  = "citadel_source"
	tagRotated = "citadel_rotated"
	tagExpires = "citadel_expires"
	maxSecret  = 16 << 10
)

var vaultNameRe = regexp.MustCompile(`^app-([a-z0-9]+(?:-[a-z0-9]+)*)--([a-z0-9]+(?:-[a-z0-9]+)*)$`)

// ParseSecretName splits app-<app>--<key>. ok is false for anything outside the app scope.
func ParseSecretName(name string) (app, key string, ok bool) {
	m := vaultNameRe.FindStringSubmatch(name)
	if m == nil || len(name) > 120 {
		return "", "", false
	}
	return m[1], m[2], true
}

var ErrSecretExists = errors.New("secret already exists")
var ErrSecretMissing = errors.New("secret not found")

type SecretMeta struct {
	Name    string    `json:"name"`
	App     string    `json:"app"`
	Key     string    `json:"key"`
	Version int64     `json:"version"`
	Created time.Time `json:"created"`
	Rotated string    `json:"rotated,omitempty"` // YYYY-MM-DD, tag
	Expires string    `json:"expires,omitempty"` // YYYY-MM-DD, tag
	State   string    `json:"state"`
	Source  string    `json:"source,omitempty"`
}

// Vault is the minimal secret store interface. Implementations must never return a value except from Reveal.
type Vault interface {
	List(ctx context.Context) ([]SecretMeta, error)
	Create(ctx context.Context, name string, value []byte, expires string) error
	AddVersion(ctx context.Context, name string, value []byte, expires string) (int64, error)
	Reveal(ctx context.Context, name string) (value []byte, version int64, err error)
}

// ---- OCI Vault ---------------------------------------------------------------------------------------

type ociVault struct {
	vc    ocivault.VaultsClient
	sc    secrets.SecretsClient
	comp  string
	vault string
	key   string
}

// NewOCIVault uses keep's instance principal: no API key exists anywhere. Compartment, vault and master key ids
// are plain configuration (OCIDs are not secret).
func NewOCIVault(compartmentID, vaultID, keyID string) (Vault, error) {
	if compartmentID == "" || vaultID == "" || keyID == "" {
		return nil, errors.New("OCI_COMPARTMENT_ID, OCI_VAULT_ID and OCI_KEY_ID are required")
	}
	prov, err := auth.InstancePrincipalConfigurationProvider()
	if err != nil {
		return nil, fmt.Errorf("instance principal: %w", err)
	}
	vc, err := ocivault.NewVaultsClientWithConfigurationProvider(prov)
	if err != nil {
		return nil, err
	}
	sc, err := secrets.NewSecretsClientWithConfigurationProvider(prov)
	if err != nil {
		return nil, err
	}
	return &ociVault{vc: vc, sc: sc, comp: compartmentID, vault: vaultID, key: keyID}, nil
}

func (o *ociVault) summaries(ctx context.Context, name string) ([]ocivault.SecretSummary, error) {
	var out []ocivault.SecretSummary
	var page *string
	for {
		req := ocivault.ListSecretsRequest{CompartmentId: &o.comp, VaultId: &o.vault, Page: page, Limit: common.Int(100)}
		if name != "" {
			req.Name = &name
		}
		r, err := o.vc.ListSecrets(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, s := range r.Items {
			if s.SecretName == nil || !strings.HasPrefix(*s.SecretName, "app-") {
				continue
			}
			if st := s.LifecycleState; st != ocivault.SecretSummaryLifecycleStateActive && st != ocivault.SecretSummaryLifecycleStateUpdating && st != ocivault.SecretSummaryLifecycleStateCreating {
				continue
			}
			out = append(out, s)
		}
		if r.OpcNextPage == nil {
			return out, nil
		}
		page = r.OpcNextPage
	}
}

func (o *ociVault) version(ctx context.Context, id *string) int64 {
	r, err := o.vc.ListSecretVersions(ctx, ocivault.ListSecretVersionsRequest{SecretId: id, Limit: common.Int(1),
		SortBy: ocivault.ListSecretVersionsSortByVersionNumber, SortOrder: ocivault.ListSecretVersionsSortOrderDesc})
	if err != nil || len(r.Items) == 0 || r.Items[0].VersionNumber == nil {
		return 0
	}
	return *r.Items[0].VersionNumber
}

func (o *ociVault) List(ctx context.Context) ([]SecretMeta, error) {
	sums, err := o.summaries(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []SecretMeta
	for _, s := range sums {
		app, key, ok := ParseSecretName(*s.SecretName)
		if !ok {
			continue // app-* but not app-<app>--<key>: not ours to show
		}
		m := SecretMeta{Name: *s.SecretName, App: app, Key: key, State: string(s.LifecycleState),
			Rotated: s.FreeformTags[tagRotated], Expires: s.FreeformTags[tagExpires], Source: s.FreeformTags[tagSource], Version: o.version(ctx, s.Id)}
		if s.TimeCreated != nil {
			m.Created = s.TimeCreated.Time
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func tagsFor(expires string) map[string]string {
	t := map[string]string{tagSource: "switchboard", tagRotated: time.Now().UTC().Format("2006-01-02")}
	if expires != "" {
		t[tagExpires] = expires
	}
	return t
}

func content(value []byte) ocivault.SecretContentDetails {
	return ocivault.Base64SecretContentDetails{Content: common.String(base64.StdEncoding.EncodeToString(value))}
}

func (o *ociVault) Create(ctx context.Context, name string, value []byte, expires string) error {
	if ex, err := o.summaries(ctx, name); err != nil {
		return err
	} else if len(ex) > 0 {
		return ErrSecretExists
	}
	_, err := o.vc.CreateSecret(ctx, ocivault.CreateSecretRequest{CreateSecretDetails: ocivault.CreateSecretDetails{
		CompartmentId: &o.comp, VaultId: &o.vault, KeyId: &o.key, SecretName: &name,
		Description: common.String("citadel app secret (Switchboard)"), SecretContent: content(value), FreeformTags: tagsFor(expires)}})
	return err
}

func (o *ociVault) idOf(ctx context.Context, name string) (*string, error) {
	ex, err := o.summaries(ctx, name)
	if err != nil {
		return nil, err
	}
	for _, s := range ex {
		if *s.SecretName == name {
			return s.Id, nil
		}
	}
	return nil, ErrSecretMissing
}

func (o *ociVault) AddVersion(ctx context.Context, name string, value []byte, expires string) (int64, error) {
	id, err := o.idOf(ctx, name)
	if err != nil {
		return 0, err
	}
	if _, err := o.vc.UpdateSecret(ctx, ocivault.UpdateSecretRequest{SecretId: id, UpdateSecretDetails: ocivault.UpdateSecretDetails{
		SecretContent: content(value), FreeformTags: tagsFor(expires)}}); err != nil {
		return 0, err
	}
	return o.version(ctx, id), nil
}

func (o *ociVault) Reveal(ctx context.Context, name string) ([]byte, int64, error) {
	id, err := o.idOf(ctx, name)
	if err != nil {
		return nil, 0, err
	}
	r, err := o.sc.GetSecretBundle(ctx, secrets.GetSecretBundleRequest{SecretId: id, Stage: secrets.GetSecretBundleStageCurrent})
	if err != nil {
		return nil, 0, err
	}
	c, ok := r.SecretBundleContent.(secrets.Base64SecretBundleContentDetails)
	if !ok || c.Content == nil {
		return nil, 0, errors.New("unexpected secret content type")
	}
	b, err := base64.StdEncoding.DecodeString(*c.Content)
	var v int64
	if r.VersionNumber != nil {
		v = *r.VersionNumber
	}
	return b, v, err
}

// ---- in-memory vault (tests and the local demo) ----------------------------------------------------------

type memVault struct {
	mu   sync.Mutex
	data map[string]*memSecret
	Fail error
}

type memSecret struct {
	versions [][]byte
	meta     SecretMeta
}

func NewMemVault() *memVault { return &memVault{data: map[string]*memSecret{}} }

func (m *memVault) List(context.Context) ([]SecretMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SecretMeta
	for _, s := range m.data {
		x := s.meta
		x.Version = int64(len(s.versions))
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memVault) Create(_ context.Context, name string, value []byte, expires string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if _, ok := m.data[name]; ok {
		return ErrSecretExists
	}
	app, key, _ := ParseSecretName(name)
	m.data[name] = &memSecret{versions: [][]byte{append([]byte(nil), value...)}, meta: SecretMeta{Name: name, App: app, Key: key, State: "ACTIVE",
		Created: time.Now().UTC(), Rotated: time.Now().UTC().Format("2006-01-02"), Expires: expires, Source: "switchboard"}}
	return nil
}

func (m *memVault) AddVersion(_ context.Context, name string, value []byte, expires string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return 0, m.Fail
	}
	s, ok := m.data[name]
	if !ok {
		return 0, ErrSecretMissing
	}
	s.versions = append(s.versions, append([]byte(nil), value...))
	s.meta.Rotated, s.meta.Expires = time.Now().UTC().Format("2006-01-02"), expires
	return int64(len(s.versions)), nil
}

func (m *memVault) Reveal(_ context.Context, name string) ([]byte, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.data[name]
	if !ok {
		return nil, 0, ErrSecretMissing
	}
	return append([]byte(nil), s.versions[len(s.versions)-1]...), int64(len(s.versions)), nil
}

// Value returns the current value (tests only; never wired to an endpoint).
func (m *memVault) value(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.data[name]; ok {
		return string(s.versions[len(s.versions)-1])
	}
	return ""
}
