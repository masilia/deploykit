package compose

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/transport"
)

// EvaluateRestore judges a restore: the restored database must hold at least ratio of the live tables, and the
// migrations table must have rows. It reproduces the integer thresholds of restore-test.sh.
func EvaluateRestore(liveTables, restoredTables, migrations int, spec *manifest.RestoreTestSpec) error {
	floor := int(math.Floor(float64(liveTables)*spec.Checks.MinTablesRatio + 1e-9))
	if restoredTables < floor {
		return fmt.Errorf("incomplete schema: %d tables restored for %d in production (minimum %d)", restoredTables, liveTables, floor)
	}
	if spec.Checks.MigrationsTable != "" && migrations < spec.Checks.MigrationsMinRows {
		return fmt.Errorf("the restored database has %d migration row(s) in %s, expected at least %d", migrations, spec.Checks.MigrationsTable, spec.Checks.MigrationsMinRows)
	}
	return nil
}

// RestoreTest proves that the newest backup can be restored: checksum, decryption, archives, then a restore into a
// throwaway database that is compared with the live one and dropped. The live database is never touched.
type RestoreTest struct {
	Stack  *Stack
	Data   *manifest.DataSpec
	Backup *manifest.BackupSpec
	Spec   *manifest.RestoreTestSpec
	Env    *envfile.Doc
	// File is the backup to test; empty means the newest one in the backup directory.
	File   string
	Domain string
	PID    int
	Notify func(ctx context.Context, text string)

	file, tmp string
	unlock    func(context.Context) error
	live      int
}

func (rt *RestoreTest) dir() string {
	if path.IsAbs(rt.Backup.Dir) {
		return path.Clean(rt.Backup.Dir)
	}
	return path.Join(rt.Stack.Root, rt.Backup.Dir)
}

func (rt *RestoreTest) notify(ctx context.Context, text string) {
	if rt.Notify != nil {
		rt.Notify(ctx, text)
	}
}

// Run executes the test and always drops the throwaway database and the temporary files.
func (rt *RestoreTest) Run(ctx context.Context, r *runner.Runner) error {
	if rt.Data.Database.Type != "postgres" {
		return fmt.Errorf("the restore test supports postgres, not %q", rt.Data.Database.Type)
	}
	err := r.Do(ctx, rt.steps()...)
	rt.cleanup(ctx)
	if err != nil {
		rt.notify(ctx, fmt.Sprintf(":x: Restore test FAILED: %s: %v (backup %s)", rt.Domain, err, path.Base(rt.file)))
		return err
	}
	rt.notify(ctx, fmt.Sprintf(":white_check_mark: Restore test passed: %s: %s restored into a throwaway database.", rt.Domain, path.Base(rt.file)))
	return nil
}

func (rt *RestoreTest) cleanup(ctx context.Context) {
	c := context.WithoutCancel(ctx)
	if rt.Stack != nil && rt.Spec != nil {
		_, _ = rt.Stack.Script(c, nil, `compose exec -T "$DK_DB" sh -c 'dropdb -U "$POSTGRES_USER" --if-exists "$1"' sh "$DK_NAME"`+"\n",
			map[string]string{"DK_DB": rt.Data.Database.Service, "DK_NAME": rt.Spec.ThrowawayDatabase}, nil)
	}
	if rt.tmp != "" {
		_, _ = rt.Stack.T.Run(c, transport.Spec{Argv: []string{"rm", "-rf", rt.tmp}})
	}
	if rt.unlock != nil {
		_ = rt.unlock(c)
	}
}

func (rt *RestoreTest) psql(ctx context.Context, db, sql string) (int, error) {
	res, err := rt.Stack.Script(ctx, nil, `compose exec -T "$DK_DB" sh -c 'psql -U "$POSTGRES_USER" -d "${1:-$POSTGRES_DB}" -Atc "$2"' sh "$DK_NAME" "$DK_SQL"`+"\n",
		map[string]string{"DK_DB": rt.Data.Database.Service, "DK_NAME": db, "DK_SQL": sql}, nil)
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if convErr != nil {
		return 0, fmt.Errorf("unreadable measure %q", strings.TrimSpace(res.Stdout))
	}
	return n, nil
}

