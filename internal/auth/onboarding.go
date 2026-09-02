//
// onboarding.go
// The snippet, the platform instructions, and the check that actually fetches the page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/outbound"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/tracker"
)

// verifyTimeout caps the fetch of a customer's page. Ten seconds is more than a
// working site needs and short enough that a hung server does not hold the
// verification request open long enough for the browser to give up first.
const verifyTimeout = 10 * time.Second

// verifyMaxBytes is how much of a page is read before giving up on finding the
// snippet. A megabyte covers every real page; without a limit, one hostile URL
// streams until the process runs out of memory.
const verifyMaxBytes = 1 << 20

// FirstEventPollInterval is how often the waiting screen asks whether anything
// has arrived. Three seconds is frequent enough to feel live and slow enough
// that a tab left open overnight is not a load problem.
const FirstEventPollInterval = 3 * time.Second

// RoutingDelay is the worst case between creating a site and every serving
// process learning about it from the app shard system database. This number is
// what the waiting screen tells the user while remote snapshots refresh.
const RoutingDelay = 15 * time.Second

// Snippet renders the script tag for one site.
//
// The per-site path is used rather than the shared one. Filter lists name files
// individually, so a customer proxying a single well-known filename loses their
// traffic the day it is listed; a token that differs per site means one listing
// costs one site.
func Snippet(baseURL string, keyer *tracker.Keyer, site *Site) string {
	base := strings.TrimRight(baseURL, "/")

	if keyer == nil {
		return fmt.Sprintf(`<script defer data-domain="%s" src="%s%s"></script>`,
			site.Domain, base, tracker.PathLegacy)
	}

	return fmt.Sprintf(`<script defer src="%s%s"></script>`, base, keyer.Path(site.Domain))
}

// SnippetLegacy renders the attribute-carrying variant.
//
// It is offered alongside the per-site path for one reason: it is the exact
// shape an existing installation already has, so somebody migrating changes one
// hostname and nothing else. It is also what a tag manager needs, where the
// script tag is pasted into a field that may strip an opaque path.
func SnippetLegacy(baseURL string, site *Site) string {
	return fmt.Sprintf(`<script defer data-domain="%s" src="%s%s"></script>`,
		site.Domain, strings.TrimRight(baseURL, "/"), tracker.PathLegacy)
}

// InstallPlatform is one set of paste-this-here instructions.
type InstallPlatform struct {
	ID    string
	Name  string
	Steps []string

	// Note carries the one thing that is specific to this platform and that
	// people get wrong. It is the reason this list is not just "paste the
	// snippet" eleven times.
	Note string
}

// InstallPlatforms is the list the onboarding screen offers.
//
// These are written from each platform's own published behaviour rather than
// copied from anyone's documentation, and each one carries the specific step
// that platform hides — which is the only reason a list like this is worth
// having at all.
func InstallPlatforms() []InstallPlatform {
	return []InstallPlatform{
		{
			ID:   "html",
			Name: "Plain HTML",
			Steps: []string{
				"Open the template that produces the <head> of every page.",
				"Paste the snippet just before </head>.",
				"Deploy, then load any page on the site.",
			},
			Note: "If your pages are separate files rather than one template, the snippet has to go in every one of them.",
		},
		{
			ID:   "wordpress",
			Name: "WordPress",
			Steps: []string{
				"In the admin, go to Appearance → Theme File Editor and open header.php.",
				"Paste the snippet just before </head>, or use any header-scripts plugin.",
				"Save, then load the front page in a private window.",
			},
			Note: "Use a child theme or a plugin. A snippet pasted into a parent theme is erased by the next theme update, and the traffic stops with no warning.",
		},
		{
			ID:   "nextjs",
			Name: "Next.js",
			Steps: []string{
				"App router: add the tag to app/layout.tsx inside <head>.",
				"Pages router: add it to pages/_document.tsx inside <Head>.",
				"Redeploy and open the site.",
			},
			Note: "next/script with strategy=\"afterInteractive\" also works. What does not work is putting it in a client component that only renders on some routes — you will lose every page it does not render on.",
		},
		{
			ID:   "nuxt",
			Name: "Nuxt",
			Steps: []string{
				"Open nuxt.config.ts.",
				"Add the script under app.head.script, with defer: true and the src from the snippet.",
				"Redeploy and open the site.",
			},
			Note: "Nuxt renders the head on the server, so the tag is in the initial HTML — do not also add it with useHead on a page, or every pageview is counted twice.",
		},
		{
			ID:   "astro",
			Name: "Astro",
			Steps: []string{
				"Open the layout every page uses, usually src/layouts/Layout.astro.",
				"Paste the snippet inside <head>.",
				"Rebuild and deploy.",
			},
			Note: "Astro strips <script> tags it decides to process. Keeping the src attribute and adding is:inline is the reliable form.",
		},
		{
			ID:   "shopify",
			Name: "Shopify",
			Steps: []string{
				"Online Store → Themes → ⋯ → Edit code.",
				"Open layout/theme.liquid and paste the snippet before </head>.",
				"Save, then visit the storefront.",
			},
			Note: "The checkout is a separate surface on most plans and will not include this snippet, so checkout pages do not appear in your pages report.",
		},
		{
			ID:   "webflow",
			Name: "Webflow",
			Steps: []string{
				"Site settings → Custom code → Head code.",
				"Paste the snippet and save.",
				"Publish the site — custom code does not apply to the preview.",
			},
			Note: "Custom code needs a paid site plan. On a free .webflow.io site the field saves but nothing is ever published.",
		},
		{
			ID:   "squarespace",
			Name: "Squarespace",
			Steps: []string{
				"Settings → Advanced → Code Injection.",
				"Paste the snippet into the Header field and save.",
				"Load the live site, not the editor preview.",
			},
			Note: "Code injection needs a Business plan or above, and it does not run inside the editor — always check in a private window.",
		},
		{
			ID:   "ghost",
			Name: "Ghost",
			Steps: []string{
				"Settings → Code injection.",
				"Paste the snippet into Site header and save.",
				"Load any post.",
			},
			Note: "This covers the site but not Ghost's own portal or checkout overlays, which render on a different origin.",
		},
		{
			ID:   "framer",
			Name: "Framer",
			Steps: []string{
				"Project settings → General → Custom code.",
				"Paste the snippet into Start of <head>.",
				"Publish.",
			},
			Note: "Custom code only runs on the published site, never in the canvas or the preview.",
		},
		{
			ID:   "gtm",
			Name: "Google Tag Manager",
			Steps: []string{
				"New tag → Custom HTML, and paste the snippet.",
				"Trigger it on All Pages.",
				"Submit and publish the container.",
			},
			Note: "Use the data-domain form of the snippet here — tag managers rewrite opaque paths. And be aware that loading a tracker through a tag manager means every blocker that hides the tag manager also hides us.",
		},
	}
}

