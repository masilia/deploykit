// Package cloudpanel drives a CloudPanel server through clpctl over a root transport (ADR 0004).
//
// There is no CloudPanel API or login: clpctl runs on the server. Provisioning uses a root (or sudo) transport; the
// site user's transport is only used where a command must run as that user. clpctl has no "site exists", cron or
// custom-nginx-directive command, so existence is detected by filesystem (docs/cloudpanel-cli.md).
package cloudpanel

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/transport"
)

// Resolver looks up DNS names; net.DefaultResolver satisfies it.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Client provisions sites on one server.
type Client struct {
	// Root runs provisioning commands (root, or a sudoer whose commands are prefixed by the transport).
	Root transport.Transport
	// DNS resolves names for certificate decisions. Default net.DefaultResolver.
	DNS Resolver
}

func (c *Client) dns() Resolver {
	if c.DNS != nil {
		return c.DNS
	}
	return net.DefaultResolver
}

// Site kinds accepted by AddSite, matching clpctl site:add:<kind>.
const (
	SiteNodeJS       = "nodejs"
	SitePHP          = "php"
	SiteReverseProxy = "reverse-proxy"
	SiteStatic       = "static"
)

// SiteSpec describes a site to create.
type SiteSpec struct {
	Kind     string
	Domain   string
	SiteUser string
	Password string // site user password; sent in the script on stdin, never in an ssh argument (ADR 0006)

	NodeVersion   string // nodejs
	AppPort       int    // nodejs
	PHPVersion    string // php
	VhostTemplate string // php
	ProxyURL      string // reverse-proxy
}