func (rt *RestoreTest) steps() []runner.Step {
	secret := map[string]string{"DK_PASS": rt.Env.Get(rt.Backup.Encryption.PassphraseKey, "")}
	const tablesSQL = "select count(*) from information_schema.tables where table_schema='public'"
	return []runner.Step{
		{
			Name: "select backup",
			Plan: "take the lock and pick the newest backup",
			Run: func(ctx context.Context, out io.Writer) error {
				if secret["DK_PASS"] == "" {
					return fmt.Errorf("%s is missing from .env", rt.Backup.Encryption.PassphraseKey)
				}
				var err error
				if rt.unlock, err = rt.Stack.Lock(ctx, rt.PID); err != nil {
					return err
				}
				rt.file = rt.File
				if rt.file == "" {
					res, err := rt.Stack.T.Run(ctx, transport.Spec{
						Script: `ls -t "$DK_DIR"/"$DK_PREFIX"-*.tar.gpg 2>/dev/null | head -1` + "\n",
						Env:    map[string]string{"DK_DIR": rt.dir(), "DK_PREFIX": rt.Backup.NamePrefix},
					})
					if err != nil {
						return err
					}
					rt.file = strings.TrimSpace(res.Stdout)
				}
				if rt.file == "" {
					return fmt.Errorf("no backup found in %s", rt.dir())
				}
				_, _ = fmt.Fprintf(out, "backup under test: %s\n", path.Base(rt.file))
				res, err := rt.Stack.T.Run(ctx, transport.Spec{Argv: []string{"mktemp", "-d"}})
				if err != nil {
					return err
				}
				rt.tmp = strings.TrimSpace(res.Stdout)
				return nil
			},
		},
		{
			Name: "checksum",
			Plan: "verify the sha256 file, when there is one",
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := rt.Stack.T.Run(ctx, transport.Spec{
					Script: `if [ -f "$DK_FILE.sha256" ]; then cd "$(dirname "$DK_FILE")" && sha256sum -c "$(basename "$DK_FILE").sha256" >/dev/null 2>&1 || { echo "invalid sha256: the file is corrupted" >&2; exit 1; }; fi` + "\n",
					Env:    map[string]string{"DK_FILE": rt.file},
				})
				return err
			},
		},
		{
			Name: "decrypt",
			Plan: "decrypt and extract the archive, then check its contents",
			Run: func(ctx context.Context, out io.Writer) error {
				script := `set -e
printf '%s' "$DK_PASS" | gpg --batch --pinentry-mode loopback --passphrase-fd 0 --decrypt "$DK_FILE" 2>/dev/null | tar -xzf - -C "$DK_TMP" || { echo "cannot decrypt or extract (was the passphrase changed since?)" >&2; exit 1; }
[ -s "$DK_TMP/db.dump" ] || { echo "db.dump is missing or empty in the archive" >&2; exit 1; }
`
				for _, v := range rt.Data.Volumes {
					script += `tar -tzf "$DK_TMP/` + v + `.tar.gz" >/dev/null 2>&1 || { echo "the ` + v + ` archive is unreadable" >&2; exit 1; }` + "\n"
				}
				_, err := rt.Stack.T.Run(ctx, transport.Spec{Script: script, Env: map[string]string{"DK_FILE": rt.file, "DK_TMP": rt.tmp}, Secrets: secret})
				return err
			},
		},
		{
			Name: "restore",
			Plan: "restore the dump into the throwaway database",
			Run: func(ctx context.Context, out io.Writer) error {
				env := map[string]string{"DK_DB": rt.Data.Database.Service, "DK_NAME": rt.Spec.ThrowawayDatabase, "DK_TMP": rt.tmp}
				if _, err := rt.Stack.Script(ctx, out, `compose exec -T "$DK_DB" sh -c 'dropdb -U "$POSTGRES_USER" --if-exists "$1" && createdb -U "$POSTGRES_USER" "$1"' sh "$DK_NAME"`+"\n", env, nil); err != nil {
					return fmt.Errorf("cannot create the throwaway database: %w", err)
				}
				if _, err := rt.Stack.Script(ctx, out, `compose exec -T "$DK_DB" sh -c 'pg_restore -U "$POSTGRES_USER" -d "$1" --no-owner --exit-on-error' sh "$DK_NAME" < "$DK_TMP/db.dump"`+"\n", env, nil); err != nil {
					return fmt.Errorf("pg_restore failed: %w", err)
				}
				return nil
			},
		},
		{
			Name: "compare",
			Plan: "compare the restored schema with the live one",
			Run: func(ctx context.Context, out io.Writer) error {
				var err error
				if rt.live, err = rt.psql(ctx, "", tablesSQL); err != nil {
					return err
				}
				restored, err := rt.psql(ctx, rt.Spec.ThrowawayDatabase, tablesSQL)
				if err != nil {
					return err
				}
				migrations := 0
				if rt.Spec.Checks.MigrationsTable != "" {
					if migrations, err = rt.psql(ctx, rt.Spec.ThrowawayDatabase, "select count(*) from "+rt.Spec.Checks.MigrationsTable); err != nil {
						return err
					}
				}
				if err := EvaluateRestore(rt.live, restored, migrations, rt.Spec); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "schema: %d tables (production: %d), %d migrations\n", restored, rt.live, migrations)
				return nil
			},
		},
	}
}
