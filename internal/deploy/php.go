package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// phpRuntime serves the release from the PHP-FPM pool of a CloudPanel PHP site. The web server follows the current link
// (its vhost template resolves it with $realpath_root), so a switch needs no reload.
type phpRuntime struct{ n *Node }

func (r *phpRuntime) name() string { return "php-fpm" }

func (r *phpRuntime) release() string { return r.n.layout.Release(r.n.stamp) }

func (r *phpRuntime) preflightPlan() string {
	return "check the server: layout, required shared files, PHP version and extensions"
}

func (r *phpRuntime) preflight(ctx context.Context, out io.Writer) error {
	n, php := r.n, r.n.php
	bin := "php" + php.PHPVersion
	if _, err := n.Site.Run(ctx, transport.Spec{Argv: []string{"sh", "-c", `command -v "$1" >/dev/null`, "sh", bin}}); err != nil {
		return fmt.Errorf("%s is not installed on the server (CloudPanel offers it per site: check the PHP version of the site)", bin)
	}
	for _, s := range php.Shared {
		if !s.Required {
			continue
		}
		file := path.Join(n.layout.Shared(), path.Clean(s.Path))
		if _, err := n.Site.Run(ctx, transport.Spec{Argv: []string{"test", "-f", file}}); err != nil {
			return fmt.Errorf("%s is missing: create it on the server (it holds the secrets and never travels with a release), then deploy again", file)
		}
	}
	if php.PHPExtensions == "none" {
		return nil
	}
	want, err := composerExtensions(filepath.Join(n.buildDir, n.rd.Build.Workdir, "composer.json"))
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return nil
	}
	res, err := n.Site.Run(ctx, transport.Spec{Argv: []string{bin, "-m"}})
	if err != nil {
		return fmt.Errorf("%s -m: %w", bin, err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		have[strings.ToLower(strings.TrimSpace(line))] = true
	}
	var missing []string
	for _, e := range want {
		if !have[e] && !have[strings.ReplaceAll(e, "-", " ")] {
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s on the server lacks the extension(s) %s required by composer.json: install the matching %s-* packages as root (for example `apt install %s-%s`); deploykit never installs system packages",
			bin, strings.Join(missing, ", "), bin, bin, missing[0])
	}
	_, _ = fmt.Fprintf(out, "%s: the %d extension(s) of composer.json are installed\n", bin, len(want))
	return nil
}

