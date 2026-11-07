package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/masilia/deploykit/internal/selfupdate"
	"github.com/masilia/deploykit/internal/ui"
	"github.com/masilia/deploykit/internal/version"
)

// updateWait is how long a command that finishes quickly waits for the daily look at the releases: about the time an answer takes,
// never more. A longer command has had all the time it needed.
const updateWait = 300 * time.Millisecond

// updateTimeout caps the look itself.
const updateTimeout = 3 * time.Second

// currentRelease is the version of this binary when it is a tagged release; a build from a checkout has none.
func (a *app) currentRelease() (string, bool) {
	if a.currentVer != nil {
		return a.currentVer()
	}
	return version.Release()
}

func (a *app) releaseClient() *selfupdate.Client {
	if a.releases != nil {
		return a.releases
	}
	cur, ok := a.currentRelease()
	if !ok {
		cur = "dev"
	}
	return selfupdate.DefaultClient("deploykit/" + cur)
}

// executable is the real file that is running: a link such as dk is followed to deploykit, so that one update serves both names.
func (a *app) executable() (string, error) {
	var p string
	var err error
	if a.exePath != nil {
		p, err = a.exePath()
	} else {
		p, err = os.Executable()
	}
	if err != nil {
		return "", err
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		return "", err
	}
	if strings.Contains(p, string(filepath.Separator)+"go-build") {
		return "", errors.New("this deploykit runs from 'go run': there is no installed binary to replace")
	}
	return p, nil
}

func (a *app) platform() (string, string) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if a.goos != "" {
		goos = a.goos
	}
	if a.goarch != "" {
		goarch = a.goarch
	}
	return goos, goarch
}

type selfUpdateOpts struct {
	check, pre, force bool
	version           string
}

func (a *app) selfUpdateCmd() *cobra.Command {
	var o selfUpdateOpts
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Replace this deploykit by the latest release (or by --version X.Y.Z)",
		Long: `Downloads the archive of the latest release for this platform from the project's public GitHub releases, checks it against the release's
checksums.txt, makes sure the new binary runs, and replaces this one. The old binary stays next to it as deploykit.old.

Only a released build is replaced: a build from a checkout (make build, a version such as 0.5.1-8-g732186d) is left alone unless --force.
The servers keep the binary of their last deploy: it follows at the next deploykit deploy.

Once a day, at a terminal, deploykit also looks at the releases and says when a newer one exists. DEPLOYKIT_NO_UPDATE_CHECK=1 turns that off.`,
		Example: `  deploykit self-update --check      # is there a newer release?
  deploykit self-update              # install it
  deploykit self-update --version 0.5.1 --force   # go back to a given release`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.selfUpdate(cmd, o) },
	}
	f := cmd.Flags()
	f.BoolVar(&o.check, "check", false, "only say whether a newer release exists; change nothing")
	f.BoolVar(&o.pre, "pre", false, "also consider pre-releases (release candidates)")
	f.BoolVar(&o.force, "force", false, "replace a build made from a checkout, reinstall the same version, or install an older one")
	f.StringVar(&o.version, "version", "", "install this release (X.Y.Z) instead of the latest")
	return cmd
}

func (a *app) selfUpdate(cmd *cobra.Command, o selfUpdateOpts) error {
	ctx, stop := signalContext(cmd)
	defer stop()
	out := cmd.OutOrStdout()
	st := a.styles()
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	ok := func(format string, args ...any) {
		_, _ = fmt.Fprintf(out, "%s %s\n", st.Success.Render(ui.IconOK), fmt.Sprintf(format, args...))
	}

	curStr, isRelease := a.currentRelease()
	var cur selfupdate.Version
	if isRelease {
		var err error
		if cur, err = selfupdate.Parse(curStr); err != nil {
			return err
		}
	}

	c := a.releaseClient()
	var rel selfupdate.Release
	var err error
	if o.version != "" {
		rel, err = c.Find(ctx, o.version)
	} else {
		rel, err = c.Latest(ctx, o.pre)
	}
	if err != nil {
		return err
	}

	if !isRelease {
		say("This is a build made from a checkout (%s), not a release.", version.String())
	}
	if o.check {
		switch {
		case !isRelease:
			say("Latest release: %s", rel.Version)
		case rel.Version.Compare(cur) > 0:
			say("deploykit %s is available (this is %s): run 'deploykit self-update'", rel.Version, cur)
		default:
			ok("deploykit %s is up to date (latest release: %s)", cur, rel.Version)
		}
		return nil
	}

	switch {
	case !isRelease && !o.force:
		return fmt.Errorf("self-update replaces released builds only: use --force to replace this build by %s", rel.Version)
	case isRelease && rel.Version.Compare(cur) == 0 && !o.force:
		ok("deploykit %s is up to date", cur)
		return nil
	case isRelease && rel.Version.Compare(cur) < 0 && o.version == "":
		ok("deploykit %s is newer than the latest release (%s): nothing to do", cur, rel.Version)
		return nil
	case isRelease && rel.Version.Compare(cur) < 0 && !o.force:
		return fmt.Errorf("%s is older than the installed %s: use --force to go back", rel.Version, cur)
	}

	exe, err := a.executable()
	if err != nil {
		return err
	}
	goos, goarch := a.platform()
	name := selfupdate.ArchiveName(rel.Version, goos, goarch)
	if a.dryRun {
		say("Dry run: would download %s, check it against checksums.txt, and replace %s (the old one kept as %s.old). Nothing was changed.", name, exe, filepath.Base(exe))
		return nil
	}

	say("Installing %s over %s", rel.Version, curOrBuild(curStr, isRelease))
	backup, err := c.Install(ctx, rel, selfupdate.InstallOptions{Exe: exe, GOOS: goos, GOARCH: goarch})
	if err != nil {
		return err
	}
	ok("%s checked against checksums.txt, installed as %s", name, exe)
	say("  the previous binary is kept as %s", backup)
	for _, other := range separateCopies(exe) {
		say("  note: %s is a separate copy, not a link: it was not updated", other)
	}
	say("  servers keep the binary of their last deploy: it follows at the next 'deploykit deploy'")
	return nil
}

