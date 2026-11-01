package compose

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

func TestParseMonitorState(t *testing.T) {
	if s := ParseMonitorState("fail|123|3|1700000000\n"); s.Level != LevelFail || s.Signature != "123" || s.Count != 3 || s.LastAlert != 1700000000 {
		t.Errorf("state = %+v", s)
	}
	for _, junk := range []string{"", "garbage", "a|b", "ok||0|0|extra", "bogus|1|1|1"} {
		if s := ParseMonitorState(junk); s.Level != LevelOK || s.Count != 0 {
			t.Errorf("%q must read as a clean state, got %+v", junk, s)
		}
	}
	st := MonitorState{Level: LevelWarn, Signature: "9", Count: 2, LastAlert: 5}
	if ParseMonitorState(st.String()) != st {
		t.Error("state does not round trip")
	}
}

// The state machine of monitor.sh, step by step over a day of runs (threshold 2, reminder every 6 hours).
func TestDecideSequence(t *testing.T) {
	t0 := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }
	const threshold, remind = 2, 6 * time.Hour

	state := MonitorState{Level: LevelOK}
	step := func(name string, now time.Time, lvl Level, reasons, wantSend string, wantCount int) {
		t.Helper()
		d := Decide(state, lvl, reasons, now, threshold, remind)
		if d.Send != wantSend || d.Next.Count != wantCount {
			t.Errorf("%s: send %q count %d, want %q / %d", name, d.Send, d.Next.Count, wantSend, wantCount)
		}
		state = d.Next
	}
	step("healthy", at(0), LevelOK, "", "", 0)
	step("first failure: wait, it may be a blip", at(5), LevelFail, "api down", "", 1)
	step("second consecutive failure: alert", at(10), LevelFail, "api down", "alert", 2)
	step("still failing: silent", at(15), LevelFail, "api down", "", 3)
	step("still failing after 5h59: silent", at(10+359), LevelFail, "api down", "", 4)
	step("6 hours since the alert: reminder", at(10+360), LevelFail, "api down", "reminder", 5)
	step("a different problem is announced at once", at(10+365), LevelFail, "db down", "alert", 6)
	step("escalation warn->fail is a different problem", at(10+370), LevelWarn, "disk low", "alert", 7)
	step("recovered: one recovery message", at(10+375), LevelOK, "", "recovery", 0)
	step("healthy again: nothing", at(10+380), LevelOK, "", "", 0)
	step("a new blip after recovery restarts the count", at(10+385), LevelFail, "api down", "", 1)
}

func TestDecideRecoveryOnlyAfterAnAlert(t *testing.T) {
	// A single blip that never reached the threshold: nothing was announced, so nothing to recover from.
	blip := Decide(MonitorState{Level: LevelOK}, LevelFail, "x", time.Now(), 2, time.Hour)
	back := Decide(blip.Next, LevelOK, "", time.Now(), 2, time.Hour)
	if back.Send != "" {
		t.Errorf("recovery sent without a previous alert: %q", back.Send)
	}
}

func TestDecideThresholdOfOne(t *testing.T) {
	if d := Decide(MonitorState{Level: LevelOK}, LevelFail, "x", time.Now(), 1, time.Hour); d.Send != "alert" {
		t.Errorf("threshold 1 alerts at once, got %q", d.Send)
	}
}

func TestParseStatus(t *testing.T) {
	body := `{"data":{"status":"warn","summary":"Redis slow","checks":[1,2]},"meta":{}}`
	if lvl, sum := ParseStatus(body, "data.status"); lvl != LevelWarn || sum != "Redis slow" {
		t.Errorf("got %q %q", lvl, sum)
	}
	for _, bad := range []string{"", "not json", `{"data":{}}`, `{"data":{"status":"weird"}}`, `{"data":"x"}`, `{"status":5}`} {
		if lvl, _ := ParseStatus(bad, "data.status"); lvl != "" {
			t.Errorf("%q must be unreadable, got %q", bad, lvl)
		}
	}
	if lvl, _ := ParseStatus(`{"status":"ok"}`, "status"); lvl != LevelOK {
		t.Errorf("top-level path: %q", lvl)
	}
}

