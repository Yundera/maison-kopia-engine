package kopia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Files `init` works from. The host writes the marker when it finds the storage empty
// and no key on the box; Maison writes the initial key the owner chose.
func (e *Engine) NeedsSecretFile() string { return filepath.Join(e.Dir, "needs-secret") }
func (e *Engine) InitialFile() string     { return filepath.Join(e.Dir, "repository.password.initial") }

// needsSecret reports the host's needs-secret marker. As with needs-recovery, its
// contents are the host's diagnostics and presence is the whole signal.
func (e *Engine) needsSecret() bool {
	_, err := os.Stat(e.NeedsSecretFile())
	return err == nil
}

// isNotInitialized recognises kopia finding no repository in the storage. Verified
// against kopia 0.23.1, whose `repository connect` against an empty location fails with
//
//	error connecting to repository: repository not initialized in the provided storage
//
// for a filesystem path and an S3 prefix alike, and leaves no configuration behind. A
// location kopia cannot reach at all fails differently ("can't connect to storage"), so
// matching this sentence alone never reads an outage as empty storage.
func isNotInitialized(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "repository not initialized")
}

// storageFromFiles is the arguments for a connect or create on a box without a
// repository yet, read from connect.json. Storage that needs credentials (anything but
// a filesystem path) also needs credentials.env to be there. Either missing is
// ErrNotConfigured: the host has not written what this needs yet.
func (e *Engine) storageFromFiles() ([]string, error) {
	cb, err := os.ReadFile(e.ConnectFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no connect.json: %w", proto.ErrNotConfigured)
		}
		return nil, err
	}
	o, err := connectOpts(cb)
	if err != nil {
		return nil, err
	}
	args, err := storageArgs(o)
	if err != nil {
		return nil, fmt.Errorf("connect.json: %v: %w", err, proto.ErrNotConfigured)
	}
	if o.Path == "" {
		if _, err := os.Stat(e.CredentialsFile()); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("no credentials.env: %w", proto.ErrNotConfigured)
			}
			return nil, err
		}
	}
	return args, nil
}

// Probe says whether the storage already holds a repository, without a key and
// without changing anything.
//
// It connects with a random key that cannot be the repository's, to a configuration
// file and cache in a temporary directory that is removed afterwards, and reads kopia's
// refusal:
//
//   - "repository not initialized" → empty;
//   - "invalid repository password" → existing — kopia only gets that far when the
//     format blob is there;
//   - a connect that SUCCEEDS → existing. It cannot happen with a random key, and if it
//     ever did the storage plainly is not empty.
//
// Anything else is an ordinary failure. "Empty" leads the caller to create a repository,
// so it is only ever answered on kopia's own word for it, never on doubt.
func (e *Engine) Probe(ctx context.Context) (proto.Probed, error) {
	var res proto.Probed

	args, err := e.storageFromFiles()
	if err != nil {
		return res, err
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return res, fmt.Errorf("generating a probe key: %w", err)
	}
	tmp, err := os.MkdirTemp("", "maison-probe-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	_, err = e.runThrowaway(ctx, tmp, hex.EncodeToString(key), append([]string{"repository", "connect"}, args...)...)
	switch {
	case err == nil, isWrongPassword(err):
		res.Storage = proto.StorageExisting
	case ctx.Err() != nil:
		return res, err
	case isNotInitialized(err):
		res.Storage = proto.StorageEmpty
	default:
		return res, err
	}
	return res, nil
}

// Init creates the repository with the key the owner chose, on a box whose storage is
// empty and which holds no key yet — the needs-secret state.
//
// It is `connect` with the connect half removed and the password supplied by the
// caller, the mirror image of Recover. The key is read from repository.password.initial,
// which is removed on every path; on success it has become repository.password.
//
// Create ONLY: storage that already holds a repository is ErrRepositoryExists, and the
// way out of that is Recover with the old key, never a second repository.
func (e *Engine) Init(ctx context.Context) (proto.Recovered, error) {
	var res proto.Recovered

	b, err := os.ReadFile(e.InitialFile())
	if err != nil {
		if os.IsNotExist(err) {
			return res, proto.ErrNotConfigured
		}
		return res, err
	}
	// Whatever happens next, a key the owner typed does not stay on disk under a name
	// nothing else owns. On success it has been renamed and this is a no-op.
	defer os.Remove(e.InitialFile())

	key := strings.TrimSpace(string(b))
	if key == "" {
		return res, errors.New("an empty key: nothing to create the repository with")
	}

	// A box with a configuration was already initialised; creating again would rewrite
	// it — the one thing that must never happen.
	if _, err := os.Stat(e.ConfigFile()); err == nil {
		return res, errors.New("already connected: this box has a repository configuration")
	} else if !os.IsNotExist(err) {
		return res, err
	}

	args, err := e.storageFromFiles()
	if err != nil {
		return res, err
	}

	out, err := e.runWithPassword(ctx, key, append([]string{"repository", "create"}, args...)...)
	if err != nil {
		if strings.Contains(strings.ToLower(string(out)+err.Error()), "found existing data") {
			return res, ErrRepositoryExists
		}
		return res, err
	}

	// From here the repository exists and is open. As in Recover, a failure before the
	// key is promoted would leave a configuration with no password beside it, so the
	// connect is undone; the storage then holds a repository only the owner's key
	// opens, and the next attempt answers 13 and goes through `recover`.
	if err := e.blankPersistedCredentials(); err != nil {
		e.undoConnect()
		return res, err
	}
	if err := e.promote(key); err != nil {
		e.undoConnect()
		return res, err
	}
	// The create succeeding proves the storage held no repository, so a needs-recovery
	// marker is as stale as the needs-secret one — and Status would report it first.
	for _, m := range []string{e.NeedsSecretFile(), e.MarkerFile()} {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			e.Out.Log("warn", "removing "+filepath.Base(m)+": "+err.Error())
		}
	}
	return res, nil
}
