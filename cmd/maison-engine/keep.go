package main

import (
	"encoding/json"
	"fmt"

	"github.com/yundera/maison-kopia-engine/internal/kopia"
)

// parseKeep reads the retention tiers.
//
// JSON rather than five flags so the set travels as one value: a caller that meant to
// clear a tier and omitted its flag would otherwise silently keep whatever the previous
// call set, and retention is the one place where "silently kept" and "silently deleted"
// are the same bug seen from two sides.
func parseKeep(s string) (kopia.Keep, error) {
	var k kopia.Keep
	if s == "" {
		return k, fmt.Errorf("--keep is required")
	}
	var raw struct {
		Latest  int `json:"latest"`
		Daily   int `json:"daily"`
		Weekly  int `json:"weekly"`
		Monthly int `json:"monthly"`
		Annual  int `json:"annual"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return k, fmt.Errorf("unreadable --keep: %w", err)
	}
	return kopia.Keep{
		Latest: raw.Latest, Daily: raw.Daily, Weekly: raw.Weekly,
		Monthly: raw.Monthly, Annual: raw.Annual,
	}, nil
}
