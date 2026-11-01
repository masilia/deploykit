package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEnsurePath(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unset (cron without PATH)": {"", defaultPath},
		"cron's usual":              {"/usr/bin:/bin", "/usr/bin:/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin"},
		"custom dirs stay first":    {"/home/me/bin:/usr/bin", "/home/me/bin:/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"},
		"complete is unchanged":     {defaultPath, defaultPath},
		"empty elements dropped":    {"::/usr/bin::", "/usr/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/bin"},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"PATH": tc.in}
			err := ensurePath(func(k string) string { return env[k] }, func(k, v string) error { env[k] = v; return nil })
			if err != nil || env["PATH"] != tc.want {
				t.Errorf("PATH = %q, want %q (err %v)", env["PATH"], tc.want, err)
			}
		})
	}
}

// The failure seen on the pilot: a process started with no PATH at all cannot find `cat`.
func TestACommandStartedWithoutPathStillFindsItsTools(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := exec.LookPath("cat"); err == nil {
		t.Skip("cat is found with an empty PATH on this system")
	}
	if err := ensurePath(os.Getenv, os.Setenv); err != nil {
		t.Fatal(err)
	}
	if p, err := exec.LookPath("cat"); err != nil || !strings.HasSuffix(p, "/cat") {
		t.Errorf("cat not found after ensurePath: %q, %v", p, err)
	}
}
