package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/masilia/deploykit/internal/transport"
)

// addTarget gives the project a second pm2 target.
func addTarget(t *testing.T, dir, id string) {
	t.Helper()
	body := "HOST=srv2.example\nSITE_USER=othersite\nROOT_USER=root\nDOMAIN=" + id + ".example\nPATH_ON_SERVER=/home/othersite/htdocs/" + id + ".example\nAPP_PORT=3002\n"
	if err := os.WriteFile(filepath.Join(dir, "targets", id+".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func releasesOnServer(h *harness) {
	h.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "for d in *") }, transport.Result{Stdout: "" +
		"20261026000000\tlive\t{\"stamp\":\"20261026000000\",\"commit\":\"abc1234\",\"ref\":\"HEAD\",\"date\":\"2026-10-26T00:00:00Z\"}\n" +
		"20261025120000\tfailed\t\n" +
		"20261025000000\tsuperseded\t\n"})
}

func TestFleetStatusOfPM2Targets(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	addTarget(t, dir, "other")
	h := newHarness(dir)
	releasesOnServer(h)
	if err := h.run("fleet", "status", "--all"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	out := h.out.String()
	for _, want := range []string{"TARGET", "LIVE", "COMMIT", "PREV", "demo", "other", "20261026000000", "abc1234", "OK"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "20261025000000") || strings.Contains(out, "20261025120000") {
		t.Errorf("PREV must be the newest release that did not fail:\n%s", out)
	}
}

func TestFleetStatusSaysWhenNothingWasDeployed(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	h.site = transport.NewFake("demosite@srv") // the harness's default rules are first-match: start from none
	if err := h.run("fleet", "status", "--all"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !strings.Contains(h.out.String(), "to deploy") {
		t.Errorf("a target without a live release should say so:\n%s", h.out)
	}
}

func TestFleetHealthcheckOfPM2Targets(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	if err := h.run("fleet", "healthcheck", "--all"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	if !strings.Contains(h.out.String(), "demo: OK") || !strings.Contains(h.site.Trace(), "DK_PORT=3001") {
		t.Errorf("output:\n%s\ntrace:\n%s", h.out, h.site.Trace())
	}

	h = newHarness(dir)
	h.site = transport.NewFake("demosite@srv")
	h.site.OnFunc(func(s transport.Spec) bool { return strings.Contains(s.Script, "DK_TRIES") }, transport.Result{Stdout: "503\n", ExitCode: 1})
	err := h.run("fleet", "healthcheck", "--all")
	if err == nil || !strings.Contains(h.out.String(), "demo: FAILED: / answers 503, expected 200") {
		t.Errorf("an unhealthy site must fail the command (err %v):\n%s", err, h.out)
	}
}

func TestFleetBackupsAreForComposeTargets(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	h := newHarness(dir)
	err := h.run("fleet", "backups", "--all")
	if err == nil || !strings.Contains(err.Error(), "backups apply to compose targets") {
		t.Errorf("err = %v", err)
	}
}

func TestFleetDeployOfPM2Targets(t *testing.T) {
	dir := newProjectDir(t, cliManifest)
	addTarget(t, dir, "other")
	h := newHarness(dir)
	if err := h.run("fleet", "deploy", "--all", "--dry-run"); err != nil {
		t.Fatalf("%v\n%s", err, h.out)
	}
	out := h.out.String()
	if strings.Count(out, "== deploying") != 2 || !strings.Contains(out, "demo: OK") || !strings.Contains(out, "other: OK") {
		t.Errorf("both targets should be deployed in order:\n%s", out)
	}
	if err := h.run("fleet", "deploy", "--all", "--version", "1.0.0"); err == nil || !strings.Contains(err.Error(), "no version") {
		t.Errorf("a version makes no sense for pm2: %v", err)
	}
	if err := h.run("fleet", "deploy"); err == nil || !strings.Contains(err.Error(), "explicit target") {
		t.Errorf("a fleet deploy is never implicit: %v", err)
	}
}
