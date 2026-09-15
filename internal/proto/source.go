package proto

import "strings"

// A source is one thing that gets snapshotted. There are two kinds and they are the
// same kind to the protocol: "app:<name>" and "userdata".
//
// Maison passes the source id AND the path. The adapter never derives one from the
// other, even though the mapping is trivial — an adapter that computed paths would be a
// second definition of the on-disk layout, and the two would disagree at restore time,
// which is the failure that stays invisible until it matters.
const (
	AppPrefix    = "app:"
	UserDataID   = "userdata"
	sourceMaxLen = 256
)

// SplitSource returns the app name for an app source, and whether the id is one.
func SplitSource(id string) (app string, isApp bool) {
	if !strings.HasPrefix(id, AppPrefix) {
		return "", false
	}
	return strings.TrimPrefix(id, AppPrefix), true
}

// ValidSource reports whether an id is one the protocol defines. Anything else is
// rejected rather than guessed at: the id becomes a tag value and a filter.
func ValidSource(id string) bool {
	if id == UserDataID {
		return true
	}
	app, ok := SplitSource(id)
	if !ok || app == "" || len(id) > sourceMaxLen {
		return false
	}
	// The first character must be alphanumeric, matching what a compose project name
	// allows. That is not cosmetic: it is what makes a leading underscore
	// unrepresentable, and therefore what lets an engine reserve an underscore-prefixed
	// value for a source that is not an app without having to police collisions.
	if !alnum(rune(app[0])) {
		return false
	}
	for _, r := range app {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func alnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}
