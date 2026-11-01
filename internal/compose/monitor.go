package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// Level is the severity of a supervision diagnosis.
type Level string

// Diagnosis levels.
const (
	LevelOK   Level = "ok"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// MonitorState is what survives between two runs of the cron, stored as `level|signature|count|lastAlert`.
type MonitorState struct {
	Level     Level
	Signature string
	Count     int   // consecutive runs in a non-ok level
	LastAlert int64 // epoch seconds of the last message sent, 0 if none
}

// ParseMonitorState reads the state file; anything unreadable is a clean ok state.
func ParseMonitorState(s string) MonitorState {
	parts := strings.Split(strings.TrimSpace(s), "|")
	if len(parts) != 4 {
		return MonitorState{Level: LevelOK}
	}
	count, _ := strconv.Atoi(parts[2])
	alert, _ := strconv.ParseInt(parts[3], 10, 64)
	lvl := Level(parts[0])
	if lvl != LevelOK && lvl != LevelWarn && lvl != LevelFail {
		return MonitorState{Level: LevelOK}
	}
	return MonitorState{Level: lvl, Signature: parts[1], Count: count, LastAlert: alert}
}

func (s MonitorState) String() string {
	return fmt.Sprintf("%s|%s|%d|%d", s.Level, s.Signature, s.Count, s.LastAlert)
}

// Signature identifies a problem, so a different problem is announced at once.
func Signature(level Level, reasons string) string {
	return strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(string(level)+"|"+reasons))), 10)
}

// Decision is what the monitor does after a diagnosis.
type Decision struct {
	Next MonitorState
	Send string // "", "alert", "reminder" or "recovery"
}

// Decide ports the state machine of monitor.sh: a problem is announced after `threshold` consecutive runs (to avoid
// false alarms), announced again when it changes, reminded every `remind` while it lasts, and a recovery message is sent
// when everything is back to normal after an alert.
func Decide(prev MonitorState, level Level, reasons string, now time.Time, threshold int, remind time.Duration) Decision {
	if level == LevelOK {
		d := Decision{Next: MonitorState{Level: LevelOK}}
		if prev.Level != LevelOK && prev.LastAlert > 0 {
			d.Send = "recovery"
		}
		return d
	}
	count := prev.Count + 1
	if prev.Level == LevelOK {
		count = 1
	}
	sig := Signature(level, reasons)
	next := MonitorState{Level: level, Signature: sig, Count: count, LastAlert: prev.LastAlert}
	d := Decision{Next: next}
	if count >= threshold {
		switch {
		case prev.LastAlert == 0 || sig != prev.Signature:
			d.Send = "alert"
		case now.Unix()-prev.LastAlert >= int64(remind.Seconds()):
			d.Send = "reminder"
		}
	}
	if d.Send != "" {
		d.Next.LastAlert = now.Unix()
	}
	return d
}

// Diagnosis is the result of one supervision run.
type Diagnosis struct {
	Level   Level
	Reasons []string
}

func (d *Diagnosis) add(level Level, reason string) {
	d.Reasons = append(d.Reasons, reason)
	if rank(level) > rank(d.Level) {
		d.Level = level
	}
}

func rank(l Level) int {
	switch l {
	case LevelWarn:
		return 1
	case LevelFail:
		return 2
	}
	return 0
}

// Text joins the reasons the way the alert shows them.
func (d Diagnosis) Text() string { return strings.Join(d.Reasons, " ; ") }

