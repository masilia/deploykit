package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/version"
)

// newRootCmd builds the command tree writing to stdout.
func newRootCmd() *cobra.Command { return newRootCmdTo(os.Stdout, newApp(os.Stdout)) }

func newRootCmdTo(out io.Writer, a *app) *cobra.Command {
	a.out = out
	root := &cobra.Command{
		Use:           "deploykit",
		Short:         "Generic deployment CLI for Docker Compose, Node and PHP applications",
		Long:          "deploykit builds, ships, runs and supervises applications on servers managed over SSH,\nconfigured by a per-project deploykit.yaml manifest.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(out)
	pf := root.PersistentFlags()
	pf.StringVarP(&a.manifestPath, "manifest", "f", a.manifestPath, "path to deploykit.yaml (default: ./deploykit.yaml)")
	pf.StringVar(&a.repoPath, "repo", a.repoPath, "git repository to build and upload from (default: the directory of the manifest)")
	pf.BoolVarP(&a.verbose, "verbose", "v", a.verbose, "show the output of every step, not only of failed ones")
	pf.BoolVar(&a.dryRun, "dry-run", a.dryRun, "show what would run; read-only checks still execute, nothing is changed")

	root.AddCommand(
		newVersionCmd(),
		a.validateCmd(),
		a.deployCmd(),
		a.rollbackCmd(),
		a.bootstrapCmd(),
		a.statusCmd(),
		a.doctorCmd(),
		a.serverCmd(),
		a.releaseCmd(),
		a.fleetCmd(),
		a.healthcheckCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the deploykit version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "deploykit", version.String())
			return err
		},
	}
}

func (a *app) validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check deploykit.yaml and the targets of the inventory",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.out, "%s: project %s, artifact %s, runtime %s\n", p.m.Path(), p.m.Project, p.m.Artifact.Type, p.m.Runtime.Type)
			ids, err := inventory.List(p.inventoryDir())
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, err := inventory.Open(p.inventoryDir(), id, p.global); err != nil {
					return err
				}
			}
			_, _ = fmt.Fprintf(a.out, "%d target(s) in %s: %v\n", len(ids), p.inventoryDir(), ids)
			return nil
		},
	}
}
