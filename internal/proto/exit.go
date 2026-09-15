package proto

import "errors"

// Exit codes. 10 and 11 must stay distinguishable from 1: collapsing 10 turns an
// unprovisioned box into a red page, and collapsing 11 turns a capability gap into a
// fault.
const (
	ExitOK            = 0
	ExitError         = 1
	ExitNotConfigured = 10
	ExitNotSupported  = 11
	ExitNotWritable   = 12
)

// ErrNotConfigured is the normal state of a box whose host side has not connected a
// repository yet. It degrades; it does not fail.
var ErrNotConfigured = errors.New("no repository configured")

// ErrNotSupported is an operation this engine cannot perform. It must agree with what
// `capabilities` reports.
var ErrNotSupported = errors.New("operation not supported by this engine")

// ErrNotWritable is a repository that is reachable but refuses writes — a storage space
// suspended for quota. Reads and restores continue to work.
var ErrNotWritable = errors.New("the repository is not accepting writes")

// CodeFor maps an error to the exit code that describes it.
func CodeFor(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrNotConfigured):
		return ExitNotConfigured
	case errors.Is(err, ErrNotSupported):
		return ExitNotSupported
	case errors.Is(err, ErrNotWritable):
		return ExitNotWritable
	default:
		return ExitError
	}
}
