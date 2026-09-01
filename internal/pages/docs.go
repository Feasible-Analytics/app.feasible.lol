//
// docs.go
// The documentation set and the three legal pages, embedded in the binary.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package pages

import (
	"embed"
	"fmt"
	"html/template"
)

// content holds the prose. It is embedded rather than read from disk for the
// same reason everything else is: a release is one binary, and a docs directory
// that has to ship beside it is a directory that will be missing on somebody's
// server — usually the one whose owner is trying to find out why their events
// are being dropped.
//
//go:embed docs/*.html
var content embed.FS

// Doc is one page of prose.
type Doc struct {
	Slug    string
	Title   string
	Summary string

	// Body is trusted HTML written by us and compiled into the binary. It is
	// template.HTML rather than a string because it is our own file rather than
	// anything a user supplied, and escaping it would render the markup as
	// visible tags.
	Body template.HTML
}

// docsIndex is the documentation, in reading order. The order is the order
// somebody meets the product in: install it, check the numbers mean what they
// think, then the parts that need a decision.
var docsIndex = []Doc{
	{Slug: "installation", Title: "Installation", Summary: "One script tag, and how to tell whether it is working."},
	{Slug: "integrations", Title: "Integrations", Summary: "WordPress, Shopify, the frameworks, and why not to use a tag manager."},
	{Slug: "script-options", Title: "Script options", Summary: "Every data- attribute, and what happens at each limit."},
	{Slug: "proxying", Title: "Proxying", Summary: "Serving the script from your own domain, and the one thing to get right."},
	{Slug: "dashboard", Title: "The dashboard", Summary: "Every report, filters that live in the URL, comparison, realtime and the shortcuts."},
	{Slug: "metrics", Title: "Metric definitions", Summary: "What each number means, and four results that surprise people."},
	{Slug: "goals-funnels", Title: "Goals and funnels", Summary: "How they are counted, and why goals do not backfill."},
	{Slug: "custom-properties", Title: "Custom properties", Summary: "Segmenting by something only your application knows."},
	{Slug: "shields", Title: "Excluding traffic", Summary: "Blocking what you do not want counted, and merging URLs that are one page."},
	{Slug: "import-export", Title: "Import and export", Summary: "Bringing history in, and taking everything out — including every raw event."},
	{Slug: "api", Title: "The APIs", Summary: "Sending events, reading numbers back, keys and rate limits."},
	{Slug: "webhooks", Title: "Webhooks", Summary: "Being called when something happens, and verifying it was us."},
	{Slug: "mcp", Title: "The MCP server", Summary: "Letting an assistant read your analytics, over HTTP or stdio."},
	{Slug: "sdks", Title: "SDKs and plugins", Summary: "Five server-side SDKs, an npm loader, WordPress, Tag Manager and Looker Studio."},
	{Slug: "self-hosting", Title: "Self-hosting", Summary: "One binary, one directory, every feature, no billing."},
	{Slug: "privacy", Title: "Privacy and GDPR", Summary: "What we store, what we never store, and why pseudonymous is not anonymous."},
}

// legalIndex is the three documents a customer's lawyer asks for. They are kept
// out of the docs navigation because they are a different kind of reading, and
// they are linked from the footer of every page instead.
var legalIndex = []Doc{
	{Slug: "privacy", Title: "Privacy and data policy", Summary: "Who controls what, on what legal basis, and for how long."},
	{Slug: "terms", Title: "Terms of service", Summary: "What we sell, what happens if payment stops, and what we will never do with your data."},
	{Slug: "dpa", Title: "Data processing addendum", Summary: "The processor contract, the sub-processors, and the security measures."},
}

// loadDocs reads each entry's body out of the embedded tree. It runs once at
// start-up so that a missing or misnamed file is a panic on the first test run
// rather than a blank page discovered by a customer.
func loadDocs(index []Doc, prefix string) []Doc {
	out := make([]Doc, 0, len(index))

	for _, doc := range index {
		body, err := content.ReadFile("docs/" + prefix + doc.Slug + ".html")
		if err != nil {
			panic(fmt.Sprintf("pages: %v", err))
		}

		doc.Body = template.HTML(body) //nolint:gosec // our own file, embedded at build time
		out = append(out, doc)
	}

	return out
}

// The two sets, loaded once.
var (
	documentation = loadDocs(docsIndex, "")
	legal         = loadDocs(legalIndex, "legal-")
)

// findDoc looks a page up by slug.
func findDoc(set []Doc, slug string) (Doc, bool) {
	for _, doc := range set {
		if doc.Slug == slug {
			return doc, true
		}
	}

	return Doc{}, false
}
