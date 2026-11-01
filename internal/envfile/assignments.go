package envfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Assignment is one KEY=VALUE pair received on stdin or from an --env file.
type Assignment struct {
	Key   string
	Value string
}

// ParseAssignments reads the env-set.sh input format: KEY=VALUE lines, with blank lines and lines starting with '#'
// ignored. The value is everything after the first '='. A malformed line is an error that names the line number but
// never echoes the line, because it may contain a secret (the shell script printed it). Callers decide whether an
// empty result is acceptable (env-set.sh refused it).
func ParseAssignments(r io.Reader) ([]Assignment, error) {
	var out []Assignment
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", n)
		}
		if !ValidKey(key) {
			return nil, fmt.Errorf("line %d: invalid key %q", n, key)
		}
		out = append(out, Assignment{Key: key, Value: value})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
