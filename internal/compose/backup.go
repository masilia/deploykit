package compose

import (
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/masilia/deploykit/internal/envfile"
	"github.com/masilia/deploykit/internal/manifest"
	"github.com/masilia/deploykit/internal/runner"
	"github.com/masilia/deploykit/internal/tmpl"
	"github.com/masilia/deploykit/internal/transport"
)

// helperImage runs the volume archives; pinned like the legacy backup.sh.
const helperImage = "alpine:3.20"

// BackupFile is a backup file as listed on disk.
type BackupFile struct {
	Name    string
	ModTime time.Time
}

// Name builds the backup base name: <prefix>-<YYYYMMDD-HHMMSS>[-monthly]. A backup made on the monthly day is kept
// longer.
func BackupName(prefix string, now time.Time, monthlyOnDay int) string {
	name := prefix + "-" + now.Format("20060102-150405")
	if now.Day() == monthlyOnDay {
		name += "-monthly"
	}
	return name
}

// RetentionDeletes returns the files to delete: daily backups older than dailyDays, monthly ones older than
// monthlyDays. It reproduces `find -name 'P-*' ! -name '*-monthly.*' -mtime +7` and the monthly twin: -mtime +N means
// the age in whole days is strictly greater than N. The .sha256 companions follow their backup.
func RetentionDeletes(files []BackupFile, now time.Time, prefix string, dailyDays, monthlyDays int) []string {
	var del []string
	for _, f := range files {
		if !strings.HasPrefix(f.Name, prefix+"-") {
			continue
		}
		limit := dailyDays
		if ok, _ := path.Match(prefix+"-*-monthly.*", f.Name); ok {
			limit = monthlyDays
		}
		if int(now.Sub(f.ModTime).Hours()/24) > limit {
			del = append(del, f.Name)
		}
	}
	sort.Strings(del)
	return del
}

// Backup makes one verified, encrypted backup: dump, volumes and .env, gpg AES-256, verified before it is kept.
type Backup struct {
	Stack *Stack
	Data  *manifest.DataSpec
	Cfg   *manifest.BackupSpec
	Env   *envfile.Doc
	Now   func() time.Time

	name, tmp, dir string
}

// Dir is the backup directory, relative to the deployment root unless absolute.
func (b *Backup) Dir() string {
	if path.IsAbs(b.Cfg.Dir) {
		return path.Clean(b.Cfg.Dir)
	}
	return path.Join(b.Stack.Root, b.Cfg.Dir)
}

// Path is the final file, valid after the backup ran.
func (b *Backup) Path() string { return b.dir + "/" + b.name + ".tar.gpg" }

func (b *Backup) passphrase() string { return b.Env.Get(b.Cfg.Encryption.PassphraseKey, "") }

