// Package doctor checks that the tools a deployment needs exist, here and on the server, before anything is changed.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/transport"
)

// LoginFailed is the detail of a check that could not run because ssh could not log in.
const LoginFailed = "cannot log in over SSH"

// Level is the severity of a check result.
type Level int

// Levels, in increasing severity.
const (
	OK Level = iota
	Warn
	Fail
)

func (l Level) String() string { return [...]string{"ok", "warn", "FAIL"}[l] }

// Result is one line of the report.
type Result struct {
	Where  string // "local" or the transport description
	Name   string
	Level  Level
	Detail string
}

// Tool describes a command whose presence, and optionally minimum version, is checked.
type Tool struct {
	Name     string // shown in the report
	Command  string // shell snippet that prints the version line, or fails
	Min      string // minimum version, "" for presence only
	Required bool   // a missing optional tool is a warning
}

var versionPattern = regexp.MustCompile(`\d+(\.\d+)+`)

// AtLeast reports whether have (any text containing a dotted version, such as "Docker Compose version v2.27.0") is
// at least want ("2.24").
func AtLeast(have, want string) bool {
	h := versionPattern.FindString(have)
	if h == "" {
		return false
	}
	hp, wp := strings.Split(h, "."), strings.Split(want, ".")
	for i := 0; i < len(wp); i++ {
		w, _ := strconv.Atoi(wp[i])
		v := 0
		if i < len(hp) {
			v, _ = strconv.Atoi(hp[i])
		}
		if v != w {
			return v > w
		}
	}
	return true
}

// Check runs the tools on t and reports each.
func Check(ctx context.Context, t transport.Transport, tools []Tool) []Result {
	var out []Result
	for _, tool := range tools {
		res, err := t.Run(ctx, transport.Spec{Script: tool.Command + " 2>&1\n"})
		r := Result{Where: t.Describe(), Name: tool.Name}
		text := strings.TrimSpace(firstLine(res.Stdout))
		var exit *transport.ExitError
		switch {
		case errors.As(err, &exit) && exit.Code == 255:
			// ssh itself failed (exit 255): the tool was never looked for.
			r.Level, r.Detail = Fail, LoginFailed
		case err != nil:
			r.Level, r.Detail = Fail, "not found"
			if !tool.Required {
				r.Level = Warn
			}
		case tool.Min != "" && !AtLeast(text, tool.Min):
			r.Level, r.Detail = Fail, fmt.Sprintf("%s, need %s or newer", text, tool.Min)
		default:
			r.Level, r.Detail = OK, text
		}
		out = append(out, r)
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// LocalTools lists what the workstation needs for a manifest.
func LocalTools(m *manifest.Manifest) []Tool {
	tools := []Tool{
		{Name: "git", Command: "git --version", Required: true},
		{Name: "ssh", Command: "ssh -V", Required: true},
		{Name: "rsync", Command: "rsync --version | head -1", Required: m.Artifact.Type == manifest.ArtifactReleaseDir},
	}
	switch m.Artifact.Type {
	case manifest.ArtifactReleaseDir:
		if rd, err := m.ReleaseDir(); err == nil && rd.Build.Image != "" {
			// The build runs in a container: Docker is all it needs here.
			tools = append(tools, Tool{Name: "docker", Command: "docker --version", Required: true})
			break
		}
		if m.Runtime.Type == manifest.RuntimePHPFPM {
			// A PHP release is built here: PHP and composer, and node with yarn (or corepack) for the assets.
			tools = append(tools,
				Tool{Name: "php", Command: "php -v | head -1", Required: true},
				Tool{Name: "composer", Command: "composer --version 2>/dev/null | head -1", Required: true},
				Tool{Name: "node", Command: "node -v", Required: false},
				Tool{Name: "yarn", Command: "yarn -v 2>/dev/null || corepack yarn -v 2>/dev/null", Required: false},
			)
			break
		}
		tools = append(tools,
			Tool{Name: "node", Command: "node -v", Required: true},
			Tool{Name: "npm", Command: "npm -v", Required: false},
		)
	case manifest.ArtifactImage:
		tools = append(tools, Tool{Name: "docker", Command: "docker --version", Required: true})
	}
	return tools
}

// ServerTools lists what the server needs for a manifest. root is true for the provisioning identity.
func ServerTools(m *manifest.Manifest, root bool) []Tool {
	tools := []Tool{{Name: "curl", Command: "curl --version | head -1", Required: true}}
	switch m.Runtime.Type {
	case manifest.RuntimePM2:
		if root {
			tools = append(tools, Tool{Name: "clpctl", Command: "command -v clpctl", Required: true})
		} else {
			tools = append(tools,
				Tool{Name: "node", Command: "node -v", Required: false}, // absent before bootstrap installs the site
				Tool{Name: "pm2", Command: "pm2 -v", Required: false},
				Tool{Name: "rsync", Command: "rsync --version | head -1", Required: true},
			)
		}
	case manifest.RuntimePHPFPM:
		if root {
			tools = append(tools, Tool{Name: "clpctl", Command: "command -v clpctl", Required: true})
		} else if php, err := m.PHP(); err == nil {
			// Absent before the bootstrap has created the site.
			tools = append(tools,
				Tool{Name: "php" + php.PHPVersion, Command: "php" + php.PHPVersion + " -v | head -1", Required: false},
				Tool{Name: "rsync", Command: "rsync --version | head -1", Required: true},
			)
		}
	case manifest.RuntimeCompose:
		tools = append(tools,
			Tool{Name: "docker", Command: "docker --version", Required: true},
			Tool{Name: "docker compose", Command: "docker compose version", Min: "2.24", Required: true},
			Tool{Name: "gpg", Command: "gpg --version | head -1", Required: true},
			Tool{Name: "crontab", Command: "command -v crontab", Required: true},
		)
		if root {
			tools = append(tools, Tool{Name: "clpctl", Command: "command -v clpctl", Required: false})
		}
	}
	return tools
}

// Worst returns the most severe level of a report.
func Worst(rs []Result) Level {
	w := OK
	for _, r := range rs {
		if r.Level > w {
			w = r.Level
		}
	}
	return w
}

// AllLoginFailed reports whether every result failed because ssh could not log in, which is what the site user looks
// like before the bootstrap has created it.
func AllLoginFailed(rs []Result) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if r.Detail != LoginFailed {
			return false
		}
	}
	return true
}
