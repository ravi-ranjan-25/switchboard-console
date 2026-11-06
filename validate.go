package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Deliberately simple: local@domain.tld, no quotes, no comments, no IP literals.
var emailRe = regexp.MustCompile(`^[A-Za-z0-9.!#$%&*+/=?^_{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)

// NormalizeEmails validates a comma-separated list and returns it canonical
// (lower-cased, de-duplicated, comma separated, no spaces).
func NormalizeEmails(in string) (string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.Split(in, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if len(p) > 254 || !emailRe.MatchString(p) || strings.Contains(p, "..") {
			return "", fmt.Errorf("invalid email %q", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return "", errors.New("google-restricted needs at least one email")
	}
	if len(out) > 50 {
		return "", errors.New("too many emails (max 50)")
	}
	return strings.Join(out, ","), nil
}

var appNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// modeOpenness orders the modes from most to least exposed.
var modeOpenness = map[string]int{ModePublic: 3, ModeGoogle: 2, ModeGoogleRestricted: 1, ModePrivate: 0}

// ModeAllowed reports whether a protection rule permits mode. A rule is either "mode" (pinned to exactly
// that mode) or "<=mode" (at most that open).
func ModeAllowed(rule, mode string) bool {
	if max, ok := strings.CutPrefix(rule, "<="); ok {
		return modeOpenness[mode] <= modeOpenness[max]
	}
	return mode == rule
}

// RuleLabel is the human text for a rule ("private", "google-restricted or tighter").
func RuleLabel(rule string) string {
	if max, ok := strings.CutPrefix(rule, "<="); ok {
		if max == ModePrivate {
			return max
		}
		return max + " or tighter"
	}
	return rule
}

// CheckProtected enforces the denylist. protected maps app name -> rule (see ModeAllowed).
func CheckProtected(protected map[string]string, app, mode string) error {
	if rule, ok := protected[app]; ok && !ModeAllowed(rule, mode) {
		return fmt.Errorf("%s is protected and must stay %s", app, RuleLabel(rule))
	}
	return nil
}

func CommitMessage(app, from, to string) string {
	return fmt.Sprintf("switchboard: %s %s→%s", app, from, to)
}
