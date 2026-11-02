package envfile

import (
	"fmt"
	"regexp"
)

// secretName matches the names that usually hold a secret. It is deliberately broad: showing a public key as a secret costs
// nothing, showing a password as a plain value does.
var secretName = regexp.MustCompile(`(?i)(SECRET|PASSWORD|PASSPHRASE|TOKEN|PRIVATE|CREDENTIAL|API_?KEY|_KEY$|^KEY$|DSN$)`)

// userinfo matches a URL that carries a password: scheme://user:password@host.
var userinfo = regexp.MustCompile(`://[^/@\s:]*:[^/@\s]+@`)

// IsSecret says whether a variable must not be displayed: it is declared secret by the manifest, its name looks like a
// secret, or its value is a URL with a password in it (DATABASE_URL, a mailer DSN).
func IsSecret(key, value string, declared map[string]bool) bool {
	return declared[key] || secretName.MatchString(key) || userinfo.MatchString(value)
}

// Display renders a value for a listing: the value itself, or a placeholder that gives its length and nothing else.
func Display(value string, secret bool) string {
	switch {
	case value == "":
		return "(empty)"
	case secret:
		return fmt.Sprintf("<secret, %d chars>", len(value))
	}
	return value
}
