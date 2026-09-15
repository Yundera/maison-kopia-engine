package kopia

import (
	"context"
	"fmt"
)

// Keep is the tiered (grandfather-father-son) retention Maison asks for.
//
// The adapter never decides what to keep. It declares what expiry this storage can
// survive (Caps.RetentionModel) and applies what it is given — those are two of the
// three separate questions, and the third, what the user asked for, is Maison's alone.
type Keep struct {
	Latest, Daily, Weekly, Monthly, Annual int
}

// EnsureRetention installs the retention intent for one source path.
//
// Reapplied on every run rather than only at setup: kopia policies live in the
// repository, so they outlive a Maison reinstall — and a bug can leave a stale one.
//
// Latest has a floor of 2 regardless of what is asked for. A backup writes two
// snapshots against the same source and drops the first only after the second
// succeeds, so keeping fewer than two would let retention evict the consistent
// snapshot in favour of the torn one it was about to replace.
func (e *Engine) EnsureRetention(ctx context.Context, path string, k Keep, userData bool) error {
	if path == "" {
		return fmt.Errorf("ensure-retention needs a source path")
	}
	latest := k.Latest
	if latest < 2 {
		latest = 2
	}
	args := []string{"policy", "set", path,
		"--keep-latest", itoa(latest),
		// Hourly is pinned off: Maison's schedule is daily, so an hourly tier would only
		// ever hold what the daily one already holds.
		"--keep-hourly", "0",
		"--keep-daily", itoa(k.Daily),
		"--keep-weekly", itoa(k.Weekly),
		"--keep-monthly", itoa(k.Monthly),
		"--keep-annual", itoa(k.Annual),
	}
	if userData {
		args = append(args, oneFileSystem...)
	}
	_, err := e.Run(ctx, args...)
	return err
}

// oneFileSystem keeps a snapshot inside its own filesystem.
//
// What it drops is anything MOUNTED underneath the source — a store app that mounts its
// own WebDAV server under the data root being the case in hand. Walking into one is
// wrong three ways: it is a second copy of bytes another source already holds, it
// becomes a --delete-extra target on an in-place restore, and a readdir on it can
// return EIO when the server behind it is down — which kopia counts as FATAL, so one
// app's outage fails the whole box's backup after everything else has been hashed.
//
// Kopia's tri-state policy fields take a STRING, not a boolean flag:
// --one-file-system=true|false|inherit. Verified against kopia 0.23.1.
var oneFileSystem = []string{"--one-file-system", "true"}

// EnsureIgnore replaces the ignore rules on a source path.
//
// TWO INVOCATIONS, and they cannot be merged: kopia applies --clear-ignore AFTER
// --add-ignore regardless of the order they appear on the command line, so combining
// them yields a source with no ignore rules at all — and it fails silently, the backup
// simply succeeding while carrying everything the rules were meant to keep out.
// Verified against kopia 0.23.1.
//
// Clearing first is also what makes this converge: without it, a rule the caller has
// since removed would linger in the repository forever.
func (e *Engine) EnsureIgnore(ctx context.Context, path string, rules []string) error {
	if _, err := e.Run(ctx, "policy", "set", path, "--clear-ignore"); err != nil {
		return err
	}
	if len(rules) == 0 {
		return nil
	}
	args := []string{"policy", "set", path}
	for _, r := range rules {
		args = append(args, "--add-ignore", r)
	}
	_, err := e.Run(ctx, args...)
	return err
}
