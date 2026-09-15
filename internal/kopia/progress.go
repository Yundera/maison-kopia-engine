package kopia

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/yundera/maison-kopia-engine/internal/proto"
)

// Turning kopia's --progress output into protocol progress events.
//
// This file is the whole of the kopia-specific progress knowledge, and it is the file
// a second adapter reimplements and nothing else. Rate, ETA, smoothing and gating are
// all computed downstream by Maison, identically for every engine — an adapter that
// computed them would be a second answer to a question already answered.
//
// Nothing here may fail a command: an unparsed line still carries its message.

// sizeExpr matches kopia's own rendering: "16 B", "100.8 MB", "1.2 GiB".
const sizeExpr = `[0-9]+(?:\.[0-9]+)?\s*[KMGTP]?i?B`

var (
	pctRe    = regexp.MustCompile(`\(([0-9]+(?:\.[0-9]+)?)%\)`)
	hashedRe = regexp.MustCompile(`hashed \((` + sizeExpr + `)\)`)
	cachedRe = regexp.MustCompile(`cached \((` + sizeExpr + `)\)`)

	// estimateRe is also the phase marker: a line carrying "estimated <size>" is past
	// the estimating stage. There is no state machine — every line is parsed alone.
	estimateRe = regexp.MustCompile(`estimated (` + sizeExpr + `)`)

	// restoreRe matches "Processed 1226 (1.1 GB) of 2000 (5.5 GB)." — the entry counts
	// either side are discarded, only the byte quantities are used.
	restoreRe = regexp.MustCompile(`\((` + sizeExpr + `)\)\s+of\s+\S+\s+\((` + sizeExpr + `)\)`)
)

// emitLine turns one line of kopia's output into a progress event.
//
// The message is always the raw line, parsed or not: it is what makes a support log
// readable, and the only thing to show while kopia is still estimating the tree.
func emitLine(out *proto.Emitter, line string) {
	if out == nil {
		return
	}
	pct := proto.PctUnknown
	if m := pctRe.FindStringSubmatch(line); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			pct = v
		}
	}
	done, total := bytesOf(line)
	out.Progress(line, pct, done, total)
}

// bytesOf extracts the byte counts, in three mutually exclusive shapes.
//
// On a snapshot line the total is kopia's own estimate and the done figure is
// hashed+cached. "uploaded" is deliberately excluded: after dedup and compression it is
// a fraction of what was read, and a bar built on it disagrees with the percentage
// printed on the same line.
func bytesOf(line string) (done, total int64) {
	if m := estimateRe.FindStringSubmatch(line); m != nil {
		total = parseSize(m[1])
		if h := hashedRe.FindStringSubmatch(line); h != nil {
			done += parseSize(h[1])
		}
		if c := cachedRe.FindStringSubmatch(line); c != nil {
			done += parseSize(c[1])
		}
		return done, total
	}
	if m := restoreRe.FindStringSubmatch(line); m != nil {
		return parseSize(m[1]), parseSize(m[2])
	}
	return 0, 0
}

// parseSize reads one size. Both unit families are accepted because --units is a kopia
// setting: guessing wrong would be a silent 7% error in every rate and ETA.
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	cut := len(s)
	for i, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			cut = i
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s[:cut]), 64)
	if err != nil {
		return 0
	}
	unit := strings.TrimSpace(s[cut:])
	base := 1000.0
	if strings.Contains(unit, "i") {
		base = 1024.0
	}
	mult := 1.0
	switch {
	case strings.HasPrefix(unit, "K"):
		mult = base
	case strings.HasPrefix(unit, "M"):
		mult = base * base
	case strings.HasPrefix(unit, "G"):
		mult = base * base * base
	case strings.HasPrefix(unit, "T"):
		mult = base * base * base * base
	case strings.HasPrefix(unit, "P"):
		mult = base * base * base * base * base
	}
	return int64(n * mult)
}
