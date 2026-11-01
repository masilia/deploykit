package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recorder installs a stand-in for ssh (or rsync) that stores its arguments and stdin, and returns its path and the
// directory holding the recordings.
func recorder(t *testing.T) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "fake-bin")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$REC/args\"\ncat > \"$REC/stdin\"\necho remote-output\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	t.Setenv("REC", rec)
	return bin, rec
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSSHScriptTravelsOnStdinWithSecrets(t *testing.T) {
	bin, rec := recorder(t)
	s := &SSH{Host: "srv.example.com", User: "deploy", Port: 2222, Bin: bin, Prelude: "PRELUDE_LINE"}

	res, err := s.Run(t.Context(), Spec{
		Script:  "echo \"it's fine\"\n",
		Env:     map[string]string{"APP": "x"},
		Secrets: map[string]string{"TOKEN": "s3cr3t-token"},
		Dir:     "/opt/my app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "remote-output\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}

	args := read(t, rec, "args")
	for _, want := range []string{"-p\n2222\n", "BatchMode=yes", "ConnectTimeout=10", "StrictHostKeyChecking=accept-new", "deploy@srv.example.com\nbash\n-l\n-s\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("arguments %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "s3cr3t-token") || strings.Contains(args, "echo") {
		t.Errorf("secret or script leaked into the arguments: %q", args)
	}

	stdin := read(t, rec, "stdin")
	want := "PRELUDE_LINE\nexport APP=x\nexport TOKEN=s3cr3t-token\ncd '/opt/my app' || exit 1\n" + guardStdin("echo \"it's fine\"\n")
	if stdin != want {
		t.Errorf("stdin = %q, want %q", stdin, want)
	}
}

func TestSSHArgvWithoutStdinBecomesAScript(t *testing.T) {
	bin, rec := recorder(t)
	s := &SSH{Host: "h", Bin: bin}
	if _, err := s.Run(t.Context(), Spec{Argv: []string{"docker", "compose", "up", "-d", "my svc"}, Secrets: map[string]string{"P": "pw"}}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, rec, "stdin"); got != "export P=pw\nexec docker compose up -d 'my svc'\n" {
		t.Errorf("stdin = %q", got)
	}
	if args := read(t, rec, "args"); strings.Contains(args, "pw") || strings.Contains(args, "docker") {
		t.Errorf("arguments leaked: %q", args)
	}
}

func TestSSHArgvWithStdinKeepsTheDataOnStdin(t *testing.T) {
	bin, rec := recorder(t)
	s := &SSH{Host: "h", User: "u", Bin: bin}
	_, err := s.Run(t.Context(), Spec{
		Argv:  []string{"docker", "login", "--password-stdin"},
		Stdin: strings.NewReader("the-token"),
		Env:   map[string]string{"A": "1"},
		Dir:   "/srv",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, rec, "stdin"); got != "the-token" {
		t.Errorf("stdin = %q, want only the data", got)
	}
	args := read(t, rec, "args")
	if !strings.Contains(args, "bash\n-l\n-c\n") || !strings.Contains(args, "docker login --password-stdin") || strings.Contains(args, "the-token") {
		t.Errorf("arguments = %q", args)
	}
}

func TestSSHRefusesSecretsWithStdin(t *testing.T) {
	s := &SSH{Host: "h", Bin: "unused"}
	_, err := s.Run(t.Context(), Spec{Argv: []string{"x"}, Stdin: strings.NewReader("d"), Secrets: map[string]string{"P": "pw"}})
	if err == nil {
		t.Error("want an error: secrets and stdin cannot both travel")
	}
}

func TestSSHRejectsBadVariableNames(t *testing.T) {
	s := &SSH{Host: "h", Bin: "unused"}
	if _, err := s.Run(t.Context(), Spec{Script: "true", Env: map[string]string{"A;rm -rf /": "x"}}); err == nil {
		t.Error("want an error for an unsafe variable name")
	}
}

func TestSSHDescribeAndTarget(t *testing.T) {
	if got := (&SSH{Host: "h"}).Describe(); got != "h:22" {
		t.Errorf("Describe = %q", got)
	}
	if got := (&SSH{Host: "h", User: "u", Port: 2200}).Describe(); got != "u@h:2200" {
		t.Errorf("Describe = %q", got)
	}
}

func TestSSHSync(t *testing.T) {
	bin, rec := recorder(t)
	s := &SSH{Host: "srv", User: "deploy", Port: 2200, Rsync: bin, ExtraOpts: []string{"-i", "/keys/id"}}
	err := s.Sync(t.Context(), SyncSpec{Src: "build/", Dst: "/srv/releases/1/", Delete: true, LinkDest: "/srv/releases/0", Stats: true, Checksum: true, Excludes: []string{".git"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(read(t, rec, "args")), "\n")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-az", "-c", "--delete", "--info=stats1", "--link-dest=/srv/releases/0", "--exclude=.git", "-p 2200", "-i /keys/id", "build/", "deploy@srv:/srv/releases/1/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rsync arguments %q lack %q", joined, want)
		}
	}
	if args[len(args)-2] != "build/" || args[len(args)-1] != "deploy@srv:/srv/releases/1/" {
		t.Errorf("paths must come last: %v", args)
	}
}

func TestRsyncArgsOmitsOptionalFlags(t *testing.T) {
	got := strings.Join(rsyncArgs(SyncSpec{Src: "a/", Dst: "b/"}, ""), " ")
	if got != "-az a/ b/" {
		t.Errorf("args = %q", got)
	}
}
