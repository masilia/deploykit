package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// runCompose runs a command of runtime.app.commands on a compose target, or lists them when none is named.
func (a *app) runCompose(ctx context.Context, cmd *cobra.Command, target string, args []string, running bool) error {
	ec, err := a.envContext(target)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	names := make([]string, 0, len(ec.cfg.App.Commands))
	for n := range ec.cfg.App.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(args) == 0 {
		return listAppCommands(out, ec, names)
	}
	name := args[0]
	if _, ok := ec.cfg.App.Commands[name]; !ok {
		return fmt.Errorf("%q is not a command of this manifest (runtime.app.commands: %s)", name, strings.Join(names, ", "))
	}
	where := "a one-off container of " + ec.cfg.App.Service
	if running {
		where = "the running container of " + ec.cfg.App.Service
	}
	title := "run " + name
	subtitle := ec.t.ID + " · " + where
	if a.dryRun {
		_, _ = fmt.Fprintf(out, "would run %s in %s\n", name, where)
		return nil
	}
	if ec.lockHeld(ctx) {
		return fmt.Errorf("an operation holds the lock (%s): an update, an install or a backup is running; run %s when it is over", ec.cfg.Lock, name)
	}
	doc, err := ec.file.Load(ctx)
	if err != nil {
		return err
	}
	return a.flow(title, subtitle, func() error {
		if running {
			return ec.stack.RunAppRunning(ctx, name, doc, out)
		}
		return ec.stack.RunApp(ctx, name, doc, out)
	})
}

// listAppCommands prints the declared commands. A command that takes a secret from the server .env says which variable, never the value.
func listAppCommands(out io.Writer, ec *envContext, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("the manifest declares no runtime.app.commands")
	}
	width := 0
	for _, n := range names {
		width = max(width, len(n))
	}
	_, _ = fmt.Fprintf(out, "%s: commands of service %s (deploykit run NAME -t %s)\n\n", ec.t.ID, ec.cfg.App.Service, ec.t.ID)
	for _, n := range names {
		c := ec.cfg.App.Commands[n]
		line := strings.Join(strings.Fields(c.Run), " ")
		if len(line) > 88 {
			line = line[:85] + "..."
		}
		_, _ = fmt.Fprintf(out, "  %-*s  %s\n", width, n, line)
		if len(c.EnvFrom) > 0 {
			keys := make([]string, 0, len(c.EnvFrom))
			for _, k := range c.EnvFrom {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			_, _ = fmt.Fprintf(out, "  %-*s  uses from .env: %s\n", width, "", strings.Join(keys, ", "))
		}
	}
	_, _ = fmt.Fprintln(out, "\nOne-off container by default, --exec for the running one. The update hooks are not run by this command.")
	return nil
}