// Steps returns the backup steps. The caller holds the operations lock.
func (b *Backup) Steps() []runner.Step {
	if b.Now == nil {
		b.Now = time.Now
	}
	secret := map[string]string{"DK_PASS": b.passphrase()}
	return []runner.Step{
		{
			Name: "backup: prepare",
			Plan: "check gpg, the passphrase and the free space",
			Run: func(ctx context.Context, out io.Writer) error {
				if b.Cfg.Encryption.Type != "gpg" {
					return fmt.Errorf("backup.encryption.type must be gpg")
				}
				if b.passphrase() == "" {
					return fmt.Errorf("%s is missing from .env", b.Cfg.Encryption.PassphraseKey)
				}
				if _, err := b.Stack.T.Run(ctx, transport.Spec{Script: "command -v gpg >/dev/null || { echo 'gpg not found: apt install gnupg' >&2; exit 1; }\n"}); err != nil {
					return err
				}
				b.dir = b.Dir()
				b.name = BackupName(b.Cfg.NamePrefix, b.Now(), b.Cfg.Retention.MonthlyOnDay)
				if _, err := b.Stack.T.Run(ctx, transport.Spec{Argv: []string{"mkdir", "-p", b.dir}}); err != nil {
					return err
				}
				min := 500
				if v, err := tmpl.Render(b.Cfg.MinFreeMB, EnvVars(b.Env), false); err == nil {
					if n, convErr := strconv.Atoi(v); convErr == nil {
						min = n
					}
				}
				free, err := b.Stack.FreeMB(ctx, b.dir)
				if err != nil {
					return err
				}
				if free < min {
					return fmt.Errorf("not enough disk space for the backup: %d MB free on %s (minimum %d MB)", free, b.dir, min)
				}
				res, err := b.Stack.T.Run(ctx, transport.Spec{Argv: []string{"mktemp", "-d"}})
				if err != nil {
					return err
				}
				b.tmp = strings.TrimSpace(res.Stdout)
				_, _ = fmt.Fprintf(out, "backup %s\n", b.name)
				return nil
			},
		},
		{
			Name: "backup: database dump",
			Plan: "dump the database and check that the dump is readable",
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := b.Stack.Script(ctx, out, `set -e
compose exec -T "$DK_DB" sh -c "$DK_DUMP" > "$DK_TMP/db.dump"
[ -s "$DK_TMP/db.dump" ] || { echo "the dump is empty" >&2; exit 1; }
compose exec -T "$DK_DB" sh -c "$DK_VERIFY" < "$DK_TMP/db.dump" >/dev/null 2>&1 || { echo "the dump is unreadable ($DK_VERIFY failed): backup abandoned" >&2; exit 1; }
`, map[string]string{"DK_DB": b.Data.Database.Service, "DK_DUMP": b.Data.Database.Dump, "DK_VERIFY": b.Data.Database.Verify, "DK_TMP": b.tmp}, nil)
				return err
			},
		},
		{
			Name: "backup: volumes",
			Plan: "archive the volumes",
			Run: func(ctx context.Context, out io.Writer) error {
				for _, v := range b.Data.Volumes {
					if _, err := b.Stack.T.Run(ctx, transport.Spec{Script: `set -e
docker volume inspect "$DK_VOL" >/dev/null 2>&1 || { echo "volume $DK_VOL not found (is the stack started?)" >&2; exit 1; }
docker run --rm -v "$DK_VOL:/data:ro" -v "$DK_TMP:/backup" ` + helperImage + ` tar -C /data -czf "/backup/$DK_NAME.tar.gz" .
`, Env: map[string]string{"DK_VOL": b.Stack.Project + "_" + v, "DK_TMP": b.tmp, "DK_NAME": v}, Out: out}); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			Name: "backup: bundle and encrypt",
			Plan: "bundle the files and encrypt them with gpg AES-256",
			Run: func(ctx context.Context, out io.Writer) error {
				files := []string{"db.dump"}
				for _, v := range b.Data.Volumes {
					files = append(files, v+".tar.gz")
				}
				for _, f := range b.Data.IncludeFiles {
					if _, err := b.Stack.T.Run(ctx, transport.Spec{Argv: []string{"cp", f, b.tmp + "/" + path.Base(f)}, Dir: b.Stack.Root}); err != nil {
						return err
					}
					files = append(files, path.Base(f))
				}
				cipher := b.Cfg.Encryption.Cipher
				if cipher == "" {
					cipher = "AES256"
				}
				_, err := b.Stack.T.Run(ctx, transport.Spec{
					Script: `set -e
tar -C "$DK_TMP" -czf "$DK_TMP/bundle.tar.gz" ` + transport.QuoteArgs(files) + `
printf '%s' "$DK_PASS" | gpg --batch --yes --pinentry-mode loopback --passphrase-fd 0 --symmetric --cipher-algo "$DK_CIPHER" --output "$DK_OUT" "$DK_TMP/bundle.tar.gz"
cd "$DK_DIR" && sha256sum "$DK_FILE" > "$DK_FILE.sha256"
`,
					Env:     map[string]string{"DK_TMP": b.tmp, "DK_CIPHER": cipher, "DK_OUT": b.Path(), "DK_DIR": b.dir, "DK_FILE": b.name + ".tar.gpg"},
					Secrets: secret, Out: out,
				})
				return err
			},
		},
		{
			Name: "backup: verify",
			Plan: "decrypt and list the archive; delete it if it is unreadable",
			Run: func(ctx context.Context, out io.Writer) error {
				_, err := b.Stack.T.Run(ctx, transport.Spec{
					Script: `if printf '%s' "$DK_PASS" | gpg --batch --pinentry-mode loopback --passphrase-fd 0 --decrypt "$DK_OUT" 2>/dev/null | tar -tzf - >/dev/null; then exit 0; fi
rm -f "$DK_OUT" "$DK_OUT.sha256"
echo "the backup is unreadable after encryption: deleted, nothing was kept" >&2
exit 1
`,
					Env: map[string]string{"DK_OUT": b.Path()}, Secrets: secret,
				})
				return err
			},
		},
		{
			Name: "backup: retention",
			Plan: "delete expired backups",
			Run: func(ctx context.Context, out io.Writer) error {
				res, err := b.Stack.T.Run(ctx, transport.Spec{
					Argv: []string{"find", b.dir, "-maxdepth", "1", "-name", b.Cfg.NamePrefix + "-*", "-printf", `%f\t%T@\n`},
				})
				if err != nil {
					return err
				}
				del := RetentionDeletes(parseListing(res.Stdout), b.Now(), b.Cfg.NamePrefix, b.Cfg.Retention.DailyDays, b.Cfg.Retention.MonthlyDays)
				if len(del) == 0 {
					return nil
				}
				argv := []string{"rm", "-f", "--"}
				for _, f := range del {
					argv = append(argv, b.dir+"/"+f)
				}
				_, _ = fmt.Fprintf(out, "removing %d expired file(s)\n", len(del))
				_, err = b.Stack.T.Run(ctx, transport.Spec{Argv: argv})
				return err
			},
		},
		{
			Name:     "backup: off-server copy",
			Plan:     "rsync the backup to the off-site target, if configured",
			Optional: true,
			Run: func(ctx context.Context, out io.Writer) error {
				target := renderOptional(b.Cfg.Offsite.RsyncTarget, EnvVars(b.Env))
				if target == "" {
					return nil // no off-site target configured
				}
				_, err := b.Stack.T.Run(ctx, transport.Spec{Argv: []string{"rsync", "-a", b.Path(), b.Path() + ".sha256", target}, Out: out})
				if err != nil {
					return fmt.Errorf("the local backup is kept, but the off-site copy failed: %w", err)
				}
				return nil
			},
		},
	}
}

// Run executes the steps and always removes the temporary directory, whether the backup succeeded or not.
func (b *Backup) Run(ctx context.Context, r *runner.Runner) error {
	defer b.Cleanup(ctx)
	return r.Do(ctx, b.Steps()...)
}

// Cleanup removes the temporary directory. It is safe to call twice and ignores a cancelled context.
func (b *Backup) Cleanup(ctx context.Context) {
	if b.tmp == "" {
		return
	}
	_, _ = b.Stack.T.Run(context.WithoutCancel(ctx), transport.Spec{Argv: []string{"rm", "-rf", b.tmp}})
}

// parseListing reads `name<TAB>epoch` lines produced by find -printf.
func parseListing(s string) []BackupFile {
	var out []BackupFile
	for _, line := range strings.Split(s, "\n") {
		name, epoch, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		sec, err := strconv.ParseFloat(strings.TrimSpace(epoch), 64)
		if err != nil {
			continue
		}
		out = append(out, BackupFile{Name: name, ModTime: time.Unix(int64(sec), 0)})
	}
	return out
}

// renderOptional renders a template that may legitimately be unset: an undefined variable yields "" instead of the
// unrendered text. (Treating the failed render as a value once made rsync copy a backup into a local directory named
// "{{ env.BACKUP_RSYNC_TARGET }}".)
func renderOptional(s string, vars tmpl.Vars) string {
	out, err := tmpl.Render(s, vars, false)
	if err != nil || strings.Contains(out, "{{") {
		return ""
	}
	return out
}
