package envfile

import (
	"strings"
	"testing"
)

func TestIsSecret(t *testing.T) {
	declared := map[string]bool{"CUSTOM": true}
	for _, tc := range []struct {
		key, value string
		want       bool
	}{
		{"APP_SECRET", "x", true},
		{"DB_PASSWORD", "x", true},
		{"BACKUP_PASSPHRASE", "x", true},
		{"HTTPCACHE_VARNISH_INVALIDATE_TOKEN", "x", true},
		{"GHCR_TOKEN", "x", true},
		{"JWT_SECRET_KEY", "x", true},
		{"MAILER_DSN", "smtp://u:p@h", true},
		{"CUSTOM", "plain", true},                            // declared by the manifest
		{"DATABASE_URL", "mysql://u:pw@db:3306/x?a=1", true}, // password inside the value
		{"DATABASE_URL", "mysql://db:3306/x", false},         // no credentials
		{"DOMAIN", "app.example.com", false},
		{"PROXY_PORT", "8082", false},
		{"REGISTRY", "ghcr.io/acme", false},
		{"MAILER_CONTACT_RECIPIENTS", "a@example.com", false}, // an address is not a URL password
	} {
		if got := IsSecret(tc.key, tc.value, declared); got != tc.want {
			t.Errorf("IsSecret(%q, %q) = %v, want %v", tc.key, tc.value, got, tc.want)
		}
	}
}

func TestDisplayNeverShowsASecret(t *testing.T) {
	if got := Display("hunter2hunter2", true); strings.Contains(got, "hunter") || !strings.Contains(got, "14 chars") {
		t.Errorf("Display = %q", got)
	}
	if Display("app.example.com", false) != "app.example.com" || Display("", true) != "(empty)" {
		t.Error("a plain value is shown as it is, an empty one as (empty)")
	}
}