type monitorFixture struct {
	m        *Monitor
	fake     *transport.Fake
	messages []string
}

func newMonitor(t *testing.T, pre func(*transport.Fake)) *monitorFixture {
	t.Helper()
	s, f := newStack(t, "certbot")
	if pre != nil {
		pre(f)
	}
	f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] != "" }, transport.Result{Stdout: "true healthy\n"})
	f.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{Stdout: `{"data":{"status":"ok"}}`})
	f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "9000\n"})
	mf, err := manifest.Load("../../examples/compose-app/deploykit.yaml")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := mf.MonitorConfig()
	spec.SkipWhileLocked = false // the Fake answers `test -d` with success, which would read as "locked"
	fx := &monitorFixture{fake: f}
	fx.m = &Monitor{
		Stack: s, Spec: spec, BaseURL: "https://app.example", Host: "app.example", Version: "1.1.0", Server: "srv1",
		Env:    envfile.Parse([]byte("HEALTH_TOKEN=tok-123\nMIN_FREE_MB=2000\n")),
		Notify: func(_ context.Context, text string) { fx.messages = append(fx.messages, text) },
		Now:    func() time.Time { return time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) },
	}
	return fx
}

func (fx *monitorFixture) stateWritten() string {
	for _, c := range fx.fake.Calls() {
		if c.Spec != nil && strings.Contains(strings.Join(c.Spec.Argv, " "), `cat > "$1"`) {
			return strings.TrimSpace(c.Stdin)
		}
	}
	return ""
}

func TestMonitorHealthy(t *testing.T) {
	fx := newMonitor(t, nil)
	d, err := fx.m.Run(t.Context())
	if err != nil || d.Level != LevelOK {
		t.Fatalf("diagnosis %+v, %v", d, err)
	}
	if fx.stateWritten() != "ok||0|0" || len(fx.messages) != 0 {
		t.Errorf("state %q, messages %v", fx.stateWritten(), fx.messages)
	}
	// The detailed-health request carries the token header and the public Host header.
	var detail string
	for _, c := range fx.fake.Calls() {
		if c.Spec != nil && len(c.Spec.Argv) > 0 && c.Spec.Argv[0] == "curl" && strings.Contains(strings.Join(c.Spec.Argv, " "), "health/details") {
			detail = strings.Join(c.Spec.Argv, " ")
		}
	}
	if !strings.Contains(detail, "-H X-Health-Token: tok-123") || !strings.Contains(detail, "-H Host: app.example") {
		t.Errorf("detail request = %q", detail)
	}
}

func TestMonitorAlertsAfterTwoConsecutiveFailures(t *testing.T) {
	down := func(f *transport.Fake) {
		f.OnFunc(func(sp transport.Spec) bool { return sp.Env["DK_SVC"] == "worker" }, transport.Result{Stdout: "true unhealthy\n"})
	}
	fx := newMonitor(t, down)
	// First run: previous state says one failure already.
	fx.fake.OnFunc(func(sp transport.Spec) bool { return strings.Join(sp.Argv, " ") == "cat /opt/app/log/monitor.state" }, transport.Result{})
	d, err := fx.m.Run(t.Context())
	if err != nil || d.Level != LevelFail || !strings.Contains(d.Text(), "worker not healthy (unhealthy)") {
		t.Fatalf("diagnosis %+v, %v", d, err)
	}
	if len(fx.messages) != 0 {
		t.Errorf("the first failure must wait: %v", fx.messages)
	}
	// Second run with the stored state.
	fx2 := newMonitor(t, func(f *transport.Fake) {
		down(f)
		f.On("cat /opt/app/log/monitor.state", transport.Result{Stdout: ParseMonitorState("fail|"+Signature(LevelFail, "worker not healthy (unhealthy)")+"|1|0").String() + "\n"})
	})
	if _, err := fx2.m.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fx2.messages) != 1 || !strings.Contains(fx2.messages[0], ":rotating_light:") || !strings.Contains(fx2.messages[0], "worker not healthy") || !strings.Contains(fx2.messages[0], "v1.1.0") {
		t.Errorf("messages = %v", fx2.messages)
	}
	if !strings.HasPrefix(fx2.stateWritten(), "fail|") || !strings.HasSuffix(fx2.stateWritten(), "|2|"+"1793095200") {
		t.Errorf("state = %q", fx2.stateWritten())
	}
}

