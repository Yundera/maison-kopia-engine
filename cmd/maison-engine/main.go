// Command maison-engine is the kopia adapter for Maison's backup engine seam.
//
// It runs inside the kopia image and is invoked one process per verb:
//
//	docker exec <container> maison-engine snapshot --source-id app:x --path /DATA/... --stamp ... --pass 2
//
// NDJSON on stdout, diagnostics on stderr, a documented exit code. See docs/protocol.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yundera/maison-kopia-engine/internal/kopia"
	"github.com/yundera/maison-kopia-engine/internal/proto"
)

func main() {
	os.Exit(run())
}

func run() int {
	out := proto.NewEmitter(os.Stdout)

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		return proto.ExitError
	}
	verb := os.Args[1]
	if verb == "-h" || verb == "--help" || verb == "help" {
		fmt.Fprintln(os.Stderr, usage)
		return proto.ExitOK
	}

	// Signals are forwarded to the engine child by cancelling its context, and the
	// process exits non-zero. An adapter that ignored them would leave kopia holding an
	// app's files open while Maison tries to restart the app — which is the entire
	// reason cancellation exists.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := dispatch(ctx, out, verb, os.Args[2:]); err != nil {
		// A cancelled run is not a failure to report loudly; it is what was asked for.
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "cancelled")
			return proto.ExitError
		}
		fmt.Fprintln(os.Stderr, err.Error())
		return proto.CodeFor(err)
	}
	return proto.ExitOK
}

const usage = `maison-engine <verb> [flags]

repository:  capabilities  connect  status  prepare
snapshots:   snapshot  commit  abort  list  list-all  delete
restore:     materialize  restore-in-place  entries
retention:   ensure-retention

Every verb takes --repo-dir. See docs/protocol.md.`

// flags is one verb's flag set, with the globals every verb accepts.
//
// UNKNOWN FLAGS ARE REJECTED, never ignored, which is flag.Package's default and is
// kept deliberately: ignoring --exclude-file silently ships data the caller declared
// derived, and ignoring --entries silently restores more than was asked for.
type flags struct {
	fs      *flag.FlagSet
	repoDir *string
	timeout *time.Duration
}

func newFlags(verb string) *flags {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return &flags{
		fs:      fs,
		repoDir: fs.String("repo-dir", "", "the engine's own directory (repository config, password, credentials, caches)"),
		timeout: fs.Duration("timeout", 0, "advisory deadline; Maison enforces its own"),
	}
}

func (f *flags) parse(args []string) error { return f.fs.Parse(args) }

// engine builds the engine and the context this verb runs under.
func (f *flags) engine(ctx context.Context, out *proto.Emitter) (*kopia.Engine, context.Context, context.CancelFunc, error) {
	if *f.repoDir == "" {
		return nil, nil, nil, fmt.Errorf("--repo-dir is required")
	}
	cancel := context.CancelFunc(func() {})
	if *f.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, *f.timeout)
	}
	return &kopia.Engine{Dir: *f.repoDir, Out: out, Bin: os.Getenv("MAISON_ENGINE_BINARY")}, ctx, cancel, nil
}

func dispatch(ctx context.Context, out *proto.Emitter, verb string, args []string) error {
	f := newFlags(verb)

	// Per-verb flags, declared before parsing so an unknown one is still rejected.
	var (
		sourceID    = f.fs.String("source-id", "", "app:<name> or userdata")
		path        = f.fs.String("path", "", "the source path, passed by the caller and never derived here")
		stamp       = f.fs.String("stamp", "", "the caller's identifier for one backup")
		dest        = f.fs.String("dest", "", "absolute destination path for a restore")
		pass        = f.fs.Int("pass", 0, "1 for the live pass, 2 for the stopped one")
		consume     = f.fs.Bool("consume", false, "the source is being destroyed; an engine that can take it may")
		excludeFile = f.fs.String("exclude-file", "", "newline-delimited ignore patterns, resolved by the caller; giving the flag installs them, and an empty file clears them")
		entries     = f.fs.String("entries", "", "comma-separated top-level entries to restore")
		keep        = f.fs.String("keep", "", "retention tiers as JSON")
		userData    = f.fs.Bool("user-data", false, "this source is the user-data set")

		// connect
		bucket   = f.fs.String("bucket", "", "")
		endpoint = f.fs.String("endpoint", "", "a URL; converted to what kopia parses")
		region   = f.fs.String("region", "", "")
		prefix   = f.fs.String("prefix", "", "")
		hostname = f.fs.String("hostname", "", "the identity to pin, written exactly once")
		username = f.fs.String("username", "", "")
		repoPath = f.fs.String("repo-path", "", "filesystem repository path, for development")
	)

	if err := f.parse(args); err != nil {
		return err
	}
	if f.fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", f.fs.Arg(0))
	}

	e, ctx, cancel, err := f.engine(ctx, out)
	if err != nil {
		return err
	}
	defer cancel()

	switch verb {
	case "capabilities":
		return out.Result(e.Caps(ctx))

	case "status":
		return out.Result(e.Status(ctx))

	case "prepare":
		return e.Prepare(ctx)

	case "connect":
		return e.Connect(ctx, kopia.ConnectOpts{
			Bucket: *bucket, Endpoint: *endpoint, Region: *region, Prefix: *prefix,
			Hostname: *hostname, Username: *username, Path: *repoPath,
		})

	case "snapshot":
		rules, err := readExcludes(*excludeFile)
		if err != nil {
			return err
		}
		return e.Snapshot(ctx, kopia.SnapshotOpts{
			SourceID: *sourceID, Path: *path, Stamp: *stamp,
			Pass: *pass, Consume: *consume,
			// Presence of the flag is the instruction, so an empty file clears the
			// rules rather than leaving the previous ones in place.
			Exclude: rules, ApplyExclude: *excludeFile != "",
		})

	case "commit":
		b, err := e.Commit(ctx, *sourceID, *stamp)
		if err != nil {
			return err
		}
		return out.Result(b)

	case "abort":
		return e.Abort(ctx, *sourceID, *stamp)

	case "delete":
		return e.Delete(ctx, *sourceID, *stamp)

	case "list":
		list, err := e.List(ctx, *sourceID)
		if err != nil {
			return err
		}
		return out.Result(list)

	case "list-all":
		all, err := e.ListAll(ctx)
		if err != nil {
			return err
		}
		return out.Result(all)

	case "entries":
		list, err := e.Entries(ctx, *sourceID, *stamp)
		if err != nil {
			return err
		}
		return out.Result(list)

	case "materialize":
		return e.Materialize(ctx, *sourceID, *stamp, *dest)

	case "restore-in-place":
		return e.RestoreInPlace(ctx, *sourceID, *stamp, *dest, splitList(*entries))

	case "ensure-retention":
		k, err := parseKeep(*keep)
		if err != nil {
			return err
		}
		return e.EnsureRetention(ctx, *path, k, *userData)

	default:
		return fmt.Errorf("unknown verb %q\n\n%s", verb, usage)
	}
}

// readExcludes reads the patterns the caller resolved.
//
// They are read from a FILE rather than repeated flags because a pattern can contain
// anything an app author wrote, and an absent file means no exclusions — which is
// different from an empty one only in that neither changes the outcome.
func readExcludes(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading exclusions: %w", err)
	}
	var rules []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rules = append(rules, line)
		}
	}
	return rules, nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
