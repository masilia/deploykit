package compose

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/secrets"
	"github.com/masilia/deploykit/internal/transport"
)

// RemoteEnv is an env file on the machine of a transport. Content always travels on stdin, so secrets never reach an
// argument list.
type RemoteEnv struct {
	T    transport.Transport
	Path string
}

// Load reads the file. A missing file is an error.
func (e RemoteEnv) Load(ctx context.Context) (*envfile.Doc, error) {
	res, err := e.T.Run(ctx, transport.Spec{Argv: []string{"cat", e.Path}})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", e.Path, err)
	}
	return envfile.Parse([]byte(res.Stdout)), nil
}

// Save replaces the file atomically, keeping its mode (new files are 0600).
func (e RemoteEnv) Save(ctx context.Context, d *envfile.Doc) error {
	_, err := e.T.Run(ctx, transport.Spec{
		Argv: []string{"sh", "-c", `umask 077
tmp="$1.tmp.$$"
cat > "$tmp"
if [ -f "$1" ]; then chmod --reference="$1" "$tmp" 2>/dev/null || true; fi
mv -f "$tmp" "$1"`, "sh", e.Path},
		Stdin: strings.NewReader(string(d.Bytes())),
	})
	return err
}

// Apply loads the file, sets the assignments and saves it.
func (e RemoteEnv) Apply(ctx context.Context, as []envfile.Assignment) error {
	d, err := e.Load(ctx)
	if err != nil {
		return err
	}
	if err := d.Apply(as); err != nil {
		return err
	}
	return e.Save(ctx, d)
}

// EnsureSecrets generates every secret of the manifest that is absent, empty or still a CHANGE_ME placeholder, and
// never overwrites a value that is already set (legacy ens_gen). It returns the keys it generated.
func EnsureSecrets(d *envfile.Doc, spec map[string]struct {
	HexBytes int `yaml:"hex_bytes"`
}) ([]string, error) {
	var generated []string
	for _, key := range sortedSecretKeys(spec) {
		cur := d.Get(key, "")
		if cur != "" && !strings.HasPrefix(cur, "CHANGE_ME") {
			continue
		}
		v, err := secrets.Hex(spec[key].HexBytes)
		if err != nil {
			return nil, err
		}
		if err := d.Set(key, v); err != nil {
			return nil, err
		}
		generated = append(generated, key)
	}
	return generated, nil
}

func sortedSecretKeys(m map[string]struct {
	HexBytes int `yaml:"hex_bytes"`
}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// ErrBusy means another operation holds the lock.
type ErrBusy struct{ PID string }

func (e *ErrBusy) Error() string {
	return "an operation is already running (pid " + e.PID + ": update, backup or restore); try again later"
}

// Lock takes the operations lock of the deployment root: an atomic mkdir of the lock directory holding the owner's pid.
// A lock whose process is dead is taken over (legacy acquire_lock). pid is the process id on the machine of the
// transport, so Lock is meant for the Local transport on the server.
func (s *Stack) Lock(ctx context.Context, pid int) (unlock func(context.Context) error, err error) {
	dir := s.Cfg.Lock
	res, runErr := s.T.Run(ctx, transport.Spec{
		Dir: s.Root,
		Script: `if ! mkdir "$DK_LOCK" 2>/dev/null; then
  old=$(cat "$DK_LOCK/pid" 2>/dev/null || true)
  if [ -n "$old" ] && kill -0 "$old" 2>/dev/null; then echo "$old"; exit 3; fi
  rm -rf "$DK_LOCK"
  mkdir "$DK_LOCK" 2>/dev/null || exit 4
fi
printf '%s' "$DK_PID" > "$DK_LOCK/pid"
`,
		Env: map[string]string{"DK_LOCK": dir, "DK_PID": strconv.Itoa(pid)},
	})
	if runErr != nil {
		if res.ExitCode == 3 {
			return nil, &ErrBusy{PID: strings.TrimSpace(res.Stdout)}
		}
		return nil, fmt.Errorf("cannot take the lock %s: %w", dir, runErr)
	}
	return func(ctx context.Context) error {
		_, err := s.T.Run(ctx, transport.Spec{Argv: []string{"rm", "-rf", dir}, Dir: s.Root})
		return err
	}, nil
}

// Journal appends a line to the versions journal: `YYYY-MM-DD HH:MM:SS<TAB>text`.
func (s *Stack) Journal(ctx context.Context, now time.Time, text string) error {
	line := now.Format("2006-01-02 15:04:05") + "\t" + text + "\n"
	_, err := s.T.Run(ctx, transport.Spec{
		Argv:  []string{"sh", "-c", `cat >> "$1"`, "sh", s.Cfg.Journal},
		Dir:   s.Root,
		Stdin: strings.NewReader(line),
	})
	return err
}

// FreeMB returns the free space in megabytes of the filesystem holding path.
func (s *Stack) FreeMB(ctx context.Context, path string) (int, error) {
	res, err := s.T.Run(ctx, transport.Spec{
		Script: `df -Pm "$DK_PATH" 2>/dev/null | awk 'NR==2{print $4}'` + "\n",
		Dir:    s.Root,
		Env:    map[string]string{"DK_PATH": path},
	})
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if convErr != nil {
		return 0, fmt.Errorf("cannot read the free space of %s", path)
	}
	return n, nil
}
