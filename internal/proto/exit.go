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

	// ExitRepositoryExists says the storage already holds a repository this box has no
	// password for — a rebuilt box, or a prefix collision.
	//
	// It is its own code because the only safe response is to STOP. Initialising a
	// second repository under the same prefix would strand the first one's snapshots
	// behind a key nobody has, and the caller cannot tell that from an ordinary
	// connect failure by reading an error string.
	ExitRepositoryExists = 13
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

// ErrRepositoryExists is storage that already holds a repository this box cannot open.
// See ExitRepositoryExists.
var ErrRepositoryExists = errors.New("the storage already holds a repository and this box has no password for it")

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
	case errors.Is(err, ErrRepositoryExists):
		return ExitRepositoryExists
	default:
		return ExitError
	}
}
