package main

import "testing"

func TestModes(t *testing.T) {
	for _, m := range []string{"public", "google", "google-restricted", "private"} {
		if !validMode(m) {
			t.Errorf("%s should be valid", m)
		}
	}
	for _, m := range []string{"", "Public", "restricted", "private ", "public;rm"} {
		if validMode(m) {
			t.Errorf("%q should be invalid", m)
		}
	}
}

func TestEmails(t *testing.T) {
	got, err := NormalizeEmails(" A@X.com, b@y.co ,a@x.com")
	if err != nil || got != "a@x.com,b@y.co" {
		t.Errorf("got %q %v", got, err)
	}
	for _, bad := range []string{"", " , ", "nope", "a@b", "a@@b.com", "a b@c.com", "a@b.com;c", `a"@b.com`, "a..b@c.com", "a@-b.com", "a@b..com"} {
		if _, err := NormalizeEmails(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDenylist(t *testing.T) {
	p := map[string]string{"switchboard": "private", "auth": "public"}
	if CheckProtected(p, "switchboard", "public") == nil || CheckProtected(p, "switchboard", "google") == nil {
		t.Error("switchboard left private")
	}
	if CheckProtected(p, "switchboard", "private") != nil {
		t.Error("staying private should pass")
	}
	if CheckProtected(p, "auth", "private") == nil {
		t.Error("auth made private")
	}
	if CheckProtected(p, "n8n", "private") != nil {
		t.Error("unprotected app refused")
	}
}

func TestDenylistCeiling(t *testing.T) {
	p := map[string]string{"switchboard": "<=google-restricted"}
	for _, m := range []string{"public", "google"} {
		if CheckProtected(p, "switchboard", m) == nil {
			t.Errorf("switchboard allowed %s", m)
		}
	}
	for _, m := range []string{"google-restricted", "private"} {
		if err := CheckProtected(p, "switchboard", m); err != nil {
			t.Errorf("%s refused: %v", m, err)
		}
	}
}

func TestCommitMessage(t *testing.T) {
	if got := CommitMessage("planka", "google", "private"); got != "switchboard: planka google→private" {
		t.Error(got)
	}
}

func TestAppName(t *testing.T) {
	for _, bad := range []string{"", "../x", "A", "a/b", "-a", "a.yaml"} {
		if appNameRe.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConfig(t *testing.T) {
	env := map[string]string{"GITHUB_APP_ID": "1", "GITHUB_INSTALLATION_ID": "2", "GITHUB_PRIVATE_KEY_PATH": "/k", "ALLOWED_EMAILS": "ME@example.com", "REPO": "example-owner/citadel"}
	c, err := loadConfig(func(k string) string { return env[k] }, func(string) ([]byte, error) { return []byte("pem"), nil })
	if err != nil || c.Owner != "example-owner" || c.Repo != "citadel" || c.ImagePrefix != "ghcr.io/example-owner/" || c.AppsDir != "clusters/keep/apps" || string(c.PrivateKey) != "pem" || c.Protected["switchboard"] != "<=google-restricted" || len(c.AllowedEmails) != 1 || c.AllowedEmails[0] != "me@example.com" {
		t.Errorf("%+v %v", c, err)
	}
	env2 := map[string]string{"GITHUB_APP_ID": "1", "GITHUB_INSTALLATION_ID": "2", "GITHUB_PRIVATE_KEY_PATH": "/k", "REPO": "example-owner/citadel"}
	if _, err := loadConfig(func(k string) string { return env2[k] }, func(string) ([]byte, error) { return []byte("pem"), nil }); err == nil {
		t.Error("missing ALLOWED_EMAILS accepted")
	}
	env2["DEV_NO_AUTH"] = "1"
	if c, err := loadConfig(func(k string) string { return env2[k] }, func(string) ([]byte, error) { return []byte("pem"), nil }); err != nil || !c.DevNoAuth {
		t.Errorf("dev no auth: %v", err)
	}
	delete(env, "GITHUB_APP_ID")
	if _, err := loadConfig(func(k string) string { return env[k] }, nil); err == nil {
		t.Error("missing app id accepted")
	}
}
