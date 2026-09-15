package kopia

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// The progress parser is the file a second adapter reimplements, so what it can and
// cannot read is worth pinning. An unreadable line still carries its message: that is
// the only thing to show while kopia is estimating the tree.
func TestProgressParsingIsTolerant(t *testing.T) {
	for _, tc := range []struct {
		line string
		want float64
	}{
		{"* 0 hashing, 12 hashed (1.2 GB), 0 cached (0 B), uploaded 1.2 GB, estimated 2.4 GB (50.0%) 0s left", 50.0},
		{"| 3 hashing, 0 hashed (0 B), 0 cached (0 B), uploaded 0 B, estimating...", proto.PctUnknown},
		{"Snapshotting pcs@host:/DATA/AppData", proto.PctUnknown},
	} {
		var buf bytes.Buffer
		emitLine(proto.NewEmitter(&buf), tc.line)

		var got proto.Line
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("%q: %v", tc.line, err)
		}
		if got.Pct == nil || *got.Pct != tc.want {
			t.Errorf("%q: pct = %v, want %v", tc.line, got.Pct, tc.want)
		}
		if got.Message != tc.line {
			t.Errorf("%q: message = %q, want the raw line", tc.line, got.Message)
		}
	}
}

// done is hashed+cached against kopia's own estimate. "uploaded" is deliberately
// excluded: after dedup and compression it is a fraction of what was read, and a bar
// built on it disagrees with the percentage printed on the same line.
func TestProgressCarriesTheByteCounts(t *testing.T) {
	for _, tc := range []struct {
		line              string
		wantDone, wantTot int64
	}{
		{"* 0 hashing, 12 hashed (1.2 GB), 3 cached (800 MB), uploaded 500 MB, estimated 2.4 GB (50.0%)", 2_000_000_000, 2_400_000_000},
		{"Processed 1226 (1.1 GB) of 2000 (5.5 GB).", 1_100_000_000, 5_500_000_000},
		{"nothing measurable here", 0, 0},
	} {
		done, total := bytesOf(tc.line)
		if done != tc.wantDone || total != tc.wantTot {
			t.Errorf("bytesOf(%q) = %d/%d, want %d/%d", tc.line, done, total, tc.wantDone, tc.wantTot)
		}
	}
}

// Both unit families are accepted, because --units is a kopia setting and guessing
// wrong is a silent 7% error in every rate and ETA Maison derives.
func TestBothUnitFamiliesAreRead(t *testing.T) {
	if got := parseSize("1 KB"); got != 1000 {
		t.Errorf("1 KB = %d, want 1000", got)
	}
	if got := parseSize("1 KiB"); got != 1024 {
		t.Errorf("1 KiB = %d, want 1024", got)
	}
}

// kopia's --endpoint is a host[:port], NOT a URL — given one it fails with "Endpoint url
// is not valid". The credential API returns a URL because that contract is
// engine-independent, so the conversion lives here.
func TestEndpointIsReducedToAHost(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		wantHost string
		wantTLS  bool
	}{
		{"https://s3.fr-par.scw.cloud", "s3.fr-par.scw.cloud", true},
		{"https://s3.example.com/", "s3.example.com", true},
		{"http://minio:9000", "minio:9000", false},
		{"s3.example.com:443", "s3.example.com:443", true},
	} {
		host, tls, err := endpointHost(tc.raw)
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if host != tc.wantHost || tls != tc.wantTLS {
			t.Errorf("endpointHost(%q) = %q/%v, want %q/%v", tc.raw, host, tls, tc.wantHost, tc.wantTLS)
		}
	}
}

// Identity must be explicit. Deriving it here is how one repository ends up holding two
// user@host lineages that never see each other — invisible until a restore comes back
// empty.
func TestConnectRefusesToInventAnIdentity(t *testing.T) {
	if _, err := storageArgs(ConnectOpts{Bucket: "b", Endpoint: "https://s3"}); err == nil {
		t.Error("storageArgs accepted a connect with no identity")
	}
}

// The source id is the protocol's vocabulary; the tag is kopia's. They must map both
// ways, and the reserved user-data value must not be reachable as an app name.
func TestSourceTagRoundTrip(t *testing.T) {
	for _, id := range []string{"app:jellyfin", proto.UserDataID} {
		tag, err := sourceTag(id)
		if err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		if got := sourceIDFor(tag); got != id {
			t.Errorf("%q -> %q -> %q", id, tag, got)
		}
	}
	if _, err := sourceTag("jellyfin"); err == nil {
		t.Error("a bare app name was accepted as a source id")
	}
	// The reserved tag has a leading underscore, which ValidSource rejects in an app
	// name — so the guard makes the reservation rather than this having to police it.
	if proto.ValidSource(proto.AppPrefix + userDataTag) {
		t.Error("the reserved user-data tag is reachable as an app name")
	}
}

// "restore just Documents" must never become "restore whatever path you name".
func TestSelectEntriesRefusesWhatTheSnapshotDoesNotHold(t *testing.T) {
	have := []proto.Entry{{Name: "Documents", Dir: true}, {Name: "notes.txt"}}
	got, err := selectEntries(have, []string{"Documents"})
	if err != nil || len(got) != 1 || !got[0].Dir {
		t.Fatalf("selectEntries = %v, %v", got, err)
	}
	if _, err := selectEntries(have, []string{"../../etc"}); err == nil {
		t.Error("selectEntries accepted a name the snapshot does not hold")
	}
}
