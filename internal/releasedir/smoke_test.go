package releasedir

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/manifest"
)

// TestHelperServer is not a test: smoke tests re-run the test binary as the "release" server.
func TestHelperServer(t *testing.T) {
	if os.Getenv("DK_HELPER_SERVER") != "1" {
		t.Skip("helper process")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/fr", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<link href="/_next/static/css/app.css"><link href="/_next/static/css/second.css">`)
	})
	mux.HandleFunc("/_next/static/css/app.css", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "css") })
	// Language redirect that needs the cookie it sets: without a cookie jar it loops.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if _, err := r.Cookie("lang"); err != nil {
			http.SetCookie(w, &http.Cookie{Name: "lang", Value: "fr", Path: "/"})
			http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = fmt.Fprint(w, "home")
	})
	mux.HandleFunc("/video", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			_, _ = fmt.Fprint(w, "whole")
			return
		}
		w.WriteHeader(http.StatusPartialContent)
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	fmt.Println("helper listening on", os.Getenv("PORT"))
	_ = http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), mux) //nolint:gosec // test server
}

func smokeConfig(paths ...string) *manifest.Smoke {
	cfg := &manifest.Smoke{FollowRedirects: 3, Expect: []int{200, 206}, Paths: paths}
	cfg.Start.Command = fmt.Sprintf("%q -test.run=TestHelperServer", os.Args[0])
	cfg.Start.Env = map[string]string{"DK_HELPER_SERVER": "1"}
	cfg.Start.Port.Range = []int{34000, 34500}
	cfg.Start.RemoveEnv = []string{"NODE_OPTIONS"}
	return cfg
}

func TestSmokePasses(t *testing.T) {
	cfg := smokeConfig("/fr", "/")
	cfg.Discover = []struct {
		From  string `yaml:"from"`
		Match string `yaml:"match"`
		First bool   `yaml:"first"`
	}{{From: "/fr", Match: `/_next/static/[^"\\]*\.css`, First: true}}
	cfg.Ranges = []string{"/video"}

	checks, err := Smoke(t.Context(), t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, c := range checks {
		got[c.Path] = c.Code
	}
	want := map[string]int{"/fr": 200, "/": 200, "/_next/static/css/app.css": 200, "/video": 206}
	if len(got) != len(want) {
		t.Fatalf("checks = %v, want %v (discover must take only the first match)", got, want)
	}
	for p, code := range want {
		if got[p] != code {
			t.Errorf("%s = %d, want %d", p, got[p], code)
		}
	}
}

func TestSmokeFailsOnUnexpectedCode(t *testing.T) {
	checks, err := Smoke(t.Context(), t.TempDir(), smokeConfig("/fr", "/boom", "/missing"))
	if err == nil || !strings.Contains(err.Error(), "2 of 3 smoke checks failed") {
		t.Fatalf("err = %v", err)
	}
	bad := 0
	for _, c := range checks {
		if !c.OK {
			bad++
		}
	}
	if bad != 2 {
		t.Errorf("%d failed checks, want 2", bad)
	}
}

func TestSmokeRedirectLoopFails(t *testing.T) {
	if _, err := Smoke(t.Context(), t.TempDir(), smokeConfig("/fr", "/loop")); err == nil {
		t.Error("a redirect loop must fail the smoke test")
	}
}

func TestSmokeServerThatExitsImmediately(t *testing.T) {
	cfg := smokeConfig("/")
	cfg.Start.Command = "echo cannot start >&2; exit 3"
	_, err := Smoke(t.Context(), t.TempDir(), cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Errorf("err = %v, want the server output", err)
	}
}

func TestSmokeEnvironment(t *testing.T) {
	t.Setenv("NODE_OPTIONS", "--inspect")
	t.Setenv("PORT", "1")
	env := strings.Join(smokeEnv(smokeConfig("/"), 3333), "\n")
	if strings.Contains(env, "NODE_OPTIONS") {
		t.Error("NODE_OPTIONS must be removed")
	}
	if !strings.Contains(env, "PORT=3333") || strings.Contains(env, "PORT=1\n") {
		t.Errorf("PORT must be the chosen port: %s", env)
	}
}
