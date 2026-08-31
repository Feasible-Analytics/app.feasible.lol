//
// wrap.go
// Keeping every line under the SMTP limit, because a long one loses the whole message.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"strings"
	"unicode/utf8"
)

// MaxLineLength is the ceiling every body is wrapped to. RFC 5321 allows 998
// octets plus CRLF; wrapping at 900 leaves room for a relay that adds its own
// prefix and for the CRLF pair itself, and no message in this product is close
// enough to the limit for the margin to cost anything.
//
// The failure this guards against is total and silent. A server that sees a
// line over the limit rejects the message after DATA — not the line, the
// message — and the sender's own logs frequently record the send as fine.
const MaxLineLength = 900

// Wrap breaks a body so that no line exceeds the limit in bytes. It is
// deliberately conservative about where it breaks: HTML is whitespace-
// insensitive between tags, so a newline after a `>` or in place of a space
// changes nothing a reader sees, while a newline inside an attribute value
// would corrupt a link.
//
// A run with no safe break point still has to be broken — a data URI or a
// minified style block can exceed the limit on its own — and in that case the
// cut is made on a rune boundary so the body stays valid UTF-8.
func Wrap(body string, limit int) string {
	if limit <= 0 || len(body) <= limit {
		return body
	}

	var out strings.Builder
	out.Grow(len(body) + len(body)/limit + 8)

	for i, line := range strings.Split(body, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}

		writeWrapped(&out, line, limit)
	}

	return out.String()
}

// writeWrapped emits one already-split line, breaking it as many times as it
// takes. It is separate from Wrap so that the per-line loop and the per-break
// search do not have to share a variable, which is where an off-by-one in this
// kind of code always hides.
func writeWrapped(out *strings.Builder, line string, limit int) {
	for len(line) > limit {
		cut := breakPoint(line, limit)

		out.WriteString(strings.TrimRight(line[:cut], " \t"))
		out.WriteByte('\n')

		line = strings.TrimLeft(line[cut:], " \t")
	}

	out.WriteString(line)
}

// breakPoint chooses where to cut a line, preferring the break a reader will
// never notice. The order is the whole of the logic:
//
//  1. just after the last `>` that is not inside a quoted attribute, which is
//     between two tags and always safe
//  2. at the last space or tab that is not inside a quoted attribute, which
//     HTML collapses anyway
//  3. on the last rune boundary, for a run with neither
//
// Both of the first two skip anything inside quotes, because a newline in the
// middle of an href is worse than a long line: the message arrives and the
// upgrade button in it does not work. The third case only comes up for a single
// unbreakable run longer than the limit — a data URI, a minified style block —
// where the choice is between corrupting that run and losing the whole message,
// and losing the whole message is worse.
func breakPoint(line string, limit int) int {
	var (
		lastTag   = -1
		lastSpace = -1
		inQuote   byte
	)

	for i := 0; i < limit; i++ {
		c := line[i]

		switch {
		case inQuote != 0:
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
		case c == '>':
			lastTag = i
		case c == ' ' || c == '\t':
			lastSpace = i
		}
	}

	if lastTag > 0 {
		return lastTag + 1
	}

	if lastSpace > 0 {
		return lastSpace
	}

	// Cutting mid-rune would leave invalid UTF-8 in the body, which some mail
	// clients render as a replacement character and others refuse outright.
	cut := limit
	for cut > 0 && !utf8.ValidString(line[:cut]) {
		cut--
	}

	if cut == 0 {
		return limit
	}

	return cut
}

// LongestLine reports the longest line in a body, in bytes. It exists so the
// guarantee can be asserted rather than assumed: the test suite renders every
// template with hostile inputs and checks this against the limit.
func LongestLine(body string) int {
	longest := 0

	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) > longest {
			longest = len(line)
		}
	}

	return longest
}
