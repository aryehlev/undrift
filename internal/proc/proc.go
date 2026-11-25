// Package proc runs external processes (agents, verification commands)
// with timeouts, process-group cancellation, a minimal environment and an
// optional sandbox prefix.
package proc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// BaseEnv lists variables always forwarded to child processes. Credentials
// are never included here: they must be named explicitly.
var BaseEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TERM",
	"TMPDIR", "TZ", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"GOPATH", "GOCACHE", "GOMODCACHE", "GOFLAGS", "CARGO_HOME", "RUSTUP_HOME",
	"NPM_CONFIG_CACHE", "PIP_CACHE_DIR", "JAVA_HOME", "CI",
}

// Env builds a filtered environment: BaseEnv plus the named variables
// (supporting a trailing "*" wildcard, e.g. "ANTHROPIC_*") plus extra
// KEY=VALUE pairs.
func Env(names []string, extra ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(k string) {
		if seen[k] {
			return
		}
		if v, ok := os.LookupEnv(k); ok {
			seen[k] = true
			out = append(out, k+"="+v)
		}
	}
	for _, k := range BaseEnv {
		add(k)
	}
	for _, n := range names {
		if strings.HasSuffix(n, "*") {
			prefix := strings.TrimSuffix(n, "*")
			for _, kv := range os.Environ() {
				if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, prefix) {
					add(k)
				}
			}
			continue
		}
		add(n)
	}
	return append(out, extra...)
}

// Spec describes a process to run.
type Spec struct {
	Argv    []string
	Dir     string
	Env     []string
	Stdin   io.Reader
	Timeout time.Duration
	Sandbox []string  // command prefix
	Log     io.Writer // receives combined output as it is produced
	// MaxOutput bounds captured stdout/stderr (tail is kept). 0 = 1 MiB.
	MaxOutput int
}

// Result of a process run.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	TimedOut bool
}

// Shell returns argv that runs a command line through sh.
func Shell(cmd string) []string { return []string{"sh", "-c", cmd} }

// Run executes the process. A non-zero exit status is reported in Result,
// not as an error; err is returned only if the process could not run.
func Run(ctx context.Context, s Spec) (*Result, error) {
	argv := append(append([]string{}, s.Sandbox...), s.Argv...)
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	max := s.MaxOutput
	if max <= 0 {
		max = 1 << 20
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdin = s.Stdin
	stdout := &tail{max: max}
	stderr := &tail{max: max}
	if s.Log != nil {
		cmd.Stdout = io.MultiWriter(stdout, s.Log)
		cmd.Stderr = io.MultiWriter(stderr, s.Log)
	} else {
		cmd.Stdout, cmd.Stderr = stdout, stderr
	}
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	r := &Result{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(start)}
	if ctx.Err() == context.DeadlineExceeded {
		r.TimedOut = true
		r.ExitCode = -1
		return r, nil
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.ExitCode = ee.ExitCode()
	case ctx.Err() != nil:
		r.ExitCode = -1
		return r, ctx.Err()
	default:
		return r, err
	}
	return r, nil
}

// tail keeps the last max bytes written.
type tail struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > 2*t.max {
		b := t.buf.Bytes()
		keep := append([]byte{}, b[len(b)-t.max:]...)
		t.buf.Reset()
		t.buf.Write(keep)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tail) String() string {
	b := t.buf.Bytes()
	if len(b) > t.max {
		b = b[len(b)-t.max:]
		t.truncated = true
	}
	if t.truncated {
		return "...[truncated]...\n" + string(b)
	}
	return string(b)
}

// Tail returns the last n bytes of s.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}
