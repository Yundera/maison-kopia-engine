package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Every line on stdout must be one JSON object. Maison parses all of them, so a line
// that is not JSON is a protocol violation rather than noise.
func TestEveryLineIsOneJSONObject(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.Progress("estimating", PctUnknown, 0, 0)
	e.Progress("uploading", 42.5, 4194304, 9871232)
	e.Log("warn", "cache rebuilt")
	if err := e.Result(Status{Configured: true, Connected: true}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4: %q", len(lines), buf.String())
	}
	for i, l := range lines {
		var got Line
		if err := json.Unmarshal([]byte(l), &got); err != nil {
			t.Errorf("line %d is not a JSON object: %v (%q)", i, err, l)
		}
	}
}

// Unknown progress has to survive the round trip as unknown. Encoded as a plain zero it
// would render as "0%% done" forever, which is worse than an indeterminate bar.
func TestUnknownProgressSurvivesEncoding(t *testing.T) {
	var buf bytes.Buffer
	NewEmitter(&buf).Progress("estimating", PctUnknown, 0, 0)

	var got Line
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Pct == nil {
		t.Fatal("pct was dropped; an absent percentage is indistinguishable from zero")
	}
	if *got.Pct != PctUnknown {
		t.Errorf("pct = %v, want %v", *got.Pct, PctUnknown)
	}
}

// Zero percent is a real reading and must not be confused with "unknown".
func TestZeroPercentIsNotUnknown(t *testing.T) {
	var buf bytes.Buffer
	NewEmitter(&buf).Progress("starting", 0, 0, 0)

	var got Line
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Pct == nil || *got.Pct != 0 {
		t.Errorf("pct = %v, want an explicit 0", got.Pct)
	}
}

// One result, and last. A second would leave Maison reading the first while the verb
// believed it had reported the second.
func TestASecondResultIsRefused(t *testing.T) {
	e := NewEmitter(&bytes.Buffer{})
	if err := e.Result(Status{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Result(Status{}); err == nil {
		t.Error("a second result was accepted")
	}
}

// Source ids feed tag values and, downstream, path construction. Anything not of the
// two defined shapes is refused rather than guessed at.
func TestValidSource(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"userdata", true},
		{"app:jellyfin", true},
		{"app:my-app_1.2", true},
		{"app:", false},
		{"jellyfin", false},
		{"app:../../etc", false},
		{"app:a/b", false},
		{"app:a b", false},
		{"", false},
	} {
		if got := ValidSource(tc.id); got != tc.want {
			t.Errorf("ValidSource(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// Every sentinel keeps its own exit code, wrapped or not. A wrong key collapsed into 1
// reads as a broken engine rather than as "try again".
func TestCodeFor(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{nil, ExitOK},
		{ErrNotConfigured, ExitNotConfigured},
		{ErrNotSupported, ExitNotSupported},
		{ErrNotWritable, ExitNotWritable},
		{ErrRepositoryExists, ExitRepositoryExists},
		{ErrWrongPassword, 14},
		{fmt.Errorf("recover: %w", ErrWrongPassword), 14},
		{errors.New("anything else"), ExitError},
	} {
		if got := CodeFor(tc.err); got != tc.want {
			t.Errorf("CodeFor(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}
