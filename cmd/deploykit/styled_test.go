package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func styledHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(newProjectDir(t, cliManifest))
	h.app.forceStyled = true
	return h
}

func TestStyledValidate(t *testing.T) {
	h := styledHarness(t)
	if err := h.run("validate"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"◆ validate", "✔ demo  release-dir · pm2", "✔ 1 target(s)  demo"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestStyledStatusOfAPM2Target(t *testing.T) {
	h := styledHarness(t)
	releasesOnServer(h)
	if err := h.run("status", "-t", "demo"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"◆ status", "RELEASE", "STATUS", "COMMIT", "› ", "20261026000000", "live", "abc1234", "superseded"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

func TestStyledFleetStatusAndHealthcheck(t *testing.T) {
	h := styledHarness(t)
	addTarget(t, filepath.Dir(h.app.manifestPath), "other")
	releasesOnServer(h)
	if err := h.run("fleet", "status", "--all"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	for _, want := range []string{"TARGET", "LIVE", "STATE", "demo", "other", "OK", "──"} {
		if !strings.Contains(h.out.String(), want) {
			t.Errorf("fleet status lacks %q:\n%s", want, h.out)
		}
	}
	h.out.Reset()
	if err := h.run("fleet", "healthcheck", "--all"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if out := h.out.String(); !strings.Contains(out, "◆ healthcheck") || !strings.Contains(out, "✔ demo  healthy") {
		t.Errorf("healthcheck output:\n%s", out)
	}
}

func TestStyledDeployIsFramedAndFoldsTheOutput(t *testing.T) {
	h := styledHarness(t)
	if err := h.run("deploy", "--target", "demo"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	out := h.out.String()
	for _, want := range []string{"◆ deploy", "demo → demo", "✔ build", "✔ activate", "✔ deploy done in"} {
		if !strings.Contains(out, want) {
			t.Errorf("deploy output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[") && strings.Contains(out, "] ›") {
		t.Errorf("the styled layout has no timestamps:\n%s", out)
	}
}

func TestStyledDeployDryRunSaysNothingChanged(t *testing.T) {
	h := styledHarness(t)
	if err := h.run("deploy", "--dry-run"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if out := h.out.String(); !strings.Contains(out, "◌ upload") || !strings.Contains(out, "deploy: dry run, nothing was changed") {
		t.Errorf("dry run output:\n%s", out)
	}
}

func TestStyledDeployFailureIsReported(t *testing.T) {
	dir := newProjectDir(t, strings.Replace(cliManifest, `command: "mkdir -p out && printf ok > out/index.txt"`, `command: "echo about to fail; exit 3"`, 1))
	h := newHarness(dir)
	h.app.forceStyled = true
	err := h.run("deploy", "--target", "demo")
	if err == nil {
		t.Fatal("the build fails")
	}
	out := h.out.String()
	for _, want := range []string{"✖ build", "about to fail", "✖ deploy failed after"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure output lacks %q:\n%s", want, out)
		}
	}
}

func TestPlainFlagKeepsTheTimestampedLines(t *testing.T) {
	h := styledHarness(t)
	if err := h.run("--plain", "deploy", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	// forceStyled is a test seam that outranks --plain; the flag itself is checked through the real decision:
	h.app.forceStyled = false
	h.out.Reset()
	if err := h.run("--plain", "validate"); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); !strings.Contains(out, "project demo, artifact release-dir, runtime pm2") || strings.Contains(out, "◆") {
		t.Errorf("--plain must keep the plain text:\n%s", out)
	}
}
