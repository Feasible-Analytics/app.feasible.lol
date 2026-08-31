//
// referrer.go
// Turning a raw Referer header into a stored referrer and a canonical source.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package referrer resolves where a visit came from. It answers two separate
// questions that are easy to conflate: what the referrer literally was, which
// is stored verbatim so a customer can audit it, and which canonical source it
// belongs to, which is what every report groups by.
//
// The rule that generates more support questions than anything else in the
// product lives one layer up, in the session fold, and is worth stating here
// too: attribution is frozen at session start. A UTM tag on the second pageview
// of a visit is discarded, which is why testing several campaign links
// back-to-back from one browser always looks broken and is not.
package referrer

import (
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Direct is the canonical source token for a visit with no referrer and no
// campaign tags. It is a real value rather than an absence because "Direct /
// None" is usually the largest bucket on the Sources tab, and leaving it out
// makes every total on the page look wrong.
const Direct = "(direct)"

// androidScheme is the referrer prefix an Android in-app browser sends. It is
// not a URL any host lookup resolves, so it is handled before parsing.
const androidScheme = "android-app://"

// Result is everything the acquisition pipeline needs from a referrer.
type Result struct {
	// Referrer is what gets stored: host and path, without the scheme, the
	// leading "www." or a query string. The query string is dropped because
	// referrer query strings routinely carry session tokens from the referring
	// site, which are neither ours to keep nor useful to report on.
	Referrer string

	// Source is the canonical name, or Direct.
	Source string

	// Category drives the channel rules.
	Category Category
}

// Parse resolves a Referer header against the site it arrived at. The site
// hostname is needed because a link from one page of a site to another is not
// an acquisition at all, and counting it as a referral would make every site
// its own biggest traffic source.
func Parse(rawReferrer, siteHost string) Result {
	rawReferrer = strings.TrimSpace(rawReferrer)
	if rawReferrer == "" {
		return Result{Source: Direct}
	}

	// An Android in-app referrer is a package name, not a host, and resolving
	// it is the difference between attributing in-app social traffic and
	// dumping all of it into Direct.
	if strings.HasPrefix(rawReferrer, androidScheme) {
		pkg := strings.Trim(strings.TrimPrefix(rawReferrer, androidScheme), "/")
		if source, ok := androidPackages[pkg]; ok {
			return Result{Referrer: pkg, Source: source.Name, Category: source.Category}
		}

		return Result{Referrer: pkg, Source: pkg}
	}

	parsed, err := url.Parse(rawReferrer)
	if err != nil || parsed.Host == "" {
		// A referrer we cannot parse is still evidence of something. Keeping the
		// raw string is what lets a customer see a malformed referrer rather
		// than wonder why traffic they can see in their own logs is Direct.
		trimmed := strings.TrimSpace(rawReferrer)
		return Result{Referrer: trimmed, Source: trimmed}
	}

	host := normaliseHost(parsed.Host)
	if host == "" {
		return Result{Source: Direct}
	}

	// Same-site navigation is not acquisition. Comparing registrable domains
	// rather than hostnames means a link from the blog subdomain to the
	// marketing site is correctly internal.
	if sameSite(host, normaliseHost(siteHost)) {
		return Result{Source: Direct}
	}

	result := Result{Referrer: host + trimPath(parsed.Path)}

	source, ok := lookupHost(host)
	if ok {
		result.Source = source.Name
		result.Category = source.Category
		return result
	}

	// An unrecognised referrer reports under its own domain, which is what a
	// customer expects to see and is far more useful than "other".
	result.Source = registrable(host)

	return result
}

// SourceFromUTM resolves an explicit utm_source tag to a canonical name. UTM
// tags win over the referrer because somebody set them on purpose, and a
// campaign that says it is Facebook is Facebook even when the click arrived
// through a link shortener.
func SourceFromUTM(utmSource string) (Source, bool) {
	key := strings.ToLower(strings.TrimSpace(utmSource))
	if key == "" {
		return Source{}, false
	}

	if source, ok := utmSourceAliases[key]; ok {
		return source, true
	}

	// A UTM source is often just a hostname somebody pasted in, so the host map
	// is worth a second look before giving up on the category.
	if source, ok := lookupHost(normaliseHost(key)); ok {
		return Source{Name: source.Name, Category: source.Category}, true
	}

	// Unknown but present: the tag is the source name, kept exactly as sent so
	// the Campaigns report can distinguish tags that the Sources tab folds
	// together.
	return Source{Name: strings.TrimSpace(utmSource)}, true
}

// lookupHost resolves a hostname, falling back to its registrable domain. The
// fallback is what makes one entry for "reddit.com" cover every regional and
// mobile subdomain without listing them.
func lookupHost(host string) (Source, bool) {
	if source, ok := hosts[host]; ok {
		return source, true
	}

	root := registrable(host)
	if root != host {
		if source, ok := hosts[root]; ok {
			return source, true
		}
	}

	// A global company runs one domain per country and they share no
	// registrable domain with each other — google.co.uk and google.com are two
	// separate purchases — so the last resort is the label before the public
	// suffix.
	if label, _, found := strings.Cut(root, "."); found {
		if source, ok := secondLevel[label]; ok {
			return source, true
		}
	}

	return Source{}, false
}

// normaliseHost lower-cases a host and drops the port and any leading "www.".
// Without it "WWW.Google.com:443" and "google.com" are two different sources on
// the same report.
func normaliseHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))

	// A bracketed IPv6 literal has no port to strip in the usual sense, and
	// cutting at the first colon would mangle it.
	if !strings.HasPrefix(host, "[") {
		if index := strings.LastIndex(host, ":"); index > 0 && !strings.Contains(host[index:], ":") {
			host = host[:index]
		}
	}

	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "www.")

	return host
}

// registrable returns the registrable domain — the part somebody actually
// bought. It is what makes subdomains of one site collapse into one source, and
// it falls back to the input because the public-suffix list has no answer for
// an IP literal or an internal hostname.
func registrable(host string) string {
	root, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || root == "" {
		return host
	}

	return root
}

// sameSite reports whether a referrer belongs to the site it arrived at.
func sameSite(referrerHost, siteHost string) bool {
	if siteHost == "" || referrerHost == "" {
		return false
	}

	return registrable(referrerHost) == registrable(siteHost)
}

// trimPath keeps a referrer path only when it says something. A bare "/" adds
// a character to every row and no information, and a trailing slash would split
// one referring page into two rows.
func trimPath(path string) string {
	if path == "" || path == "/" {
		return ""
	}

	return strings.TrimSuffix(path, "/")
}
