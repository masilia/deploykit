package releasedir

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/masilia/deploykit/internal/manifest"
)

// Check is the outcome of one smoke-test request.
type Check struct {
	Path string
	Code int
	OK   bool
	Err  error
}

// Smoke starts the release locally and requests the configured paths before anything is uploaded (deploy.sh step
// "Smoke test of the release"). It keeps a cookie jar and follows at most FollowRedirects redirects, because language
// redirects depend on cookies; a path outside the Expect codes fails the run. The server process is always stopped.
func Smoke(ctx context.Context, dir string, cfg *manifest.Smoke) ([]Check, error) {
	port, err := freePort(cfg.Start.Port.Range[0], cfg.Start.Port.Range[1])
	if err != nil {
		return nil, err
	}
	proc, err := startServer(ctx, dir, cfg, port)
	if err != nil {
		return nil, err
	}
	defer proc.stop()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 20 * time.Second,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > cfg.FollowRedirects {
				return fmt.Errorf("more than %d redirects", cfg.FollowRedirects)
			}
			return nil
		},
	}
	first := "/"
	if len(cfg.Paths) > 0 {
		first = cfg.Paths[0]
	}
	if err := proc.waitReady(ctx, client, base+first); err != nil {
		return nil, err
	}

	paths := append([]string(nil), cfg.Paths...)
	for _, d := range cfg.Discover {
		found, err := discover(ctx, client, base+d.From, d.Match, d.First)
		if err != nil {
			return nil, fmt.Errorf("discover from %s: %w", d.From, err)
		}
		paths = append(paths, found...)
	}

	var checks []Check
	failed := 0
	record := func(path string, headers map[string]string) {
		c := request(ctx, client, base+path, headers)
		c.Path = path
		c.OK = c.Err == nil && contains(cfg.Expect, c.Code)
		if !c.OK {
			failed++
		}
		checks = append(checks, c)
	}
	for _, p := range paths {
		record(p, nil)
	}
	for _, p := range cfg.Ranges {
		record(p, map[string]string{"Range": "bytes=0-1023"})
	}
	if failed > 0 {
		return checks, fmt.Errorf("%d of %d smoke checks failed; server output:\n%s", failed, len(checks), proc.tail(20))
	}
	return checks, nil
}

func request(ctx context.Context, client *http.Client, url string, headers map[string]string) Check {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Check{Err: err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Check{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return Check{Code: resp.StatusCode}
}

func discover(ctx context.Context, client *http.Client, url, pattern string, firstOnly bool) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	matches := re.FindAllString(string(body), -1)
	if firstOnly && len(matches) > 1 {
		matches = matches[:1]
	}
	return matches, nil
}

func contains(codes []int, c int) bool {
	for _, x := range codes {
		if x == c {
			return true
		}
	}
	return false
}

// freePort picks a random free port in [low, high].
func freePort(low, high int) (int, error) {
	for i := 0; i < 50; i++ {
		p := low + rand.IntN(high-low+1) //nolint:gosec // not security sensitive: a throwaway local port
		l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			_ = l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %d-%d", low, high)
}

type server struct {
	cmd  *exec.Cmd
	out  *tailBuffer
	done chan struct{}
}

func startServer(ctx context.Context, dir string, cfg *manifest.Smoke, port int) (*server, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.Start.Command) //nolint:gosec // the command comes from the project's own manifest
	cmd.Dir = dir
	cmd.Env = smokeEnv(cfg, port)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &tailBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", cfg.Start.Command, err)
	}
	s := &server{cmd: cmd, out: out, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.done) }()
	return s, nil
}

func smokeEnv(cfg *manifest.Smoke, port int) []string {
	drop := map[string]bool{"PORT": true}
	for _, k := range cfg.Start.RemoveEnv {
		drop[k] = true
	}
	for k := range cfg.Start.Env {
		drop[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			env = append(env, kv)
		}
	}
	for k, v := range cfg.Start.Env {
		env = append(env, k+"="+v)
	}
	return append(env, "PORT="+strconv.Itoa(port))
}

func (s *server) waitReady(ctx context.Context, client *http.Client, url string) error {
	for i := 0; i < 40; i++ {
		select {
		case <-s.done:
			return fmt.Errorf("the release exited before answering:\n%s", s.tail(20))
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("the release did not answer within 20s:\n%s", s.tail(20))
}

func (s *server) tail(n int) string { return s.out.tail(n) }

func (s *server) stop() {
	if s.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-s.done
	}
}

// tailBuffer keeps the last bytes written, for failure messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 64<<10 {
		t.buf.Next(t.buf.Len() - 32<<10)
	}
	return len(p), nil
}

func (t *tailBuffer) tail(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimRight(t.buf.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
