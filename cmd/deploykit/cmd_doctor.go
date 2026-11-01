package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/doctor"
	"github.com/masilia/deploykit/internal/manifest"
)

func (a *app) doctorCmd() *cobra.Command {
	var target string
	var local bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the tools needed here and on the server before deploying",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := a.load()
			if err != nil {
				return err
			}
			report := doctor.Check(cmd.Context(), a.local, doctor.LocalTools(p.m))
			if !local {
				t, err := p.target(target)
				if err != nil {
					return err
				}
				site, err := a.ssh(t, p.m, roleSite)
				if err != nil {
					return err
				}
				siteReport := doctor.Check(cmd.Context(), site, doctor.ServerTools(p.m, false))
				if p.m.Runtime.Type == manifest.RuntimePM2 && doctor.AllLoginFailed(siteReport) {
					// Before the bootstrap the site user does not exist yet: one clear line instead of one failure per tool.
					siteReport = []doctor.Result{{Where: site.Describe(), Name: "site user", Level: doctor.Warn, Detail: "cannot log in yet: run `deploykit bootstrap` (it creates the user and installs your key)"}}
				}
				report = append(report, siteReport...)
				if p.m.Runtime.Type == manifest.RuntimePM2 || p.m.Ingress.Type == manifest.IngressCloudPanel || p.m.Ingress.Select != "" {
					root, err := a.ssh(t, p.m, roleRoot)
					if err != nil {
						return err
					}
					report = append(report, doctor.Check(cmd.Context(), root, doctor.ServerTools(p.m, true))...)
				}
			}
			a.printReport(report)
			if doctor.Worst(report) == doctor.Fail {
				return fmt.Errorf("some required tools are missing")
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&target, "target", "t", "", "target id")
	cmd.Flags().BoolVar(&local, "local", false, "check only this machine")
	return cmd
}