func curOrBuild(cur string, isRelease bool) string {
	if isRelease {
		return cur
	}
	return "this build"
}

// separateCopies are the other names of the tool in the folder of exe (deploykit, dk) that are files of their own and not links, so
// that they would keep running the old version.
func separateCopies(exe string) []string {
	var out []string
	for _, n := range []string{"deploykit", "dk"} {
		p := filepath.Join(filepath.Dir(exe), n)
		if n == filepath.Base(exe) {
			continue
		}
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

// ---- the daily notice --------------------------------------------------------------------------------------------------------

// updateNoticeApplies says whether this run may look at the releases and mention a newer one. Never for a script or a pipe (the
// styled layout is for a person at a terminal), a build from a checkout, CI, an opt-out, nor for the commands that cron and the
// shell completion run.
func (a *app) updateNoticeApplies(cmd *cobra.Command) bool {
	if !a.styled() || os.Getenv("DEPLOYKIT_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" {
		return false
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "self-update", "server", "completion", "__complete", "__completeNoDesc", "help", "version":
			return false
		}
	}
	_, ok := a.currentRelease()
	return ok
}

func (a *app) cache() (selfupdate.Cache, bool) {
	if a.updateCache != nil {
		return *a.updateCache, true
	}
	c, err := selfupdate.DefaultCache()
	return c, err == nil
}

// beginUpdateCheck starts the daily look in the background, when it is due, so that a long command has its answer by the time it ends.
func (a *app) beginUpdateCheck(cmd *cobra.Command, _ []string) {
	a.updateDone = nil
	if !a.updateNoticeApplies(cmd) {
		return
	}
	cache, ok := a.cache()
	if !ok || !cache.Load().Due(a.now()) {
		return
	}
	done := make(chan struct{})
	a.updateDone = done
	client, now := a.releaseClient(), a.now()
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
		defer cancel()
		_ = selfupdate.Refresh(ctx, client, cache, now) // an offline machine just tries again tomorrow
	}()
}

// endUpdateCheck gives the look a moment to finish, and says on stderr that a newer release exists when one does.
func (a *app) endUpdateCheck(cmd *cobra.Command, _ []string) {
	if a.updateDone != nil {
		wait := updateWait
		if a.checkWait > 0 {
			wait = a.checkWait
		}
		select {
		case <-a.updateDone:
		case <-time.After(wait):
		}
	}
	a.printUpdateNotice(cmd.ErrOrStderr(), cmd)
}

// printUpdateNotice writes the sentence once a day, from what is remembered; it asks the network for nothing.
func (a *app) printUpdateNotice(w io.Writer, cmd *cobra.Command) {
	if !a.updateNoticeApplies(cmd) {
		return
	}
	// stderr redirected to a file (deploykit status 2>log) is not a person reading: say nothing.
	if f, isFile := w.(*os.File); isFile && !a.forceStyled && !term.IsTerminal(f.Fd()) {
		return
	}
	cache, ok := a.cache()
	cur, isRelease := a.currentRelease()
	if !ok || !isRelease {
		return
	}
	state := cache.Load()
	msg, show := state.Notice(cur, a.now())
	if !show {
		return
	}
	_ = cache.Save(state)
	st := ui.NewStyles(w)
	_, _ = fmt.Fprintf(w, "\n%s %s\n", st.Warn.Render("↑"), st.Muted.Render(msg+" (DEPLOYKIT_NO_UPDATE_CHECK=1 silences this)"))
}
