package main

import (
	"os"
	"strings"
)

// defaultPath is where docker, gpg, tar, rsync and friends live on the servers deploykit manages.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ensurePath makes sure the standard directories are searched. A shell falls back to a built-in default when PATH is
// unset, a Go program does not: started by cron with no PATH, deploykit could not find `cat`, and the monitoring it had
// just replaced stopped working without a sound. Directories already in PATH keep their order and come first.
func ensurePath(getenv func(string) string, setenv func(string, string) error) error {
	current := getenv("PATH")
	have := map[string]bool{}
	var parts []string
	for _, d := range strings.Split(current, ":") {
		if d != "" && !have[d] {
			have[d] = true
			parts = append(parts, d)
		}
	}
	for _, d := range strings.Split(defaultPath, ":") {
		if !have[d] {
			have[d] = true
			parts = append(parts, d)
		}
	}
	if joined := strings.Join(parts, ":"); joined != current {
		return setenv("PATH", joined)
	}
	return nil
}

func init() {
	_ = ensurePath(os.Getenv, os.Setenv)
}
