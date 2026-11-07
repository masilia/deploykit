package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/masilia/deploykit/internal/inventory"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

// serverIsRemote reports whether a `server` command was typed on a workstation rather than on the server: the project is here, with its
// inventory, but there is no deployment root (no .deploykit/target.env next to the manifest, which `deploy` uploads to the server only).
// Then the command is for a target, and runs there. With --target-file, or with no inventory to choose from, it is the server's own path,
// which reports its own errors.
func (a *app) serverIsRemote(targetFile string) (*project, bool) {
	if targetFile != "" {
		return nil, false
	}
	p, err := a.load()
	if err != nil {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(p.dir, ".deploykit", "target.env")); err == nil {
		return nil, false
	}
	if ids, err := inventory.List(p.inventoryDir()); err != nil || len(ids) == 0 {
		return nil, false
	}
	return p, true
}

// localOnlyFlags are the flags that describe the workstation, not the command: they are not sent to the server.
var localOnlyFlags = map[string]bool{"target": true, "target-file": true, "manifest": true, "repo": true, "plain": true}

// forwardedFlags are the flags the user set for the command (--dry-run, --base-url...), in the form the server's own command reads.
func forwardedFlags(cmd *cobra.Command) []string {
	var out []string
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if localOnlyFlags[f.Name] {
			return
		}
		if f.Value.Type() == "bool" {
			out = append(out, "--"+f.Name+"="+f.Value.String())
			return
		}
		out = append(out, "--"+f.Name, f.Value.String())
	})
	return out
}

// serverRemote runs `deploykit server <sub>` on a target, over SSH, with the deploykit that `deploy` put in its deployment root. The
// output comes back as it is produced. `update` and `install` are not run this way: they belong to `deploy`, which first uploads the
// binary and the files they run with.
func (a *app) serverRemote(cmd *cobra.Command, p *project, targetID string) error {
	sub := cmd.Name()
	if sub == "update" || sub == "install" {
		return fmt.Errorf("'deploykit server %s' runs on the server and is the last step of 'deploykit deploy', which uploads the binary and the files first: use 'deploykit deploy --version X.Y.Z%s'",
			sub, map[bool]string{true: " --install", false: ""}[sub == "install"])
	}
	if p.m.Runtime.Type != manifest.RuntimeCompose {
		return fmt.Errorf("`server` commands manage compose stacks; this manifest uses runtime %s", p.m.Runtime.Type)
	}
	ctx, stop := signalContext(cmd)
	defer stop()
	t, err := p.target(targetID)
	if err != nil {
		return err
	}
	root := strings.TrimRight(t.Get("path"), "/")
	if root == "" || !strings.HasPrefix(root, "/") {
		return fmt.Errorf("target %s: path must be an absolute directory", t.ID)
	}
	site, err := a.ssh(t, p.m, roleSite)
	if err != nil {
		return err
	}
	bin := root + "/.deploykit/deploykit"
	if _, err := site.Run(ctx, transport.Spec{Argv: []string{"test", "-x", bin}}); err != nil {
		var exit *transport.ExitError
		if errors.As(err, &exit) && exit.Code != 255 { // 255 is ssh itself: the server could not be reached
			return fmt.Errorf("target %s has no deploykit at %s yet: run 'deploykit deploy -t %s' first", t.ID, bin, t.ID)
		}
		return err // the server could not be reached
	}
	argv := append([]string{bin, "server", sub}, forwardedFlags(cmd)...)
	argv = append(argv, "--manifest", root+"/deploykit.yaml")
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintln(out, a.styles().Muted.Render(fmt.Sprintf("on %s (%s): deploykit server %s", t.ID, site.Describe(), strings.Join(argv[2:len(argv)-2], " "))))
	if _, err := site.Run(ctx, transport.Spec{Argv: argv, Dir: root, Out: out}); err != nil {
		return fmt.Errorf("deploykit server %s failed on %s: %w", sub, t.ID, err)
	}
	return nil
}
