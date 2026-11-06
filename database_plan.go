package main

import (
	"errors"
	"fmt"
)

type dbEdit struct {
	path string
	fn   func([]byte) ([]byte, error)
}

// dbPlatformEdits are the edits outside the app (db cluster + bootstrap). Order: bootstrap first (inert), then the db files.
func dbPlatformEdits(app string) []dbEdit {
	return []dbEdit{
		{"bootstrap/secrets.catalog.yaml", func(b []byte) ([]byte, error) { return AddCatalogGenerated(b, app) }},
		{"bootstrap/vault/mapping.yaml", func(b []byte) ([]byte, error) { return AddMappingLine(b, app) }},
		{"apps/db/externalsecret.yaml", func(b []byte) ([]byte, error) { return AddDBExternalSecret(b, app) }},
		{"apps/db/cluster.yaml", func(b []byte) ([]byte, error) { return AddDBRole(b, app) }},
		{"apps/db/databases.yaml", func(b []byte) ([]byte, error) { return AddDBDatabase(b, app) }},
		{"apps/db/dump-cronjob.yaml", func(b []byte) ([]byte, error) { return AddDumpDB(b, app) }},
	}
}

// dbChanges turns edits into Changes, skipping the ones that are already in place (idempotent).
func dbChanges(s *Snapshot, edits []dbEdit) ([]Change, error) {
	var out []Change
	for _, e := range edits {
		e := e
		old, ok := s.Files[e.path]
		if !ok {
			return nil, fmt.Errorf("%s not found in the repo (is the platform layout different?)", e.path)
		}
		nw, err := e.fn(old)
		if errors.Is(err, ErrAlready) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.path, err)
		}
		out = append(out, Change{Path: e.path, Action: ActUpdate, Old: old, New: nw, Rebuild: func(b []byte) ([]byte, error) {
			r, err := e.fn(b)
			if errors.Is(err, ErrAlready) {
				return b, nil
			}
			return r, err
		}})
	}
	return out, nil
}

// PlanAddDatabase gives an existing app its own database on the shared cluster.
func PlanAddDatabase(s *Snapshot, cfg *Config, app string) (*Plan, *AppInfo, error) {
	a, err := manageable(s, cfg, app)
	if err != nil {
		return nil, nil, err
	}
	if len(a.Secrets) != 1 {
		return nil, nil, fmt.Errorf("%s needs exactly one ExternalSecret to carry DATABASE_URL (found %d); add one first", app, len(a.Secrets))
	}
	edits := dbPlatformEdits(app)
	edits = append(edits,
		dbEdit{a.Secrets[0].File, func(b []byte) ([]byte, error) { return SetDatabaseURL(b, app) }},
		dbEdit{a.KFile, EnsureDBClient})
	ch, err := dbChanges(s, edits)
	if err != nil {
		return nil, nil, err
	}
	if len(ch) == 0 {
		return nil, nil, errors.New(app + " already has its database wired up")
	}
	return &Plan{Kind: "add-database", App: app, Message: "switchboard: add database for " + app, Changes: ch}, a, nil
}

// CheckDatabase verifies, in the end state, every piece of the wiring for app.
func CheckDatabase(s *Snapshot, cfg *Config, app string) []Check {
	c := &checker{}
	has := func(path string, fn func([]byte) ([]byte, error), what string) {
		b, ok := s.Files[path]
		if !ok {
			c.fail("database", "%s missing", path)
			return
		}
		if _, err := fn(b); errors.Is(err, ErrAlready) {
			c.ok("database", "%s", what)
		} else {
			c.fail("database", "%s is not wired in %s", what, path)
		}
	}
	for _, e := range dbPlatformEdits(app) {
		e := e
		what := map[string]string{"bootstrap/secrets.catalog.yaml": "generated secret " + dbSecretKey(app), "bootstrap/vault/mapping.yaml": "vault name " + dbVaultKey(app),
			"apps/db/externalsecret.yaml": "ExternalSecret db-" + app, "apps/db/cluster.yaml": "managed role " + dbName(app),
			"apps/db/databases.yaml": "Database " + dbName(app), "apps/db/dump-cronjob.yaml": "nightly dump of " + dbName(app)}[e.path]
		has(e.path, e.fn, what)
	}
	if a := loadAppFrom(s, cfg, app); a != nil {
		if len(a.Secrets) == 1 {
			has(a.Secrets[0].File, func(b []byte) ([]byte, error) { return SetDatabaseURL(b, app) }, "DATABASE_URL from the generated password")
		}
		has(a.KFile, EnsureDBClient, "db-client component and dependsOn db")
	}
	return c.out
}

const dbSecretsNote = "Switchboard cannot create the password. After the commits are in, run in your shell: make secrets (generates the new password) and then make vault-push. Until then the new database role and the app's DATABASE_URL show as not Ready."
