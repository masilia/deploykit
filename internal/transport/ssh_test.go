package transport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// flaky installs a stand-in for ssh or rsync that fails the first `failures` calls with the given stderr, stdout and
// exit code, then succeeds. It counts its calls in $REC/calls.
func flaky(t *testing.T, failures int, stdout, stderr string, code int) (bin, rec string) {
	t.Helper()
	dir := t.TempDir()
	rec = filepath.Join(dir, "rec")
	if err := os.Mkdir(rec, 0o700); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "flaky-bin")
	script := "#!/bin/sh\ncat >/dev/null\nn=$(cat \"$REC/calls\" 2>/dev/null || echo 0); n=$((n+1)); echo $n > \"$REC/calls\"\n" +
		"if [ \"$n\" -le " + strconv.Itoa(failures) + " ]; then\n  printf '%s' '" + stdout + "'\n  printf '%s\\n' '" + stderr + "' >&2\n  exit " + strconv.Itoa(code) + "\nfi\necho ok\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test double
		t.Fatal(err)
	}
	t.Setenv("REC", rec)
	return bin, rec
}

func calls(t *testing.T, rec string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(read(t, rec, "calls")))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// retrying returns an SSH whose pauses are recorded instead of waited for.
func retrying(bin string, delays *[]time.Duration) *SSH {
	return &SSH{Host: "srv", Bin: bin, Rsync: bin, sleep: func(_ context.Context, d time.Duration) error {
		*delays = append(*delays, d)
		return nil
	}}
}

const refused = "ssh: connect to host srv port 22: Connection refused"

func TestSSHRetriesAConnectionThatCannotBeEstablished(t *testing.T) {
	bin, rec := flaky(t, 2, "", refused, 255)
	var delays []time.Duration
	var out strings.Builder
	res, err := retrying(bin, &delays).Run(t.Context(), Spec{Script: "true\n", Out: &out})
	if err != nil {
		t.Fatalf("the third attempt should succeed: %v", err)
	}
	if calls(t, rec) != 3 || !strings.Contains(res.Stdout, "ok") {
		t.Errorf("calls = %d, stdout %q", calls(t, rec), res.Stdout)
	}
	if len(delays) != 2 || delays[0] != 2*time.Second || delays[1] != 4*time.Second {
		t.Errorf("pauses = %v, want 2s then 4s", delays)
	}
	if !strings.Contains(out.String(), "retrying in 2s (attempt 2 of 3)") {
		t.Errorf("the retry should be announced: %q", out.String())
	}
}

func TestSSHGivesUpAfterTheRetries(t *testing.T) {
	bin, rec := flaky(t, 99, "", refused, 255)
	var delays []time.Duration
	_, err := retrying(bin, &delays).Run(t.Context(), Spec{Script: "true\n"})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 255 {
		t.Fatalf("want the ssh ExitError, got %v", err)
	}
	if calls(t, rec) != 3 {
		t.Errorf("attempts = %d, want 3", calls(t, rec))
	}
}

func TestSSHNeverRetriesWhatMayHaveRun(t *testing.T) {
	for name, tc := range map[string]struct{ stdout, stderr string }{
		"output already produced": {"half done\n", refused},
		"authentication failure":  {"", "deploy@srv: Permission denied (publickey)."},
		"host key changed":        {"", "Host key verification failed."},
	} {
		t.Run(name, func(t *testing.T) {
			bin, rec := flaky(t, 99, tc.stdout, tc.stderr, 255)
			var delays []time.Duration
			if _, err := retrying(bin, &delays).Run(t.Context(), Spec{Script: "true\n"}); err == nil {
				t.Fatal("want an error")
			}
			if calls(t, rec) != 1 || len(delays) != 0 {
				t.Errorf("attempts = %d, pauses = %v: it must not be retried", calls(t, rec), delays)
			}
		})
	}
}

func TestSSHSaysWhenTheSessionBrokeMidRun(t *testing.T) {
	bin, rec := flaky(t, 99, "step 1 done\n", "client_loop: send disconnect: Broken pipe", 255)
	var delays []time.Duration
	_, err := retrying(bin, &delays).Run(t.Context(), Spec{Script: "true\n"})
	if err == nil || !strings.Contains(err.Error(), "may have partly run") {
		t.Errorf("the error should warn that the effect is unknown: %v", err)
	}
	if calls(t, rec) != 1 {
		t.Errorf("attempts = %d: a script that started must not be run again", calls(t, rec))
	}
}

func TestSSHRetriesCanBeDisabledAndStdinIsNeverReplayed(t *testing.T) {
	bin, rec := flaky(t, 99, "", refused, 255)
	var delays []time.Duration
	s := retrying(bin, &delays)
	s.Retries = -1
	if _, err := s.Run(t.Context(), Spec{Script: "true\n"}); err == nil || calls(t, rec) != 1 {
		t.Errorf("Retries < 0 must make a single attempt (calls %d, err %v)", calls(t, rec), err)
	}
	bin, rec = flaky(t, 99, "", refused, 255)
	s = retrying(bin, &delays)
	if _, err := s.Run(t.Context(), Spec{Argv: []string{"docker", "login"}, Stdin: strings.NewReader("token")}); err == nil || calls(t, rec) != 1 {
		t.Errorf("a command that consumes a reader must not be retried (calls %d, err %v)", calls(t, rec), err)
	}
}

func TestRsyncRetriesAnInterruptedTransferButNotAPermissionError(t *testing.T) {
	bin, rec := flaky(t, 1, "", "rsync: connection unexpectedly closed (0 bytes received so far) [sender]", 12)
	var delays []time.Duration
	if err := retrying(bin, &delays).Sync(t.Context(), SyncSpec{Src: "a/", Dst: "/b/"}); err != nil {
		t.Fatalf("the second attempt should succeed: %v", err)
	}
	if calls(t, rec) != 2 || len(delays) != 1 {
		t.Errorf("calls %d, pauses %v", calls(t, rec), delays)
	}
	bin, rec = flaky(t, 99, "", "rsync: send_files failed to open \"/b/x\": Permission denied (13)", 23)
	if err := retrying(bin, &delays).Sync(t.Context(), SyncSpec{Src: "a/", Dst: "/b/"}); err == nil || calls(t, rec) != 1 {
		t.Errorf("a permission error must not be retried (calls %d, err %v)", calls(t, rec), err)
	}
	bin, rec = flaky(t, 99, "", "deploy@srv: Permission denied (publickey).\nrsync: connection unexpectedly closed", 255)
	if err := retrying(bin, &delays).Sync(t.Context(), SyncSpec{Src: "a/", Dst: "/b/"}); err == nil || calls(t, rec) != 1 {
		t.Errorf("an authentication failure must not be retried (calls %d, err %v)", calls(t, rec), err)
	}
}

func TestAKeepAliveTimeoutIsASessionLostNotAConnectionRefused(t *testing.T) {
	bin, rec := flaky(t, 99, "", "Connection to 203.0.113.9 port 22 timed out", 255)
	var delays []time.Duration
	_, err := retrying(bin, &delays).Run(t.Context(), Spec{Script: "true\n"})
	if err == nil || !strings.Contains(err.Error(), "may have partly run") {
		t.Errorf("the error should warn that the effect is unknown: %v", err)
	}
	if calls(t, rec) != 1 {
		t.Errorf("attempts = %d: a session that broke must not be replayed", calls(t, rec))
	}
}
