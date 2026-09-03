//
// lists.go
// The embedded datacentre address list, and where it is regenerated from.
//
// Created: 2026-09-03
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package lists holds the classification data that ships inside the binary and
// the code that rebuilds it from its upstream sources.
//
// The address list is data rather than an algorithm, and it is data that goes
// stale: providers announce new space every week. So it lives in two places on
// purpose — an embedded copy, so a fresh install classifies correctly on its
// first request with no network and no setup, and a file in the data directory
// that wins over it, so a running install is never frozen at whatever its
// binary shipped with.
package lists

import (
	"bufio"
	_ "embed"
	"strings"
)

// datacentersFile is the generated baseline. It is committed rather than built,
// because `go build` has to work from a clean checkout with no network, and a
// list assembled at build time would make every build depend on a dozen and a
// half third-party services all being up at once.
//
//go:embed datacenters.txt
var datacentersFile string

// Datacenters returns the embedded address ranges, one CIDR block per line.
//
// These are hosting and cloud-compute ranges: a browser does not originate
// there, so traffic from them is a script. It is deliberately not a list of
// "bad" addresses — the classification it feeds keeps the row and buckets the
// visitor, because a commercial VPN exit is a datacentre address too and the
// person behind it is real.
func Datacenters() []string {
	return parse(datacentersFile)
}

// parse reads one newline-delimited list, dropping comments and blanks. The
// generated files carry a header saying when and from what they were built, and
// that header must not become a range nobody can parse.
func parse(body string) []string {
	var lines []string

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lines = append(lines, line)
	}

	return lines
}