func TestMonitorIsSilentWhileAnOperationHoldsTheLock(t *testing.T) {
	fx := newMonitor(t, nil) // the Fake answers `test -d .lock-ops` with success: the lock exists
	fx.m.Spec.SkipWhileLocked = true
	d, err := fx.m.Run(t.Context())
	if err != nil || d.Level != LevelOK {
		t.Fatalf("diagnosis %+v, %v", d, err)
	}
	trace := fx.fake.Trace()
	if strings.Contains(trace, "curl") || fx.stateWritten() != "" || len(fx.messages) != 0 {
		t.Errorf("a locked stack must not be diagnosed, written or announced:\n%s", trace)
	}

	free := newMonitor(t, func(f *transport.Fake) { f.Fail("test -d .lock-ops", 1, "") })
	free.m.Spec.SkipWhileLocked = true
	if _, err := free.m.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(free.fake.Trace(), "curl") {
		t.Error("without the lock the checks run")
	}
}

func TestMonitorRecoveryAndWarnings(t *testing.T) {
	fx := newMonitor(t, func(f *transport.Fake) {
		f.On("cat /opt/app/log/monitor.state", transport.Result{Stdout: "fail|1|3|1793000000\n"})
	})
	if _, err := fx.m.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(fx.messages) != 1 || !strings.Contains(fx.messages[0], "recovered") {
		t.Errorf("messages = %v", fx.messages)
	}

	warn := newMonitor(t, func(f *transport.Fake) {
		f.OnFunc(func(sp transport.Spec) bool {
			return len(sp.Argv) > 0 && sp.Argv[0] == "curl" && strings.Contains(strings.Join(sp.Argv, " "), "health/details")
		}, transport.Result{Stdout: `{"data":{"status":"warn","summary":"queue backlog: 120 messages"}}`})
		f.OnFunc(func(sp transport.Spec) bool { return strings.Contains(sp.Script, "df -Pm") }, transport.Result{Stdout: "900\n"})
	})
	d, _ := warn.m.Run(t.Context())
	if d.Level != LevelWarn || !strings.Contains(d.Text(), "queue backlog: 120 messages") || !strings.Contains(d.Text(), "host disk: 900 MB free") {
		t.Errorf("diagnosis = %+v", d)
	}
}

func TestMonitorDryRunWritesNothing(t *testing.T) {
	fx := newMonitor(t, nil)
	fx.m.DryRun = true
	if _, err := fx.m.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if fx.stateWritten() != "" || len(fx.messages) != 0 {
		t.Errorf("dry run wrote %q / sent %v", fx.stateWritten(), fx.messages)
	}
}

func TestMonitorAPIUnreachable(t *testing.T) {
	fx := newMonitor(t, func(f *transport.Fake) {
		f.OnFunc(func(sp transport.Spec) bool { return len(sp.Argv) > 0 && sp.Argv[0] == "curl" }, transport.Result{ExitCode: 7, Stderr: "connection refused"})
	})
	d, _ := fx.m.Run(t.Context())
	if d.Level != LevelFail || !strings.Contains(d.Text(), "unreachable: https://app.example/api/v1/health") {
		t.Errorf("diagnosis = %+v", d)
	}
}
