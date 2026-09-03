//
// main_test.go
// The coverage floor, which is the only thing standing between a half-fetched
// run and a committed list that quietly stops recognising a cloud.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package main

import (
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest/lists"
)

// ranges builds a list of the given length. The contents do not matter to the
// floor, only how many there are.
func ranges(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "192.0.2.0/24")
	}

	return out
}

// TestSoundAcceptsAFullRebuild checks that an ordinary run is allowed through.
// A floor that rejects the good case is a floor somebody turns off.
func TestSoundAcceptsAFullRebuild(t *testing.T) {
	if err := sound(ranges(len(lists.Datacenters())), 18, nil, false); err != nil {
		t.Fatalf("a full rebuild was refused: %v", err)
	}
}

// TestSoundRefusesAShrunkList checks the case the floor exists for: every
// source answered, one of them returned far less than it used to, and the
// result would otherwise be committed and shipped.
func TestSoundRefusesAShrunkList(t *testing.T) {
	err := sound(ranges(len(lists.Datacenters())/2), 18, nil, false)
	if err == nil {
		t.Fatal("a list half the committed size was accepted")
	}

	// The message has to say what to do about it, because the person reading it
	// is looking at a successful-looking run.
	if !strings.Contains(err.Error(), "-allow-shrink") {
		t.Errorf("the error does not say how to proceed deliberately: %v", err)
	}
}

// TestSoundRefusesWhenHalfTheSourcesFailed checks the other trip wire. Enough
// sources missing is a network problem at this end, and the ranges that did
// arrive are not a list worth keeping.
func TestSoundRefusesWhenHalfTheSourcesFailed(t *testing.T) {
	failed := []string{"AWS", "Azure", "Google Cloud", "Oracle Cloud", "Linode",
		"Vultr", "DigitalOcean", "Hetzner", "OVH"}

	if err := sound(ranges(len(lists.Datacenters())), 18, failed, false); err == nil {
		t.Fatal("half the sources failed and the run was accepted")
	}
}

// TestAllowShrinkIsDeliberate checks the escape hatch works, because dropping a
// source on purpose shrinks the list on purpose and the floor cannot tell the
// two apart.
func TestAllowShrinkIsDeliberate(t *testing.T) {
	if err := sound(ranges(10), 18, nil, true); err != nil {
		t.Fatalf("an explicitly allowed shrink was still refused: %v", err)
	}

	// It does not excuse a run where the sources were simply not reachable.
	failed := []string{"AWS", "Azure", "Google Cloud", "Oracle Cloud", "Linode",
		"Vultr", "DigitalOcean", "Hetzner", "OVH"}

	if err := sound(ranges(10), 18, failed, true); err == nil {
		t.Error("allow-shrink excused half the sources failing")
	}
}

// TestDescribeNamesTheProblem checks both halves of the message. "A source is
// missing" with nothing named is the kind of error that gets ignored.
func TestDescribeNamesTheProblem(t *testing.T) {
	if got := describe([]string{"Azure"}); !strings.Contains(got, "Azure") {
		t.Errorf("describe did not name the failed source: %q", got)
	}

	if got := describe(nil); !strings.Contains(got, "format") {
		t.Errorf("describe did not point at a format change when nothing failed: %q", got)
	}
}
