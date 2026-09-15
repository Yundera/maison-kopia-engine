// Package kopia implements the adapter protocol on top of kopia's CLI.
//
// It runs INSIDE the kopia image, so the engine is a child process rather than a
// container: no docker, no image pull, no networking decision. Everything about how
// this is invoked — the container, its capabilities, its network, its mounts — belongs
// to whoever declared the container, and none of it is restated here.
//
// See docs/kopia-mapping.md for the verb mapping and the hazards this absorbs.
package kopia

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Binary is kopia's own entrypoint inside the image.
const Binary = "/bin/kopia"

// Engine is one configured repository directory.
type Engine struct {
	// Dir is --repo-dir: the host-written directory holding repository.config,
	// repository.password, credentials.env and the caches. The adapter reads it and
	// never writes to it — only the host-side script owns it.
	Dir string

	Out *proto.Emitter

	// Bin is the kopia binary, overridable so tests can point at a stub.
	Bin string
}

func (e *Engine) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	return Binary
}

func (e *Engine) ConfigFile() string      { return filepath.Join(e.Dir, "repository.config") }
func (e *Engine) PasswordFile() string    { return filepath.Join(e.Dir, "repository.password") }
func (e *Engine) CredentialsFile() string { return filepath.Join(e.Dir, "credentials.env") }
func (e *Engine) CacheDir() string        { return filepath.Join(e.Dir, "cache") }
func (e *Engine) LogDir() string          { return filepath.Join(e.Dir, "logs") }

// password is the repository key, read fresh on every invocation.
func (e *Engine) password() (string, error) {
	b, err := os.ReadFile(e.PasswordFile())
	if err != nil {
		if os.IsNotExist(err) {
			return "", proto.ErrNotConfigured
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// credentials are the storage credentials, read fresh on every invocation.
//
// They are NOT in repository.config: kopia persists whatever it was given at connect
// time and offers no flag to stop it (--no-persist-credentials governs the repository
// *password*), so the host script blanks the persisted fields and rotates this file
// alone. Reading it per invocation is what makes a rotation take effect on the next
// backup rather than on the next restart.
//
// An absent file is not an error — a filesystem repository has none.
func (e *Engine) credentials() (map[string]string, error) {
	b, err := os.ReadFile(e.CredentialsFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// First "=" only, so base64 padding in a secret survives.
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// env is the child's environment.
//
// KOPIA_CACHE_DIRECTORY and KOPIA_LOG_DIR are set explicitly because the image BAKES
// them to /app/cache and /app/logs, and those OUTRANK --cache-directory and --log-dir
// on the command line. /app belongs to root, so without this every command fails with
// "unable to create cache directory" before it ever reaches the repository.
func (e *Engine) env(withSecrets bool) ([]string, error) {
	env := append(os.Environ(),
		"KOPIA_CACHE_DIRECTORY="+e.CacheDir(),
		"KOPIA_LOG_DIR="+e.LogDir(),
	)
	if !withSecrets {
		return env, nil
	}
	pw, err := e.password()
	if err != nil {
		return nil, err
	}
	env = append(env, "KOPIA_PASSWORD="+pw)
	creds, err := e.credentials()
	if err != nil {
		return nil, err
	}
	for k, v := range creds {
		env = append(env, k+"="+v)
	}
	return env, nil
}

// Run invokes kopia and returns its stdout.
//
// stdout is captured whole (it is JSON, when there is any) while stderr is scanned
// line by line and turned into progress — kopia redraws progress in place with \r and
// emits no \n until it is done, so anything waiting for whole lines would deliver
// nothing until exit.
func (e *Engine) Run(ctx context.Context, args ...string) ([]byte, error) {
	return e.run(ctx, true, args...)
}

// RunBare is Run without the repository secrets, for a command that must work on a box
// that has no repository yet — `--version`, and the connect path before a password
// exists.
func (e *Engine) RunBare(ctx context.Context, args ...string) ([]byte, error) {
	return e.run(ctx, false, args...)
}

func (e *Engine) run(ctx context.Context, withSecrets bool, args ...string) ([]byte, error) {
	env, err := e.env(withSecrets)
	if err != nil {
		return nil, err
	}
	// --config-file last, so it applies to every subcommand and no caller has to
	// remember it. It is the only globally applied flag.
	args = append(append([]string{}, args...), "--config-file="+e.ConfigFile())

	cmd := exec.CommandContext(ctx, e.bin(), args...)
	cmd.Env = env

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting kopia: %w", err)
	}

	var tail lastLines
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		sc.Split(scanProgress)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			tail.add(line)
			emitLine(e.Out, line)
		}
	}()

	var out bytes.Buffer
	_, copyErr := io.Copy(&out, stdout)
	wg.Wait()
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return out.Bytes(), fmt.Errorf("cancelled: %w", ctx.Err())
	}
	if waitErr != nil {
		return out.Bytes(), fmt.Errorf("kopia %s: %w: %s", args[0], waitErr, tail.String())
	}
	return out.Bytes(), copyErr
}

// lastLines keeps the tail of stderr, which is what makes a failure message useful.
type lastLines struct {
	buf [8]string
	n   int
}

func (l *lastLines) add(s string) { l.buf[l.n%len(l.buf)] = s; l.n++ }

func (l *lastLines) String() string {
	if l.n == 0 {
		return ""
	}
	start, count := 0, l.n
	if count > len(l.buf) {
		start, count = l.n-len(l.buf), len(l.buf)
	}
	parts := make([]string, 0, count)
	for i := start; i < l.n; i++ {
		parts = append(parts, l.buf[i%len(l.buf)])
	}
	return strings.Join(parts, " | ")
}

// scanProgress splits on \r as well as \n. Engines redraw a progress line in place and
// emit no newline until they finish; splitting on newlines alone delivers nothing until
// the command exits.
func scanProgress(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