// VerifyOutcome is what the installation check found. The four values are the
// four things that are actually wrong in practice, and each one has a different
// fix, which is why this is not a boolean.
type VerifyOutcome string

const (
	// VerifyFound is the snippet present and pointing at this site.
	VerifyFound VerifyOutcome = "found"

	// VerifyMissing is a page that loaded fine with no snippet in it. Usually
	// the change was not deployed, or it went into a template this page does
	// not use.
	VerifyMissing VerifyOutcome = "missing"

	// VerifyWrongDomain is a snippet whose data-domain names a different site.
	// It is the classic copy-paste from another site's setup screen, and it
	// sends every pageview to the wrong place while looking installed.
	VerifyWrongDomain VerifyOutcome = "wrong_domain"

	// VerifyBlockedByCSP is a snippet that is present but that the page's own
	// Content-Security-Policy will not let the browser load. Nothing in the
	// HTML looks wrong, which is what makes this one so hard to diagnose
	// without being told.
	VerifyBlockedByCSP VerifyOutcome = "blocked_by_csp"

	// VerifyUnreachable is a page we could not fetch at all.
	VerifyUnreachable VerifyOutcome = "unreachable"
)

// VerifyResult is the full answer, including the detail the message needs.
type VerifyResult struct {
	Outcome     VerifyOutcome
	URL         string
	StatusCode  int
	FoundDomain string
	CSPHeader   string
	Message     string
}

// OK reports whether the installation is good.
func (v VerifyResult) OK() bool {
	return v.Outcome == VerifyFound
}

// scriptTag matches a script tag whose source could be ours. It is a regular
// expression over the raw HTML rather than a parsed document because we are
// looking for one attribute on one tag, and a full HTML parser would be a
// dependency and a lot of tree walking to answer the same question.
var scriptTag = regexp.MustCompile(`(?is)<script[^>]*\ssrc\s*=\s*["']([^"']+)["'][^>]*>`)

// dataDomainAttr pulls the data-domain off a matched script tag.
var dataDomainAttr = regexp.MustCompile(`(?is)data-domain\s*=\s*["']([^"']+)["']`)

