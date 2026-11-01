package secrets

import (
	"regexp"
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := Password(24)
		if err != nil {
			t.Fatal(err)
		}
		if len(p) != 24 || !regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(p) || strings.ContainsAny(p, "0OIl1") {
			t.Fatalf("bad password %q", p)
		}
		if seen[p] {
			t.Fatalf("duplicate password %q", p)
		}
		seen[p] = true
	}
	if p, _ := Password(3); len(p) != 12 {
		t.Errorf("short request gave %d characters, want 12", len(p))
	}
}

func TestHex(t *testing.T) {
	h, err := Hex(16)
	if err != nil || len(h) != 32 || !regexp.MustCompile(`^[0-9a-f]+$`).MatchString(h) {
		t.Errorf("Hex(16) = %q, %v", h, err)
	}
	if _, err := Hex(0); err == nil {
		t.Error("Hex(0) must fail")
	}
}