// composerExtensions lists the ext-* requirements of a composer.json (names without the ext- prefix, lower case).
func composerExtensions(file string) ([]string, error) {
	data, err := os.ReadFile(file) //nolint:gosec // the composer.json of the project being deployed
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c struct {
		Require map[string]string `json:"require"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var out []string
	for name := range c.Require {
		if ext, ok := strings.CutPrefix(strings.ToLower(name), "ext-"); ok {
			out = append(out, ext)
		}
	}
	sort.Strings(out)
	return out, nil
}

// afterUpload links the shared files and folders into the release and creates the writable ones.
func (r *phpRuntime) afterUpload(ctx context.Context, out io.Writer) error {
	n, php := r.n, r.n.php
	var script strings.Builder
	script.WriteString("set -e\ncd \"$DK_RELEASE\"\n")
	for _, s := range php.Shared {
		p := path.Clean(s.Path)
		shared := path.Join(n.layout.Shared(), p)
		kind := "dir"
		if s.Kind == "file" {
			kind = "file"
		}
		fmt.Fprintf(&script, "link_%s %s %s\n", kind, transport.Quote(p), transport.Quote(shared))
	}
	for _, w := range php.Writable {
		script.WriteString("mkdir -p " + transport.Quote(path.Clean(w)) + "\n")
	}
	if _, err := n.Site.Run(ctx, transport.Spec{
		Script: linkHelpers + script.String(),
		Env:    map[string]string{"DK_RELEASE": r.release()},
		Out:    out,
	}); err != nil {
		return fmt.Errorf("link the shared files: %w", err)
	}
	return nil
}

// linkHelpers are the shell functions of the script above. A shared folder that does not exist yet is created and seeded
// with what the release has at that path (an empty skeleton such as public/var); then the release's own copy is replaced
// by a link to the shared one.
const linkHelpers = `link_dir() {
  rel=$1; shared=$2
  if [ ! -e "$shared" ]; then
    mkdir -p "$shared"
    if [ -d "$rel" ] && [ ! -L "$rel" ]; then cp -a "$rel/." "$shared/"; fi
  fi
  rm -rf "$rel"
  mkdir -p "$(dirname "$rel")"
  ln -sfn "$shared" "$rel"
}
link_file() {
  rel=$1; shared=$2
  if [ ! -e "$shared" ] && [ -f "$rel" ] && [ ! -L "$rel" ]; then
    mkdir -p "$(dirname "$shared")"
    cp -a "$rel" "$shared"
  fi
  rm -f "$rel"
  mkdir -p "$(dirname "$rel")"
  if [ -e "$shared" ]; then ln -sfn "$shared" "$rel"; fi
}
`

func (r *phpRuntime) preparePlan() (string, bool) {
	if len(r.n.php.Prepare) == 0 {
		return "", false
	}
	return "run " + strings.Join(r.n.php.Prepare, ", ") + " in the new release", true
}

func (r *phpRuntime) prepare(ctx context.Context, out io.Writer) error {
	for _, name := range r.n.php.Prepare {
		if err := r.runCommand(ctx, r.release(), name, out); err != nil {
			return err
		}
	}
	return nil
}

// runCommand runs a named command of the manifest in dir as the site user.
func (r *phpRuntime) runCommand(ctx context.Context, dir, name string, out io.Writer) error {
	n := r.n
	raw, ok := n.php.Commands[name]
	if !ok {
		return fmt.Errorf("command %q is not declared in runtime.commands (declared: %s)", name, strings.Join(n.php.CommandNames(), ", "))
	}
	cmd, err := tmpl.Render(raw, n.vars, true)
	if err != nil {
		return fmt.Errorf("runtime.commands.%s: %w", name, err)
	}
	_, _ = fmt.Fprintf(out, "$ %s\n", name)
	if _, err = n.Site.Run(ctx, transport.Spec{Script: "set -e\n" + cmd + "\n", Dir: dir, Env: n.php.Env, Out: out}); err != nil {
		return fmt.Errorf("command %s failed: %w", name, err)
	}
	return nil
}

func (r *phpRuntime) activatePlan() string {
	return "switch the current link, check that the site answers past Varnish, roll back on failure"
}

func (r *phpRuntime) reloadPlan() string {
	if r.n.php.Reload == "root-systemctl" {
		return "reload php" + r.n.php.PHPVersion + "-fpm as root"
	}
	return "nothing to reload (the vhost follows the link)"
}

func (r *phpRuntime) reloadLabel() string { return "php-fpm reload" }

func (r *phpRuntime) reload(ctx context.Context) error {
	if r.n.php.Reload != "root-systemctl" {
		return nil
	}
	if r.n.Root == nil {
		return errors.New("reload: root-systemctl needs the provisioning (root) identity")
	}
	_, err := r.n.Root.Run(ctx, transport.Spec{Argv: []string{"systemctl", "reload", "php" + r.n.php.PHPVersion + "-fpm"}})
	return err
}

func (r *phpRuntime) health(ctx context.Context) (string, bool, error) {
	h := r.n.php.Health
	return HealthyHTTP(ctx, r.n.Site, h.Backend, r.n.Target.Get("domain"), h.Path, h.Expect, h.Tries, h.Interval)
}

func (r *phpRuntime) healthy() string {
	return fmt.Sprintf("answers %v on %s", r.n.php.Health.Expect, r.n.php.Health.Path)
}

func (r *phpRuntime) healthFailure(last string) error {
	h := r.n.php.Health
	return fmt.Errorf("the new release does not answer %v on %s%s (last code %s)", h.Expect, h.Backend, h.Path, last)
}

func (r *phpRuntime) rollbackFailure(last string) error {
	return fmt.Errorf("the site answers %s after the rollback, check the PHP-FPM and nginx logs of the site", last)
}

func (r *phpRuntime) afterSwitchPlan() (string, bool) {
	if len(r.n.php.AfterSwitch) == 0 {
		return "", false
	}
	return "run " + strings.Join(r.n.php.AfterSwitch, ", ") + " in the live release", true
}

func (r *phpRuntime) afterSwitch(ctx context.Context, out io.Writer) error {
	var errs []error
	for _, name := range r.n.php.AfterSwitch {
		if err := r.runCommand(ctx, r.n.layout.CurrentLink(), name, out); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (r *phpRuntime) recover(ctx context.Context, out io.Writer, back bool) {
	if !back || len(r.n.php.AfterSwitch) == 0 {
		return
	}
	if err := r.afterSwitch(ctx, out); err != nil {
		_, _ = fmt.Fprintf(out, "warning: after the rollback: %v\n", err)
	}
}

// HealthyHTTP asks the server itself to fetch path from the site's backend with the site's Host header, up to tries
// times interval apart, until the status is one of expect. backend is where the site's own web server listens (on a
// CloudPanel PHP site 127.0.0.1:8080, which is past Varnish). It returns the last status seen.
func HealthyHTTP(ctx context.Context, t transport.Transport, backend, host, path string, expect []int, tries int, interval time.Duration) (last string, ok bool, err error) {
	if !strings.HasPrefix(path, "/") {
		return "", false, fmt.Errorf("health path %q must start with /", path)
	}
	codes := make([]string, len(expect))
	for i, c := range expect {
		codes[i] = strconv.Itoa(c)
	}
	script := `code=000
i=0
while [ "$i" -lt "$DK_TRIES" ]; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $DK_HOST" "http://$DK_BACKEND$DK_PATH" || true)
  case " $DK_EXPECT " in *" $code "*) echo "$code"; exit 0 ;; esac
  i=$((i + 1))
  sleep "$DK_SLEEP"
done
echo "$code"
exit 1
`
	res, runErr := t.Run(ctx, transport.Spec{Script: script, Env: map[string]string{
		"DK_TRIES": strconv.Itoa(tries), "DK_BACKEND": backend, "DK_HOST": host, "DK_PATH": path,
		"DK_EXPECT": strings.Join(codes, " "), "DK_SLEEP": strconv.FormatFloat(interval.Seconds(), 'f', -1, 64),
	}})
	last = strings.TrimSpace(res.Stdout)
	if runErr == nil {
		return last, true, nil
	}
	if res.ExitCode == 1 {
		return last, false, nil
	}
	return last, false, runErr
}