// VerifyInstallation fetches a site's home page and reports what it found.
//
// It really fetches. A verification step that only checks whether an event has
// arrived cannot tell "you have not deployed yet" from "your CSP is blocking
// us", and those have completely different fixes — which is exactly the moment
// somebody gives up and files a support ticket instead.
func VerifyInstallation(ctx context.Context, client *http.Client, baseURL string, site *Site) (result VerifyResult) {
	target := "https://" + site.Domain + "/"

	result = VerifyResult{URL: target}

	if client == nil {
		client = outbound.Policy{}.NewClient(verifyTimeout)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		result.Outcome = VerifyUnreachable
		result.Message = "We could not build a request for " + target + "."

		return result
	}

	// A real browser user agent, because a surprising number of sites serve a
	// stripped page or a challenge to anything that looks like a robot, and a
	// challenge page never contains the snippet.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; feasible.lol installation check; +https://feasible.lol/bot)")

	resp, err := client.Do(req)
	if err != nil {
		result.Outcome = VerifyUnreachable
		result.Message = "We could not load " + target + ": " + err.Error()

		return result
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			result.Outcome = VerifyUnreachable
			result.Message = "We loaded " + target + " but could not finish closing the connection."
		}
	}()

	result.StatusCode = resp.StatusCode
	result.CSPHeader = resp.Header.Get("Content-Security-Policy")

	if resp.StatusCode >= 400 {
		result.Outcome = VerifyUnreachable
		result.Message = fmt.Sprintf("%s answered %d. We can only check a page that loads.", target, resp.StatusCode)

		return result
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, verifyMaxBytes))
	if err != nil {
		result.Outcome = VerifyUnreachable
		result.Message = "We started reading " + target + " but the connection ended early."

		return result
	}

	host := hostOf(baseURL)
	html := string(body)

	var (
		found       bool
		foundDomain string
	)

	for _, match := range scriptTag.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(match[0], host) && !strings.Contains(match[1], host) {
			continue
		}

		found = true

		if attr := dataDomainAttr.FindStringSubmatch(match[0]); attr != nil {
			foundDomain = strings.ToLower(strings.TrimSpace(attr[1]))
		}

		break
	}

	if !found {
		result.Outcome = VerifyMissing
		result.Message = "We loaded " + target + " but found no snippet in the HTML. If you have just deployed, give the CDN a minute and check again."

		return result
	}

	// A data-domain that names a different site is the copy-paste mistake, and
	// it is worth calling out precisely: the page looks instrumented, and every
	// pageview is being filed under somebody else's site.
	if foundDomain != "" {
		result.FoundDomain = foundDomain

		// The snippet may legitimately list several domains, which is how one
		// script serves a site and its www variant.
		matched := false
		for _, candidate := range strings.Split(foundDomain, ",") {
			if normaliseHost(candidate) == normaliseHost(site.Domain) {
				matched = true
				break
			}
		}

		if !matched {
			result.Outcome = VerifyWrongDomain
			result.Message = "The snippet on " + target + " says data-domain=\"" + foundDomain +
				"\", but this site is " + site.Domain + ". Every pageview is being recorded against the wrong site."

			return result
		}
	}

	// The snippet is there; the remaining question is whether the browser will
	// be allowed to load it.
	if csp := result.CSPHeader; csp != "" && !cspAllows(csp, host) {
		result.Outcome = VerifyBlockedByCSP
		result.Message = "The snippet is on the page, but this site's Content-Security-Policy does not allow scripts from " +
			host + ". Add it to your script-src directive."

		return result
	}

	result.Outcome = VerifyFound
	result.Message = "The snippet is installed on " + target + " and nothing is blocking it."

	return result
}

// cspAllows reports whether a Content-Security-Policy would let the browser
// load a script from a host.
//
// It reads only script-src, falling back to default-src, because those are the
// two directives that decide whether a script tag loads. It is deliberately
// forgiving: a policy we cannot parse is treated as permissive, since telling
// somebody their CSP is blocking us when it is not sends them to edit a
// security header for no reason.
func cspAllows(policy, host string) bool {
	directives := map[string]string{}

	for _, part := range strings.Split(policy, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		name, value, _ := strings.Cut(part, " ")
		directives[strings.ToLower(strings.TrimSpace(name))] = strings.ToLower(value)
	}

	sources, ok := directives["script-src"]
	if !ok {
		sources, ok = directives["default-src"]
	}
	if !ok {
		return true
	}

	if strings.Contains(sources, "*") && !strings.Contains(sources, "'none'") {
		return true
	}

	// A host-only match is enough. Matching the scheme and port as well would
	// be more correct and would produce false alarms on every policy written
	// with a bare hostname, which is most of them.
	bare := normaliseHost(host)

	for _, source := range strings.Fields(sources) {
		if normaliseHost(source) == bare {
			return true
		}
	}

	return false
}

// hostOf pulls the host out of a base URL, so the checks compare hostnames
// rather than whole URLs that differ in scheme or trailing slash.
func hostOf(baseURL string) string {
	trimmed := baseURL

	if idx := strings.Index(trimmed, "://"); idx >= 0 {
		trimmed = trimmed[idx+3:]
	}

	trimmed = strings.TrimSuffix(trimmed, "/")

	if idx := strings.Index(trimmed, "/"); idx >= 0 {
		trimmed = trimmed[:idx]
	}

	return trimmed
}

// normaliseHost strips what does not matter when comparing two hostnames from
// two different sources — a scheme, a port, a leading www, quoting.
func normaliseHost(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Trim(value, "'\"")

	if idx := strings.Index(value, "://"); idx >= 0 {
		value = value[idx+3:]
	}

	value = strings.TrimPrefix(value, "www.")
	value = strings.TrimSuffix(value, "/")

	if idx := strings.Index(value, ":"); idx >= 0 {
		value = value[:idx]
	}

	return value
}
