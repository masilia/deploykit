package scaffold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// detectPHP recognises a PHP application: a composer.json at the root or in a direct subfolder that also has public/index.php
// (Ibexa projects keep the application in ibexa/).
func detectPHP(dir string, f *Findings) bool {
	for _, sub := range append([]string{""}, subdirs(dir)...) {
		app := filepath.Join(dir, sub)
		if _, err := os.Stat(filepath.Join(app, "public", "index.php")); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(app, "composer.json")) //nolint:gosec // a file of the project being scaffolded
		if err != nil {
			continue
		}
		var c struct {
			Name    string            `json:"name"`
			Require map[string]string `json:"require"`
		}
		if json.Unmarshal(data, &c) != nil {
			continue
		}
		f.PHPDir = sub
		f.Framework = framework(c.Require)
		f.PHPVersion = phpMinor(c.Require["php"])
		f.Encore = c.Require["symfony/webpack-encore-bundle"] != "" || f.Framework == "ibexa" && fileExists(filepath.Join(app, "package.json"))
		f.Console = consoleCommands(c.Require)
		// A bundle that arrives as a dependency of the framework is not in composer.json, but its configuration file is in the project.
		f.JWT = c.Require["lexik/jwt-authentication-bundle"] != "" || fileExists(filepath.Join(app, "config", "packages", "lexik_jwt_authentication.yaml"))
		if f.Framework == "ibexa" && fileExists(filepath.Join(app, "config", "packages", "graphql.yaml")) {
			f.Console = appendOnce(f.Console, "ibexa:graphql:generate-schema")
			sort.Strings(f.Console)
		}
		f.pkgName = c.Name[strings.LastIndex(c.Name, "/")+1:]
		if f.Framework == "ibexa" {
			f.pkgName = "" // the skeleton is called oss-skeleton: the folder or the repository names the project better
		}
		full(app, dir, f)
		return true
	}
	return false
}

func subdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "node_modules" && e.Name() != "vendor" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func framework(require map[string]string) string {
	for name := range require {
		if strings.HasPrefix(name, "ibexa/") || strings.HasPrefix(name, "ezsystems/") {
			return "ibexa"
		}
	}
	if require["symfony/framework-bundle"] != "" || require["symfony/symfony"] != "" {
		return "symfony"
	}
	return "php"
}

var minorPattern = regexp.MustCompile(`(\d+)\.(\d+)`)

// phpMinor reads the lowest PHP version a constraint allows: ">=8.2" and "^8.2" → "8.2", "8.1.*" → "8.1".
func phpMinor(constraint string) string {
	if m := minorPattern.FindStringSubmatch(constraint); m != nil {
		return m[1] + "." + m[2]
	}
	return ""
}

// consoleCommands lists the commands a deploy usually runs, from the packages the project requires.
func consoleCommands(require map[string]string) []string {
	var out []string
	for name := range require {
		switch {
		case strings.HasPrefix(name, "kaliop/") && strings.Contains(name, "migration"), name == "tanoconsulting/ibexa-migration-bundle":
			out = appendOnce(out, "kaliop:migration:migrate")
		case name == "doctrine/doctrine-migrations-bundle":
			out = appendOnce(out, "doctrine:migrations:migrate")
		case name == "ibexa/graphql":
			out = appendOnce(out, "ibexa:graphql:generate-schema")
		}
	}
	sort.Strings(out)
	return out
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// full looks for what a CloudPanel PHP site cannot provide: Redis, Solr, Varnish.
func full(app, root string, f *Findings) {
	add := func(reason string) {
		f.Full = true
		f.FullReasons = append(f.FullReasons, reason)
	}
	if matches, _ := filepath.Glob(filepath.Join(app, "config", "packages", "cache_pool", "*redis*")); len(matches) > 0 {
		add("a Redis cache pool (" + filepath.Base(matches[0]) + ")")
	}
	if fileExists(filepath.Join(app, "config", "packages", "ibexa_solr.yaml")) {
		add("Solr (config/packages/ibexa_solr.yaml)")
	}
	for _, base := range []string{root, app} {
		var vcl []string
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && (d.Name() == "vendor" || d.Name() == "node_modules" || d.Name() == "var" || d.Name() == ".git") {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".vcl") {
				vcl = append(vcl, d.Name())
			}
			return nil
		})
		if len(vcl) > 0 {
			add("Varnish (" + vcl[0] + ")")
			return
		}
	}
}
