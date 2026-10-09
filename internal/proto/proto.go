// Package proto is the Maison backup adapter wire format.
//
// One JSON object per line on stdout, nothing else ever — Maison parses every line,
// so a stray fmt.Println is a protocol violation. Diagnostics that are not worth a log
// line go to stderr, whose tail Maison keeps for the error message.
//
// See docs/protocol.md, which is authoritative.
package proto

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Line types.
const (
	TypeProgress = "progress"
	TypeLog      = "log"
	TypeResult   = "result"
)

// PctUnknown marks progress that carries no measurable percentage. The bar renders as
// indeterminate rather than as a lie.
const PctUnknown = -1.0

// Line is one NDJSON record. Encoding uses the narrower types below; this is the shape
// both sides decode.
type Line struct {
	Type    string   `json:"type"`
	Message string   `json:"message,omitempty"`
	Level   string   `json:"level,omitempty"`
	Pct     *float64 `json:"pct,omitempty"`
	Done    int64    `json:"done,omitempty"`
	Total   int64    `json:"total,omitempty"`

	// Result is the verb's payload, present on exactly one line and only the last.
	Result json.RawMessage `json:"result,omitempty"`
}

// Caps is what `capabilities` returns. Zero values are the conservative answer: a field
// an adapter omits means "this engine cannot", never "can".
type Caps struct {
	EngineID       string `json:"engineId"`
	EngineVersion  string `json:"engineVersion,omitempty"`
	AdapterVersion string `json:"adapterVersion,omitempty"`
	Protocol       string `json:"protocol"`

	Offsite           bool `json:"offsite,omitempty"`
	Encrypted         bool `json:"encrypted,omitempty"`
	KeyEscrow         bool `json:"keyEscrow,omitempty"`
	InstantRestore    bool `json:"instantRestore,omitempty"`
	NeedsLocalSpace   bool `json:"needsLocalSpace,omitempty"`
	InPlaceRestore    bool `json:"inPlaceRestore,omitempty"`
	IncrementalPasses bool `json:"incrementalPasses,omitempty"`
	ConsumesSource    bool `json:"consumesSource,omitempty"`
	Retention         bool `json:"retention,omitempty"`

	// RetentionModel says what kind of expiry this storage can survive:
	// "snapshot" | "chain" | "lifecycle" | "none". See docs/protocol.md.
	RetentionModel string `json:"retentionModel,omitempty"`

	// Recover says the adapter implements `recover`: reconnecting a rebuilt box to the
	// repository its storage already holds, with a key the user supplies. Maison offers
	// the key form only when this is set.
	Recover bool `json:"recover,omitempty"`

	// ChangeSecret says the adapter implements `change-secret`: replacing the key that
	// opens the repository, on a box that holds the current one. Maison offers the
	// change form only when this is set.
	ChangeSecret bool `json:"changeSecret,omitempty"`
}

// Status is what `status` returns.
//
// Configured and Connected are different questions. Not configured is a box whose
// host-side provisioning has not run — normal, and not a fault. Configured and
// unreachable IS a fault, and is what raises an incident.
//
// Label, writability and credential expiry are deliberately absent: they come from the
// host-written state.json, which is engine-neutral and which Maison already reads.
//
// NeedsRecovery is a third state, distinct from both: the storage holds a repository
// this box has no key for (a rebuilt box). Configured and Connected are false, because
// nothing here can open it, and it is not "awaiting provisioning" either — provisioning
// ran and stopped rather than create a second repository over the first. Only the key
// the user was mailed gets it out, through `recover`.
type Status struct {
	Configured    bool   `json:"configured"`
	Connected     bool   `json:"connected"`
	NeedsRecovery bool   `json:"needsRecovery,omitempty"`
	Identity      string `json:"identity,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// Recovered is what `recover` returns: how many snapshots the repository holds, and how
// many of them were pinned against expiry.
type Recovered struct {
	Snapshots int `json:"snapshots"`
	Pinned    int `json:"pinned"`
}

// Backup is one restorable backup of one source.
type Backup struct {
	SourceID  string    `json:"sourceId"`
	Stamp     string    `json:"stamp"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size,omitempty"`
}

// Entry is one top-level member of a snapshot, for the caller that has to decide which
// of them to restore.
type Entry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir,omitempty"`
}

// Emitter writes NDJSON. Its mutex matters: progress arrives from the goroutine reading
// the engine's stderr while the main goroutine may be writing the result.
type Emitter struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
	// done guards the one-result rule, so a bug cannot emit two and leave Maison
	// reading the first.
	done bool
}

func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{w: w, enc: json.NewEncoder(w)}
}

// Progress reports what was observed. Rate, ETA and smoothing are Maison's to derive —
// an adapter that computed them would be a second answer to a question already
// answered, differently, for every other engine.
//
// Total is allowed to move: an engine discovering a tree as it walks it revises its own
// estimate upward, and Maison expects that rather than treating it as a fault.
func (e *Emitter) Progress(msg string, pct float64, done, total int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.enc.Encode(Line{Type: TypeProgress, Message: msg, Pct: &pct, Done: done, Total: total})
}

// Log is a diagnostic for Maison's log, never for the UI.
func (e *Emitter) Log(level, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.enc.Encode(Line{Type: TypeLog, Level: level, Message: msg})
}

// Result emits the verb's payload. At most one per invocation, and last.
func (e *Emitter) Result(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding result: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return fmt.Errorf("a second result was emitted")
	}
	e.done = true
	return e.enc.Encode(Line{Type: TypeResult, Result: b})
}
