package compose

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/tmpl"
)

// tlsSite is one name the standalone proxy serves over HTTPS: its certificate lives in the letsencrypt volume, its server
// blocks in the file written from the template.
type tlsSite struct {
	Domain   string
	Template string
	Target   string
}

// tlsSites lists the names to secure: DOMAIN first, then each extra domain whose `.env` value is set. An extra domain whose
// value is empty is reported and left out (the name is optional for the project); one that is not a valid name is an error.
func tlsSites(out io.Writer, st *manifest.Standalone, doc *envfile.Doc) ([]tlsSite, error) {
	domain := doc.Get("DOMAIN", "")
	vars := tmpl.Vars{"target": map[string]string{"domain": domain}, "env": envMap(doc)}
	target, err := tmpl.Render(st.TLS.DomainConf.Target, vars, false)
	if err != nil {
		return nil, err
	}
	sites := []tlsSite{{Domain: domain, Template: st.TLS.DomainConf.Template, Target: target}}
	for i, e := range st.TLS.ExtraDomains {
		d, err := tmpl.Render(e.Domain, vars, false)
		if err != nil {
			return nil, fmt.Errorf("ingress.tls.extra_domains[%d].domain: %w", i, err)
		}
		d = strings.TrimSpace(d)
		if d == "" {
			_, _ = fmt.Fprintf(out, "extra domain %q is empty in the .env: skipped\n", e.Domain)
			continue
		}
		if !domainPattern.MatchString(d) {
			return nil, fmt.Errorf("ingress.tls.extra_domains[%d]: %q is not a valid domain name", i, d)
		}
		sites = append(sites, tlsSite{Domain: d, Template: e.Template, Target: e.Target})
	}
	return sites, nil
}

// EnsureTLS issues the Let's Encrypt certificate of every name the proxy serves and writes its server blocks, reloading the
// proxy when it does. Each name is handled on its own and a name that already has its certificate and its file is left alone, so
// it is safe to run on every update. The certificates are renewed together by `renew-certs`. Every name must resolve to this server
// and port 80 must reach the proxy, which answers the challenge.
func EnsureTLS(ctx context.Context, out io.Writer, s *Stack, st *manifest.Standalone, doc *envfile.Doc) error {
	if len(st.TLS.Volumes) < 2 {
		return fmt.Errorf("ingress.tls.volumes must list the letsencrypt and certbot-www volumes")
	}
	sites, err := tlsSites(out, st, doc)
	if err != nil {
		return err
	}
	var failed []string
	for _, site := range sites {
		if _, err := s.Script(ctx, out, tlsScript, map[string]string{
			"DK_LE": s.Project + "_" + st.TLS.Volumes[0], "DK_WWW": s.Project + "_" + st.TLS.Volumes[1],
			"DK_DOMAIN": site.Domain, "DK_EMAIL": doc.Get("LETSENCRYPT_EMAIL", ""), "DK_CONF": site.Target, "DK_TEMPLATE": site.Template,
		}, nil); err != nil {
			_, _ = fmt.Fprintf(out, "TLS for %s failed: %v\n", site.Domain, err)
			failed = append(failed, site.Domain)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("TLS could not be set up for %s", strings.Join(failed, ", "))
	}
	return nil
}

// tlsScript secures one name: certificate first (the proxy answers the challenge on port 80), then the server blocks.
// The file is written only once the certificate exists, because nginx refuses to load a block whose certificate is missing.
const tlsScript = `set -e
if docker volume inspect "$DK_LE" >/dev/null 2>&1 && docker run --rm -v "$DK_LE:/etc/letsencrypt" ` + helperImage + ` test -d "/etc/letsencrypt/live/$DK_DOMAIN"; then
  echo "certificate already present for $DK_DOMAIN"
else
  docker run --rm -v "$DK_WWW:/var/www/certbot" -v "$DK_LE:/etc/letsencrypt" certbot/certbot certonly --webroot -w /var/www/certbot -d "$DK_DOMAIN" --agree-tos -m "$DK_EMAIL" --non-interactive \
    || { echo "certificate issuance failed (does the DNS point at this machine? is port 80 open?)" >&2; exit 1; }
fi
if [ ! -f "$DK_CONF" ]; then
  sed "s|__DOMAIN__|$DK_DOMAIN|g" "$DK_TEMPLATE" > "$DK_CONF"
  compose exec -T proxy nginx -s reload
  echo "TLS enabled for $DK_DOMAIN"
fi
`
