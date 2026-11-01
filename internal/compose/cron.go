package compose

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/masilia/deploykit/internal/transport"
)

// CronEntry is one scheduled command of a deployment root.
type CronEntry struct {
	Name     string // also the log file name
	Schedule string // five cron fields
	Command  string // run after `cd <root> &&`
}

var schedulePattern = regexp.MustCompile(`^(\S+\s+){4}\S+$`)

// ValidSchedule reports whether s has the five fields of a crontab schedule.
func ValidSchedule(s string) bool { return schedulePattern.MatchString(strings.TrimSpace(s)) }

func blockMarkers(root string) (begin, end string) {
	return "# BEGIN deploykit " + root, "# END deploykit " + root
}

// MergeCrontab returns the crontab with the entries of root replaced: the previous deploykit block of this root is
// removed, and so are the legacy lines written by the legacy shell scripts for the same root (`cd <root> && …`). Lines of
// other roots and users' own lines are untouched, so several deployments can share one server.
func MergeCrontab(existing, root string, entries []CronEntry) string {
	begin, end := blockMarkers(root)
	legacy := "cd " + root + " &&"
	var kept []string
	inBlock := false
	for _, line := range strings.Split(strings.TrimRight(existing, "\n"), "\n") {
		switch {
		case line == begin:
			inBlock = true
		case line == end:
			inBlock = false
		case inBlock:
		case strings.Contains(line, legacy):
		case line == "" && len(kept) == 0:
		default:
			kept = append(kept, line)
		}
	}
	var b strings.Builder
	for _, l := range kept {
		b.WriteString(l + "\n")
	}
	if len(entries) > 0 {
		b.WriteString(begin + "\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "%s cd %s && %s >> %s/log/%s.log 2>&1\n", strings.TrimSpace(e.Schedule), root, e.Command, root, e.Name)
		}
		b.WriteString(end + "\n")
	}
	return b.String()
}

// InstallCrons rewrites the crontab of the user running the transport.
func (s *Stack) InstallCrons(ctx context.Context, entries []CronEntry) error {
	for _, e := range entries {
		if !ValidSchedule(e.Schedule) {
			return fmt.Errorf("cron %s: invalid schedule %q", e.Name, e.Schedule)
		}
		if strings.ContainsAny(e.Name, " /\n") || e.Name == "" {
			return fmt.Errorf("cron: invalid name %q", e.Name)
		}
	}
	if _, err := s.T.Run(ctx, transport.Spec{Argv: []string{"mkdir", "-p", s.Root + "/log"}}); err != nil {
		return err
	}
	res, err := s.T.Run(ctx, transport.Spec{Script: "crontab -l 2>/dev/null || true\n"})
	if err != nil {
		return err
	}
	merged := MergeCrontab(res.Stdout, s.Root, entries)
	_, err = s.T.Run(ctx, transport.Spec{Argv: []string{"crontab", "-"}, Stdin: strings.NewReader(merged)})
	return err
}
