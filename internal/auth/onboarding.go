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
	"net/url"
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
func Snippet(baseURL, scriptBaseURL string, keyer *tracker.Keyer, site *Site) string {
	base := strings.TrimRight(baseURL, "/")

	if keyer == nil {
		return SnippetLegacy(baseURL, scriptBaseURL, site)
	}

	source := scriptSource(base, scriptBaseURL) + keyer.Path(site.Domain)

	return fmt.Sprintf("%s\n"+`<script defer src="%s"></script>`, tracker.QueueStub, source)
}

// SnippetLegacy renders the attribute-carrying variant.
//
// It is offered alongside the per-site path for one reason: it is the exact
// shape an existing installation already has, so somebody migrating changes one
// hostname and nothing else. It is also what a tag manager needs, where the
// script tag is pasted into a field that may strip an opaque path.
func SnippetLegacy(baseURL, scriptBaseURL string, site *Site) string {
	base := strings.TrimRight(baseURL, "/")

	return fmt.Sprintf("%s\n"+`<script defer data-domain="%s" src="%s%s"></script>`,
		tracker.QueueStub, site.Domain, scriptSource(base, scriptBaseURL), tracker.PathLegacy)
}

// scriptSource picks the origin the script tag points at.
//
// Whatever it names has to answer /api/event as well as serve the file, because
// the script reports to wherever it was loaded from. That default is what makes
// a customer's own reverse proxy work with nothing else to configure, and
// keeping it means the snippet stays one URL with nothing to get half-right.
func scriptSource(baseURL, scriptBaseURL string) string {
	if script := strings.TrimRight(scriptBaseURL, "/"); script != "" {
		return script
	}

	return baseURL
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
				"App router: add both tags to app/layout.tsx inside <head>.",
				"Pages router: add them to pages/_document.tsx inside <Head>.",
				"Redeploy and open the site.",
			},
			Note: "next/script with strategy=\"afterInteractive\" works for the second tag, but write the first as a plain inline script — a deferred first line is a first line that does nothing. What does not work at all is putting either in a client component that only renders on some routes, because you lose every page it does not render on.",
		},
		{
			ID:   "nuxt",
			Name: "Nuxt",
			Steps: []string{
				"Open nuxt.config.ts.",
				"Add both parts under app.head: the first line as an inline script, and the second under app.head.script with defer: true and the src from the snippet.",
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
			Note: "Astro processes <script> tags it recognises, which moves them out of the head and changes when they run. Add is:inline to both tags — the first one needs it most, because a hoisted stub runs after the very calls it exists to catch.",
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

// VerifyOutcome is what the installation check found. Each failing value has a
// different fix, which is why this is not a boolean.
type VerifyOutcome string

const (
	// VerifyFound is the snippet present and pointing at this site.
	VerifyFound VerifyOutcome = "found"

	// VerifyMissing is a page that loaded fine with no snippet in its HTML.
	// Usually the change was not deployed, or it went into a template this
	// page does not use, or a tag manager adds it in the browser.
	VerifyMissing VerifyOutcome = "missing"

	// VerifyCollecting is a page with no snippet in its HTML on a site whose
	// visits are arriving anyway.
	VerifyCollecting VerifyOutcome = "collecting"

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
	return v.Outcome == VerifyFound || v.Outcome == VerifyCollecting
}

// InstallCheck is what the installation check needs to know about this server
// to recognise its own snippet on somebody else's page.
type InstallCheck struct {
	// BaseURL is the application's own origin. A self-hosted install serves
	// its script from here, and so did the hosted service's earlier snippets.
	BaseURL string

	// ScriptBaseURL is the origin today's snippet loads the script from, when
	// that is not the application itself.
	ScriptBaseURL string

	// Keyer derives the per-site script path, which identifies a snippet on
	// any host, including a customer's own reverse proxy.
	Keyer *tracker.Keyer
}

// hosts lists every hostname a snippet handed out by this server points at.
func (c InstallCheck) hosts() []string {
	var hosts []string

	for _, origin := range []string{c.ScriptBaseURL, c.BaseURL} {
		if host := normaliseHost(hostOf(origin)); host != "" {
			hosts = append(hosts, host)
		}
	}

	return hosts
}

// scriptTag matches an opening script tag and captures its attributes. Quoted
// values are stepped over whole so a ">" inside one does not end the tag early.
var scriptTag = regexp.MustCompile(`(?is)<script\b((?:[^>"']|"[^"]*"|'[^']*')*)>`)

// tagAttr matches one attribute, with a double-quoted, single-quoted, unquoted
// or absent value. Minifiers drop the quotes whenever the value allows it.
var tagAttr = regexp.MustCompile("(?s)([^\\s\"'<>/=]+)(?:\\s*=\\s*(?:\"([^\"]*)\"|'([^']*)'|([^\\s\"'=<>`]+)))?")

// tagAttrs reads the attributes of one tag into a map keyed by lowercase name.
func tagAttrs(raw string) map[string]string {
	attrs := map[string]string{}

	for _, match := range tagAttr.FindAllStringSubmatch(raw, -1) {
		name := strings.ToLower(match[1])

		// A repeated attribute keeps its first value, as a browser does.
		if _, seen := attrs[name]; seen {
			continue
		}

		attrs[name] = strings.TrimSpace(match[2] + match[3] + match[4])
	}

	return attrs
}

// snippetMatch says how one script tag relates to the site being checked.
type snippetMatch int

const (
	// snippetNone is a script tag that is not ours.
	snippetNone snippetMatch = iota

	// snippetOurs is our script, pointing at this site.
	snippetOurs

	// snippetOtherSite is our script, pointing at a different site.
	snippetOtherSite
)

// classifyScript decides whether one script tag is this site's snippet.
//
// Three things identify it: the per-site path, which is unique to the site on
// any host; one of this server's own hostnames; or a data-domain naming the
// site on a tag shaped like ours, which is what a proxied install looks like.
func classifyScript(check InstallCheck, site *Site, attrs map[string]string) (snippetMatch, string) {
	src := attrs["src"]
	if src == "" {
		return snippetNone, ""
	}

	parsed, err := url.Parse(src)
	if err != nil {
		return snippetNone, ""
	}

	host := normaliseHost(parsed.Host)
	file := parsed.Path[strings.LastIndex(parsed.Path, "/")+1:]
	perSite := strings.HasPrefix(file, "fs-") && strings.HasSuffix(file, ".js")

	if check.Keyer != nil && perSite && strings.HasSuffix(check.Keyer.Path(site.Domain), "/"+file) {
		return snippetOurs, host
	}

	domain, hasDomain := attrs["data-domain"]
	namesSite := hasDomain && domainListHas(domain, site.Domain)

	ownHost := false
	for _, candidate := range check.hosts() {
		if host == candidate {
			ownHost = true
			break
		}
	}

	if ownHost {
		if hasDomain && !namesSite {
			return snippetOtherSite, host
		}

		// A per-site path that is not this site's is another site's snippet.
		// Without a keyer there is nothing to compare it with.
		if perSite && check.Keyer != nil {
			return snippetOtherSite, host
		}

		return snippetOurs, host
	}

	// Other analytics scripts carry data-domain too, so on a host we do not
	// know the tag must also look like ours.
	_, hasAPI := attrs["data-api"]
	if namesSite && (hasAPI || perSite || file == "script.js") {
		return snippetOurs, host
	}

	return snippetNone, ""
}

// domainListHas reports whether a data-domain value, which may list several
// domains, names the site.
func domainListHas(list, domain string) bool {
	for _, candidate := range strings.Split(list, ",") {
		if normaliseHost(candidate) == normaliseHost(domain) {
			return true
		}
	}

	return false
}

// VerifyInstallation fetches a site's home page and reports what it found.
//
// It really fetches. A verification step that only checks whether an event has
// arrived cannot tell "you have not deployed yet" from "your CSP is blocking
// us", and those have completely different fixes — which is exactly the moment
// somebody gives up and files a support ticket instead.
func VerifyInstallation(ctx context.Context, client *http.Client, check InstallCheck, site *Site) (result VerifyResult) {
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

	var (
		found      bool
		scriptHost string
		otherSite  bool
		otherName  string
	)

	// Every tag is read before deciding, so a page carrying two snippets is
	// judged by the one that is right rather than the one that comes first.
	for _, match := range scriptTag.FindAllStringSubmatch(string(body), -1) {
		attrs := tagAttrs(match[1])

		kind, host := classifyScript(check, site, attrs)
		if kind == snippetOurs {
			found = true
			scriptHost = host

			break
		}

		if kind == snippetOtherSite && !otherSite {
			otherSite = true
			otherName = strings.ToLower(attrs["data-domain"])
		}
	}

	// A snippet for a different site is the copy-paste mistake, and it is worth
	// calling out precisely: the page looks instrumented, and every pageview is
	// being filed under somebody else's site.
	if !found && otherSite {
		result.Outcome = VerifyWrongDomain
		result.FoundDomain = otherName

		if otherName != "" {
			result.Message = "The snippet on " + target + " says data-domain=\"" + otherName +
				"\", but this site is " + site.Domain + ". Every pageview is being recorded against the wrong site."
		} else {
			result.Message = "The snippet on " + target + " belongs to a different site, not " + site.Domain +
				". Copy the snippet from this screen and replace it."
		}

		return result
	}

	if !found {
		result.Outcome = VerifyMissing
		result.Message = "We loaded " + target + " and the snippet isn't in its HTML. " +
			"If you've just deployed, clear your cache and check again. " +
			"If you installed through a tag manager, the snippet is added in the browser where we can't see it. " +
			"Open your site and your first visit will show up on this screen."

		return result
	}

	// The snippet is there; the remaining question is whether the browser will
	// be allowed to load it. A script served by the page's own host is covered
	// by 'self' in any policy that lets the site run its own scripts.
	sameOrigin := scriptHost == "" || scriptHost == normaliseHost(site.Domain)

	if csp := result.CSPHeader; csp != "" && !sameOrigin && !cspAllows(csp, scriptHost) {
		result.Outcome = VerifyBlockedByCSP
		result.Message = "The snippet is on the page, but this site's Content-Security-Policy does not allow scripts from " +
			scriptHost + ". Add it to your script-src directive."

		return result
	}

	result.Outcome = VerifyFound
	result.Message = "The snippet is installed on " + target + " and nothing is blocking it."

	// The bundle loads either way, so this is not a blocked install — but the
	// first line is an inline script, and a page that refuses one loses every
	// event fired before the bundle arrives. Saying "nothing is blocking it"
	// there would be the wrong half of the truth.
	if csp := result.CSPHeader; csp != "" && !cspAllowsInline(csp) {
		result.Message = "The snippet is installed on " + target + " and collecting. " +
			"The first line is an inline script and this site's Content-Security-Policy refuses those, " +
			"so an event fired before the script finishes loading is still lost. " +
			"Add " + tracker.QueueStubHash + " to your script-src to allow just that line."
	}

	return result
}

// recentVisitWindow is how fresh a site's last visit must be for it to stand in
// for a snippet the check could not see.
const recentVisitWindow = 24 * time.Hour

// WithRecentVisit turns "not in the HTML" into a pass when the site recorded a
// visit recently.
//
// A tag manager, a script loaded by an SDK and some proxies put nothing in the
// HTML this server can recognise. Visits arriving is the stronger evidence, and
// telling a working install it has no snippet sends somebody to fix nothing.
func (v VerifyResult) WithRecentVisit(lastVisit, now time.Time) VerifyResult {
	if v.Outcome != VerifyMissing || lastVisit.IsZero() || now.Sub(lastVisit) > recentVisitWindow {
		return v
	}

	v.Outcome = VerifyCollecting
	v.Message = "The snippet isn't in the HTML of " + v.URL + ", which is normal with a tag manager or a proxy. " +
		"Visits are arriving, so it's installed and working."

	return v
}

// cspAllowsInline reports whether a policy would run the queueing stub.
//
// It is forgiving in the same way as cspAllows and for the same reason, with
// one addition: a policy carrying a nonce is treated as permissive, because
// whether the customer put that nonce on our tag is not something the HTML can
// answer — their template may add it at render time.
func cspAllowsInline(policy string) bool {
	sources, ok := scriptSources(policy)
	if !ok {
		return true
	}

	if strings.Contains(sources, strings.ToLower(tracker.QueueStubHash)) || strings.Contains(sources, "'nonce-") {
		return true
	}

	// A hash or a nonce anywhere in the directive makes a browser ignore
	// 'unsafe-inline' entirely, so it cannot be read on its own.
	return strings.Contains(sources, "'unsafe-inline'") && !strings.Contains(sources, "'sha")
}

// scriptSources pulls the directive that decides whether a script runs, and
// reports whether the policy names one at all.
func scriptSources(policy string) (string, bool) {
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

	return sources, ok
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
	sources, ok := scriptSources(policy)
	if !ok {
		return true
	}

	if strings.Contains(sources, "*") && !strings.Contains(sources, "'none'") {
		return true
	}

	// A nonce or 'strict-dynamic' policy decides per tag, which the HTML alone
	// cannot answer, and a bare scheme allows every host on it.
	if strings.Contains(sources, "'nonce-") || strings.Contains(sources, "'strict-dynamic'") {
		return true
	}

	// A host-only match is enough. Matching the scheme and port as well would
	// be more correct and would produce false alarms on every policy written
	// with a bare hostname, which is most of them.
	bare := normaliseHost(host)

	for _, source := range strings.Fields(sources) {
		if source == "https:" || normaliseHost(source) == bare {
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