var (
	domainPattern = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?\.)+[A-Za-z]{2,}$`)
	userPattern   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// ValidDomain reports whether s is a fully qualified domain name.
func ValidDomain(s string) bool { return domainPattern.MatchString(s) }

// ValidSiteUser reports whether s is a safe Linux user name.
func ValidSiteUser(s string) bool { return userPattern.MatchString(s) }

// HasClpctl reports whether clpctl is installed on the server.
func (c *Client) HasClpctl(ctx context.Context) (bool, error) {
	return c.test(ctx, "command -v clpctl >/dev/null")
}

// SiteExists reports whether the site is already provisioned: its root folder exists or nginx has its vhost.
func (c *Client) SiteExists(ctx context.Context, path, domain string) (bool, error) {
	return c.testEnv(ctx, `{ [ -n "$CP_PATH" ] && [ -d "$CP_PATH" ]; } || [ -f "/etc/nginx/sites-enabled/$CP_DOMAIN.conf" ]`,
		map[string]string{"CP_PATH": path, "CP_DOMAIN": domain})
}

func (c *Client) test(ctx context.Context, script string) (bool, error) {
	return c.testEnv(ctx, script, nil)
}

// testEnv runs a predicate script: exit 0 is true, exit 1 is false, anything else is an error.
func (c *Client) testEnv(ctx context.Context, script string, env map[string]string) (bool, error) {
	res, err := c.Root.Run(ctx, transport.Spec{Script: script, Env: env})
	if err == nil {
		return true, nil
	}
	if res.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

const portScript = `{
  ss -ltnH 2>/dev/null | awk '{print $4}' | sed 's/.*://'
  grep -rhoE '(127\.0\.0\.1|localhost|0\.0\.0\.0):[0-9]{2,5}' /etc/nginx/sites-enabled /etc/nginx/conf.d 2>/dev/null | sed 's/.*://'
} | grep -E '^[0-9]+$' | sort -un
`

// UsedPorts lists the ports taken on the server: listening sockets plus the ports other sites' nginx vhosts forward
// to (apps that are stopped but declared).
func (c *Client) UsedPorts(ctx context.Context) ([]int, error) {
	res, err := c.Root.Run(ctx, transport.Spec{Script: portScript})
	if err != nil && res.ExitCode != 1 { // grep -E exits 1 when nothing matches
		return nil, err
	}
	var ports []int
	for _, f := range strings.Fields(res.Stdout) {
		if n, convErr := strconv.Atoi(f); convErr == nil {
			ports = append(ports, n)
		}
	}
	sort.Ints(ports)
	return ports, nil
}

// PickPort returns the first port at or above requested that is not in used.
func PickPort(requested int, used []int) int {
	taken := make(map[int]bool, len(used))
	for _, p := range used {
		taken[p] = true
	}
	for taken[requested] {
		requested++
	}
	return requested
}

// AddSite creates the site with clpctl site:add:<kind>. Not idempotent: call SiteExists first.
func (c *Client) AddSite(ctx context.Context, s SiteSpec) error {
	if !ValidDomain(s.Domain) {
		return fmt.Errorf("invalid domain %q", s.Domain)
	}
	if !ValidSiteUser(s.SiteUser) {
		return fmt.Errorf("invalid site user %q", s.SiteUser)
	}
	if s.Password == "" {
		return fmt.Errorf("a site user password is required")
	}
	env := map[string]string{"CP_DOMAIN": s.Domain, "CP_USER": s.SiteUser}
	flags := `--domainName="$CP_DOMAIN" --siteUser="$CP_USER" --siteUserPassword="$CP_PASSWORD"`
	switch s.Kind {
	case SiteNodeJS:
		if s.NodeVersion == "" || s.AppPort == 0 {
			return fmt.Errorf("nodejs site needs a node version and an app port")
		}
		env["CP_NODE"], env["CP_PORT"] = s.NodeVersion, strconv.Itoa(s.AppPort)
		flags += ` --nodejsVersion="$CP_NODE" --appPort="$CP_PORT"`
	case SitePHP:
		if s.PHPVersion == "" || s.VhostTemplate == "" {
			return fmt.Errorf("php site needs a php version and a vhost template")
		}
		env["CP_PHP"], env["CP_VHOST"] = s.PHPVersion, s.VhostTemplate
		flags += ` --phpVersion="$CP_PHP" --vhostTemplate="$CP_VHOST"`
	case SiteReverseProxy:
		if s.ProxyURL == "" {
			return fmt.Errorf("reverse-proxy site needs a url")
		}
		env["CP_URL"] = s.ProxyURL
		flags += ` --reverseProxyUrl="$CP_URL"`
	case SiteStatic:
	default:
		return fmt.Errorf("unknown site kind %q", s.Kind)
	}
	_, err := c.Root.Run(ctx, transport.Spec{
		Script:  "set -e\nclpctl site:add:" + s.Kind + " " + flags + "\n",
		Env:     env,
		Secrets: map[string]string{"CP_PASSWORD": s.Password},
	})
	return err
}

// ResolvesAll returns the hosts that do not resolve in DNS.
func (c *Client) ResolvesAll(ctx context.Context, hosts []string) (missing []string) {
	for _, h := range hosts {
		if addrs, err := c.dns().LookupHost(ctx, h); err != nil || len(addrs) == 0 {
			missing = append(missing, h)
		}
	}
	return missing
}

// InstallCertificate issues a Let's Encrypt certificate. requireDNS lists names that must resolve first: Let's
// Encrypt fails otherwise, and CloudPanel leaves a self-signed certificate. www.<domain> is added as an alternative
// name only when it resolves (docs/profile-nextjs-cloudpanel.md §8).
func (c *Client) InstallCertificate(ctx context.Context, domain string, requireDNS []string) error {
	if !ValidDomain(domain) {
		return fmt.Errorf("invalid domain %q", domain)
	}
	if missing := c.ResolvesAll(ctx, requireDNS); len(missing) > 0 {
		return fmt.Errorf("DNS does not resolve yet for %s: certificate not requested", strings.Join(missing, ", "))
	}
	script := `clpctl lets-encrypt:install:certificate --domainName="$CP_DOMAIN"`
	env := map[string]string{"CP_DOMAIN": domain}
	if len(c.ResolvesAll(ctx, []string{"www." + domain})) == 0 {
		script += ` --subjectAlternativeName="$CP_SAN"`
		env["CP_SAN"] = "www." + domain
	}
	_, err := c.Root.Run(ctx, transport.Spec{Script: script + "\n", Env: env})
	return err
}

// EnsureLayout creates dirs under root and gives them to user. Idempotent.
func (c *Client) EnsureLayout(ctx context.Context, root, user string, dirs []string) error {
	if !ValidSiteUser(user) {
		return fmt.Errorf("invalid site user %q", user)
	}
	paths := make([]string, len(dirs))
	for i, d := range dirs {
		paths[i] = strings.TrimRight(root, "/") + "/" + d
	}
	_, err := c.Root.Run(ctx, transport.Spec{
		Script: "set -e\nmkdir -p " + transport.QuoteArgs(paths) + "\nchown " + user + ":" + user + " " + transport.QuoteArgs(paths) + "\n",
	})
	return err
}

// InstallAuthorizedKey adds pubkey to the site user's authorized_keys (modes 700/600, correct owner, no duplicate).
func (c *Client) InstallAuthorizedKey(ctx context.Context, user, pubkey string) error {
	if !ValidSiteUser(user) {
		return fmt.Errorf("invalid site user %q", user)
	}
	pubkey = strings.TrimSpace(pubkey)
	if pubkey == "" || strings.ContainsAny(pubkey, "\r\n") {
		return fmt.Errorf("invalid public key")
	}
	_, err := c.Root.Run(ctx, transport.Spec{
		Env: map[string]string{"CP_USER": user, "CP_KEY": pubkey},
		Script: `set -e