// Monitor checks the stack from outside the application and alerts through Notify.
type Monitor struct {
	Stack   *Stack
	Spec    *manifest.MonitorSpec
	Env     *envfile.Doc
	BaseURL string // where the checks reach the application (the loopback proxy behind CloudPanel)
	Host    string // the proxy's Host header, the public domain
	Version string
	Server  string // host name shown in messages
	Notify  func(ctx context.Context, text string)
	Now     func() time.Time
	DryRun  bool
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Run diagnoses the stack, then updates the state and notifies. It returns the diagnosis.
func (m *Monitor) Run(ctx context.Context) (Diagnosis, error) {
	diag := Diagnosis{Level: LevelOK}
	if m.Spec.SkipWhileLocked {
		if _, err := m.Stack.T.Run(ctx, transport.Spec{Argv: []string{"test", "-d", m.Stack.Cfg.Lock}, Dir: m.Stack.Root}); err == nil {
			return diag, nil // an update, backup or restore is restarting services: no alert meanwhile
		}
	}
	vars := tmpl.Vars{"env": envMap(m.Env), "base_url": m.BaseURL}
	for _, c := range m.Spec.Checks {
		if err := m.check(ctx, c, vars, &diag); err != nil {
			return diag, err
		}
	}
	if m.DryRun {
		return diag, nil
	}
	return diag, m.record(ctx, diag)
}

func (m *Monitor) check(ctx context.Context, c manifest.MonitorCheck, vars tmpl.Vars, diag *Diagnosis) error {
	switch c.Kind {
	case "services":
		states, err := m.Stack.States(ctx)
		if err != nil {
			return err
		}
		for _, st := range states {
			if !st.Ready() {
				diag.add(LevelFail, st.Name+" "+st.String())
			}
		}
	case "http":
		url, err := tmpl.Render(c.HTTP, vars, false)
		if err != nil {
			return err
		}
		args := []string{"curl", "-sS", "-m", "15"}
		if m.Host != "" {
			args = append(args, "-H", "Host: "+m.Host)
		}
		for k, v := range c.Header {
			rendered, rerr := tmpl.Render(v, vars, false)
			if rerr != nil || rendered == "" {
				return nil // a header whose value is not configured (no token yet): this check does not apply
			}
			args = append(args, "-H", k+": "+rendered)
		}
		res, runErr := m.Stack.T.Run(ctx, transport.Spec{Argv: append(args, url)})
		switch {
		case runErr != nil || res.Stdout == "":
			diag.add(LevelFail, "unreachable: "+url)
		case c.Contains != "" && !strings.Contains(res.Stdout, c.Contains):
			diag.add(LevelFail, "unexpected answer from "+url)
		case c.JSONStatus != "":
			level, summary := ParseStatus(res.Stdout, c.JSONStatus)
			switch level {
			case LevelOK:
			case LevelWarn, LevelFail:
				if summary == "" {
					summary = "detailed health " + string(level)
				}
				diag.add(level, summary)
			default:
				diag.add(LevelWarn, "detailed health unreadable (is the health token set in the api container? run an update)")
			}
		}
	case "disk":
		min, err := tmpl.Render(c.DiskWarnMB, vars, false)
		if err != nil {
			return err
		}
		warn, _ := strconv.Atoi(min)
		free, err := m.Stack.FreeMB(ctx, ".")
		if err != nil {
			return err
		}
		if warn > 0 && free < warn {
			diag.add(LevelWarn, fmt.Sprintf("host disk: %d MB free", free))
		}
	}
	return nil
}

// ParseStatus reads a status field (a dotted path such as data.status) from a JSON document, with the sibling
// "summary" string. An unreadable document yields an empty level.
func ParseStatus(body, path string) (Level, string) {
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) != nil {
		return "", ""
	}
	var cur any = doc
	parent := doc
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return "", ""
		}
		parent = obj
		cur = obj[part]
	}
	status, _ := cur.(string)
	summary, _ := parent["summary"].(string)
	switch Level(status) {
	case LevelOK, LevelWarn, LevelFail:
		return Level(status), summary
	}
	return "", ""
}

func (m *Monitor) record(ctx context.Context, diag Diagnosis) error {
	file := m.Stack.Root + "/" + m.Spec.StateFile
	res, _ := m.Stack.T.Run(ctx, transport.Spec{Argv: []string{"cat", file}})
	prev := ParseMonitorState(res.Stdout)
	threshold, remind := 2, 6*time.Hour
	if v, err := tmpl.Render(m.Spec.FailThreshold, EnvVars(m.Env), false); err == nil {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 {
			threshold = n
		}
	}
	if v, err := tmpl.Render(m.Spec.RemindHours, EnvVars(m.Env), false); err == nil {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 {
			remind = time.Duration(n) * time.Hour
		}
	}
	d := Decide(prev, diag.Level, diag.Text(), m.now(), threshold, remind)
	project := m.Stack.Project
	switch d.Send {
	case "recovery":
		m.send(ctx, fmt.Sprintf(":white_check_mark: %s recovered: %s (v%s): everything works again.", project, m.Host, m.Version))
	case "alert", "reminder":
		icon := ":warning:"
		if diag.Level == LevelFail {
			icon = ":rotating_light:"
		}
		m.send(ctx, fmt.Sprintf("%s %s %s: %s (v%s, %s)\n%s", icon, project, diag.Level, m.Host, m.Version, m.Server, diag.Text()))
	}
	if _, err := m.Stack.T.Run(ctx, transport.Spec{
		Argv:  []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", file},
		Stdin: strings.NewReader(d.Next.String() + "\n"),
	}); err != nil {
		return fmt.Errorf("cannot write the monitor state: %w", err)
	}
	return nil
}

func (m *Monitor) send(ctx context.Context, text string) {
	if m.Notify != nil {
		m.Notify(ctx, text)
	}
}
