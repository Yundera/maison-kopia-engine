package kopia

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// NextFile is where a replacement key waits while `change-secret` installs it. Maison
// writes it; the adapter is the only thing that removes it.
func (e *Engine) NextFile() string { return filepath.Join(e.Dir, "repository.password.next") }

func (e *Engine) secretLockFile() string { return filepath.Join(e.Dir, ".secret.lock") }

// lockSecret serialises everything that may touch repository.password.next across
// processes. Maison runs one adapter process per verb, so a `status` probe can land in
// the middle of a `change-secret` — and a probe that settled .next as stale while the
// change was still running would delete the only copy of the key kopia is about to
// require.
//
// wait=false is for SettleNext: a change in progress settles its own outcome, so a
// probe that cannot take the lock simply leaves it alone.
func (e *Engine) lockSecret(wait bool) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(e.secretLockFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, true, nil
}

// settleOutcome is what SettleNext found.
type settleOutcome int

const (
	settleNothing    settleOutcome = iota // no .next on disk
	settleStale                           // the current key still opens; .next was dropped
	settlePromoted                        // only .next opens; it is now repository.password
	settleUnresolved                      // neither answer is certain yet; .next is kept
)

// SettleNext resolves a repository.password.next left behind by a change that did not
// finish — the adapter killed, or Maison's deadline, between kopia rewriting the
// repository's key and the file being replaced. In that window repository.password no
// longer opens anything, and .next is the only copy of the key that does.
//
// Which of the two opens the repository is asked of kopia, never inferred:
//
//   - the current key opens it → the change never happened; .next is stale and goes;
//   - the current key is refused and .next opens it → the change happened; .next is
//     promoted;
//   - anything else (unreachable storage, both refused) → nothing is touched, and the
//     next probe asks again. Deleting either key on a guess is the one unrecoverable move.
//
// Cheap when there is nothing to settle: one stat.
func (e *Engine) SettleNext(ctx context.Context) {
	if _, err := os.Stat(e.NextFile()); err != nil {
		return
	}
	unlock, ok, err := e.lockSecret(false)
	if err != nil {
		e.Out.Log("warn", "settling a password change: "+err.Error())
		return
	}
	if !ok {
		return // a change is running and will settle its own outcome
	}
	defer unlock()
	switch e.settleLocked(ctx) {
	case settlePromoted:
		e.Out.Log("info", "an interrupted password change was completed")
	case settleUnresolved:
		e.Out.Log("warn", "an interrupted password change could not be settled yet; "+filepath.Base(e.NextFile())+" is kept")
	}
}

func (e *Engine) settleLocked(ctx context.Context) settleOutcome {
	b, err := os.ReadFile(e.NextFile())
	if err != nil {
		if os.IsNotExist(err) {
			return settleNothing
		}
		return settleUnresolved
	}
	next := strings.TrimSpace(string(b))

	_, curErr := e.Run(ctx, "repository", "status")
	if curErr == nil {
		e.removeNext()
		return settleStale
	}
	if !isWrongPassword(curErr) || next == "" {
		return settleUnresolved
	}
	if _, err := e.runWithPassword(ctx, next, "repository", "status"); err != nil {
		return settleUnresolved
	}
	if err := e.promote(next); err != nil {
		e.Out.Log("warn", "promoting the new repository password: "+err.Error())
		return settleUnresolved
	}
	e.removeNext()
	return settlePromoted
}

func (e *Engine) removeNext() {
	if err := os.Remove(e.NextFile()); err != nil && !os.IsNotExist(err) {
		e.Out.Log("warn", "removing "+filepath.Base(e.NextFile())+": "+err.Error())
	}
}

// ChangeSecret replaces the repository password with the key in repository.password.next.
//
// kopia re-wraps the repository's master key under the new password: nothing is
// re-encrypted, every snapshot stays readable, and the old password stops opening the
// repository at once. That is all it does — anyone who already copied the repository's
// format blob together with the old password keeps access. See docs/kopia-mapping.md.
//
// .next is removed whenever the outcome is known. When it is not — the deadline hit
// while kopia was writing — it stays for SettleNext, because it may by then be the
// only key that opens the repository.
func (e *Engine) ChangeSecret(ctx context.Context) error {
	unlock, _, err := e.lockSecret(true)
	if err != nil {
		return err
	}
	defer unlock()

	b, err := os.ReadFile(e.NextFile())
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no %s: %w", filepath.Base(e.NextFile()), proto.ErrNotConfigured)
		}
		return err
	}
	next := strings.TrimSpace(string(b))
	if next == "" {
		e.removeNext()
		return fmt.Errorf("an empty key: %w", proto.ErrWrongPassword)
	}
	// A box waiting for recovery has no key to change from, and one with no
	// configuration has nothing to change.
	if e.needsRecovery() {
		e.removeNext()
		return fmt.Errorf("the repository is waiting for recovery: %w", proto.ErrNotConfigured)
	}
	if _, err := e.readConfig(); err != nil {
		e.removeNext()
		return err
	}
	current, err := e.password()
	if err != nil {
		e.removeNext()
		return err
	}
	if next == current {
		e.removeNext()
		return nil
	}

	if _, err := e.runChangePassword(ctx, next); err != nil {
		// Whether kopia got as far as rewriting the key is not knowable from the error,
		// so it is asked: the same question SettleNext answers.
		switch e.settleLocked(context.WithoutCancel(ctx)) {
		case settlePromoted:
			// A previous, interrupted change to this same key had already gone through.
			return nil
		case settleStale:
			if isWrongPassword(err) {
				return proto.ErrWrongPassword
			}
			return err
		default:
			if isWrongPassword(err) {
				// The current key does not open the repository and neither does the new
				// one: nothing was changed, and keeping .next would only confuse the next
				// settle.
				e.removeNext()
				return proto.ErrWrongPassword
			}
			return err
		}
	}

	if err := e.promote(next); err != nil {
		// kopia now wants the new key and the file still holds the old one. .next stays:
		// SettleNext retries the promote on the next probe.
		return fmt.Errorf("the repository password was changed but could not be installed (retried on the next status): %w", err)
	}
	e.removeNext()
	return nil
}