H=$(getent passwd "$CP_USER" | cut -d: -f6)
[ -n "$H" ] || { echo "no such user: $CP_USER" >&2; exit 1; }
mkdir -p "$H/.ssh" && chmod 700 "$H/.ssh"
touch "$H/.ssh/authorized_keys" && chmod 600 "$H/.ssh/authorized_keys"
grep -qxF "$CP_KEY" "$H/.ssh/authorized_keys" || printf '%s\n' "$CP_KEY" >> "$H/.ssh/authorized_keys"
chown -R "$CP_USER:$CP_USER" "$H/.ssh"
`,
	})
	return err
}

var startupPattern = regexp.MustCompile(`^sudo env PATH=\$PATH:[A-Za-z0-9_./:-]+ [A-Za-z0-9_./-]+ startup systemd -u [A-Za-z0-9._-]+ --hp [A-Za-z0-9_./-]+$`)

// EnablePM2Boot makes pm2 start at boot for the site user. pm2 prints the exact command to run as root; it is run
// only if it has the expected shape, and without the `sudo` prefix (we are already root). Never run pm2 list, kill or
// save as root: that starts a pm2 daemon for root (docs/profile-nextjs-cloudpanel.md, pitfall 7). site runs as the
// site user.
func (c *Client) EnablePM2Boot(ctx context.Context, site transport.Transport, user string) (changed bool, err error) {
	if !ValidSiteUser(user) {
		return false, fmt.Errorf("invalid site user %q", user)
	}
	res, err := site.Run(ctx, transport.Spec{
		Script: `pm2 startup systemd -u "$CP_USER" --hp "$HOME" 2>&1 | grep -E '^sudo ' | head -1 || true` + "\n",
		Env:    map[string]string{"CP_USER": user},
	})
	if err != nil {
		return false, err
	}
	line := strings.TrimSpace(res.Stdout)
	if line == "" {
		return false, nil // already set up, or pm2 printed nothing to run
	}
	if !startupPattern.MatchString(line) {
		return false, fmt.Errorf("pm2 startup printed a command of an unexpected shape; run it by hand: %s", line)
	}
	if _, err := c.Root.Run(ctx, transport.Spec{Script: strings.TrimPrefix(line, "sudo ") + "\n"}); err != nil {
		return false, err
	}
	return true, nil
}
