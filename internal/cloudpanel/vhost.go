package cloudpanel

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/masilia/deploykit/internal/transport"
)

// TemplateSpec describes a custom vhost template derived from a system one.
type TemplateSpec struct {
	Name string // the new template
	Base string // an existing template, for example "Symfony 7"
	// RootDirectory is relative to the site folder: `current/public` makes the document root the public folder of the
	// release the current link points at.
	RootDirectory string
	PHPVersion    string
}

var templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

const (
	documentRootFilename = "fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;"
	realpathFilename     = "fastcgi_param SCRIPT_FILENAME $realpath_root$fastcgi_script_name;"
)

// PHPTemplate derives the body of a custom template from the text of a base one: the first line carries the root
// directory and PHP version that CloudPanel prefills (`#{"rootDirectory":…}`), and the script path given to PHP-FPM is the
// real path of the document root. With a symlinked release the web server then hands PHP-FPM the path of the release
// itself, so opcache and the realpath cache never see the old release after the link moves and no reload is needed.
func PHPTemplate(base, rootDirectory, phpVersion string) (string, error) {
	if strings.Count(base, documentRootFilename) == 0 {
		return "", fmt.Errorf("the base template has no %q line to adapt: pick a PHP template whose PHP-FPM location uses $document_root", documentRootFilename)
	}
	header, err := json.Marshal(map[string]string{"rootDirectory": rootDirectory, "phpVersion": phpVersion})
	if err != nil {
		return "", err
	}
	body := strings.ReplaceAll(base, documentRootFilename, realpathFilename+"\n    fastcgi_param DOCUMENT_ROOT $realpath_root;")
	return "#" + string(header) + "\n" + strings.TrimLeft(body, "\n"), nil
}

// EnsureVhostTemplate creates the custom template from its base unless a template of that name exists. Idempotent: an
// existing template is left alone (edit or delete it in CloudPanel to change it).
func (c *Client) EnsureVhostTemplate(ctx context.Context, s TemplateSpec) (created bool, err error) {
	if !templateName.MatchString(s.Name) || !templateName.MatchString(s.Base) {
		return false, fmt.Errorf("invalid vhost template name %q or base %q", s.Name, s.Base)
	}
	listed, err := c.Root.Run(ctx, transport.Spec{Script: "clpctl vhost-templates:list\n"})
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(listed.Stdout, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) > 2 && strings.TrimSpace(fields[1]) == s.Name {
			return false, nil
		}
	}
	view, err := c.Root.Run(ctx, transport.Spec{Script: `clpctl vhost-template:view --name="$CP_BASE"` + "\n", Env: map[string]string{"CP_BASE": s.Base}})
	if err != nil {
		return false, fmt.Errorf("read the base template %q: %w", s.Base, err)
	}
	body, err := PHPTemplate(view.Stdout, s.RootDirectory, s.PHPVersion)
	if err != nil {
		return false, err
	}
	// The body travels on stdin and is written by the server's own shell: no quoting problem, nothing in an argument.
	_, err = c.Root.Run(ctx, transport.Spec{
		Script: "set -e\nf=$(mktemp)\ntrap 'rm -f \"$f\"' EXIT\ncat > \"$f\" <<'DEPLOYKIT_TEMPLATE_END'\n" + body +
			"\nDEPLOYKIT_TEMPLATE_END\n" + `clpctl vhost-template:add --name="$CP_NAME" --file="$f"` + "\n",
		Env: map[string]string{"CP_NAME": s.Name},
	})
	if err != nil {
		return false, fmt.Errorf("create the vhost template %q: %w", s.Name, err)
	}
	return true, nil
}
