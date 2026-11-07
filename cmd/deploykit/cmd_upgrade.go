package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/deploy"
	"github.com/masilia/deploykit/internal/inventory"
)

// upgradeServerBinary puts this machine's deploykit (or the build --server-binary names) on the server of a target, and nothing else.
func (a *app) upgradeServerBinary(ctx context.Context, p *project, t *inventory.Target, binary string) error {
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return err
	}
	c := &deploy.Compose{Manifest: p.m, Target: t, Site: site, TargetFile: t.Path}
	if binary != "" {
		c.Binary, c.BinaryExplicit = binary, true
	} else if c.Binary, err = os.Executable(); err != nil {
		return err
	}
	return a.flow("upgrade", p.m.Project+" → "+t.ID, func() error { return c.UpgradeBinary(ctx, a.runner(a.dryRun)) })
}

// binaryOnlyConflicts names the options of a deploy that make no sense when only the binary goes.
func binaryOnlyConflicts(f composeFlags) string {
	var with []string
	if f.version != "" {
		with = append(with, "--version")
	}
	if f.rollback != "" {
		with = append(with, "--rollback")
	}
	if f.install {
		with = append(with, "--install")
	}
	if f.envFile != "" {
		with = append(with, "--env")
	}
	if f.gates {
		with = append(with, "--gates")
	}
	return strings.Join(with, ", ")
}

// fleetUpgradeCmd is `deploykit fleet upgrade`: the binary of several servers, in rollout order.
func (a *app) fleetUpgradeCmd(ff *fleetFlags) *cobra.Command {
	var binary string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Put this deploykit on the servers of the targets: the binary only, no deploy",
		Long: `Uploads the deploykit binary of this machine (or the build --server-binary names) to the server of each target, checks that it arrived intact
and prints the version the server now runs. Nothing else changes: no files, no .env, no update, no restart. Cron and 'deploykit server ...' use that
binary, so they run the new one from their next call on. A target whose deployment root does not exist yet (deploy --install was never run) is refused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext(cmd)
			defer stop()
			p, err := a.load()
			if err != nil {
				return err
			}
			ts, err := a.fleetSelection(p, ff.ids, ff.all, ff.supervised)
			if err != nil || ts == nil {
				return err
			}
			inventory.SortByRollout(ts)
			var failed []string
			for _, t := range ts {
				_, _ = fmt.Fprintf(a.out, "== upgrading the binary of %s\n", t.ID)
				if err := a.upgradeServerBinary(ctx, p, t, binary); err != nil {
					failed = append(failed, t.ID)
					if !ff.continueErr {
						return fmt.Errorf("upgrade failed on %s: %w", t.ID, err)
					}
					_, _ = fmt.Fprintf(a.out, "%s: FAILED, continuing (--continue-on-error): %v\n", t.ID, err)
					continue
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("upgrade failed on %s", strings.Join(failed, ", "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&binary, "server-binary", "", "deploykit binary to upload (needed when this machine is not the server's platform)")
	cmd.Flags().BoolVar(&ff.continueErr, "continue-on-error", false, "do not stop at the first failing target")
	return cmd
}
