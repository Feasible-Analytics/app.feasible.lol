//
// pipeline.go
// The derive pipeline, in the one order that is correct.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package ingest

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/geo"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/referrer"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/salts"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/sites"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/useragent"
)

// AcquisitionParams are the only query parameters we recognise and store. The
// list is closed because every other parameter is stripped from the stored
// path: a site that puts a session token or an email address in its query
// string must not have us keep it.
var AcquisitionParams = []string{
	"ref", "source", "utm_source", "utm_medium", "utm_campaign", "utm_content", "utm_term",
}

// clickIDParams are detected but never stored by value. A click id is a unique
// per-click identifier and is not GDPR-compliant to keep without consent, so
// only the parameter's *name* survives — which is all the channel rules need.
var clickIDParams = []string{referrer.ClickIDGoogle, referrer.ClickIDMicrosoft}

// IPShield decides whether a customer has blocked an address. It is an
// interface with a no-op default because the rule list belongs to the shard,
// but the check has to run in the ingest tier: this is the only place in the
// system where the raw IP still exists.
type IPShield interface {
	Blocked(siteID int64, addr netip.Addr) bool
}

// NoShield allows everything. It is the default so that an install with no
// shield rules configured costs one interface call rather than a nil check on
// the hot path.
type NoShield struct{}

// Blocked always allows.
func (NoShield) Blocked(int64, netip.Addr) bool { return false }

// Pipeline turns an HTTP request into a derived event. Everything it needs is
// injected, because every one of these is a different licensing, deployment or
// performance decision and none of them should be reachable from a call site.
type Pipeline struct {
	Sites    *sites.Cache
	Salts    *salts.Store
	Geo      geo.Locator
	Agents   *useragent.Cache
	Bots     *BotFilter
	Trusted  *TrustedProxies
	Shards   ShardResolver
	Shield   IPShield
	Counters *Counters

	// Now is injectable so a replay test can drive the pipeline at a fixed
	// instant rather than at whatever time the test suite happens to run.
	Now func() time.Time
}

// Debug is what X-Debug-Request returns: the resolved IP and every derived
// field. It exists so anyone can debug their proxy in one curl, rather than
// filing a ticket about numbers that look wrong for a reason only we can see.
type Debug struct {
	ClientIP       string `json:"client_ip"`
	ClientIPSource string `json:"client_ip_source"`
	TrustedProxy   bool   `json:"trusted_proxy_configured"`

	Domain    string `json:"domain"`
	SiteID    int64  `json:"site_id"`
	AccountID int64  `json:"account_id"`
	Shard     int    `json:"shard"`

	EventName string `json:"event_name"`
	Timestamp int64  `json:"timestamp"`

	UserID         int64  `json:"user_id"`
	PreviousUserID int64  `json:"previous_user_id"`
	RootDomain     string `json:"root_domain"`
	SaltDay        int64  `json:"salt_day"`

	Hostname  string `json:"hostname"`
	Pathname  string `json:"pathname"`
	PageTitle string `json:"page_title"`

	Referrer     string `json:"referrer"`
	Source       string `json:"source"`
	Channel      string `json:"channel"`
	UTMSource    string `json:"utm_source"`
	UTMMedium    string `json:"utm_medium"`
	UTMCampaign  string `json:"utm_campaign"`
	UTMContent   string `json:"utm_content"`
	UTMTerm      string `json:"utm_term"`
	ClickIDParam string `json:"click_id_param"`

	Country      string `json:"country"`
	Region       string `json:"region"`
	Subdivision2 string `json:"subdivision2"`
	City         string `json:"city"`

	DeviceType     string `json:"device_type"`
	Browser        string `json:"browser"`
	BrowserVersion string `json:"browser_version"`
	OS             string `json:"os"`
	OSVersion      string `json:"os_version"`
	Language       string `json:"language"`

	ScrollDepth    int   `json:"scroll_depth"`
	EngagementTime int64 `json:"engagement_time"`
	Interactive    bool  `json:"interactive"`

	BotReason  string     `json:"bot_reason"`
	DropReason string     `json:"drop_reason"`
	Truncation Truncation `json:"truncation"`
}

// Result is what the derive step produced: an event to write, a reason to drop
// it, or both — a classified event is still written.
type Result struct {
	Event      *Event
	DropReason string
	Truncation Truncation
	Debug      Debug
}

// Derive runs the whole pipeline in the order that is correct, which is not the
// order that is convenient. The sequence below is the specification:
//
//  2. resolve the client IP
//  3. bot filter — user agent, datacentre ranges, referrer spam
//  4. build the debug view
//  5. IP shield rules — here, because it is the last place the raw IP exists
//  6. parse the user agent
//  7. geolocate, then discard the IP
//  8. compute the fingerprint
//  9. parse the URL and the acquisition parameters
//  10. parse the referrer
//  11. derive the channel
//
// Steps 1 and 12 onward belong to the handler and the writer. Hostname
// validation, country shields and page shields deliberately do not happen here:
// they run at the shard, where the settings are live. The governing rule is
// that the ingest tier holds only what needs the raw IP, and everything else
// stays where it is current.
func (p *Pipeline) Derive(ctx context.Context, r *http.Request, payload *Payload) (Result, error) {
	var result Result

	// Step 2. The single highest-leverage configuration in the system, and the
	// one that fails silently behind a 202 in every direction.
	client := ResolveClientIP(r, p.Trusted)
	clientIP := client.String()

	rawUserAgent := r.Header.Get("User-Agent")

	result.Debug.ClientIP = clientIP
	result.Debug.ClientIPSource = client.Source
	result.Debug.TrustedProxy = !p.Trusted.Empty()
	result.Debug.Domain = payload.Domain
	result.Debug.EventName = payload.Name

	// The referrer is parsed early because the spam check needs its host, but
	// the result is reused at step 10 rather than parsed twice.
	referrerInput := payload.Referrer
	if payload.OverrideReferrer != "" {
		// Unlike the incumbent we allow a server-side caller to state the
		// attribution, because a delayed or offline conversion has no referrer
		// of its own and would otherwise be Direct forever.
		referrerInput = payload.OverrideReferrer
	}

	// Step 9, in part: the hostname is needed by the referrer parser to tell an
	// internal link from an acquisition.
	pageURL, hostname, pathname, params, urlTruncated := parseEventURL(payload.URL)
	result.Truncation.URLTruncated = urlTruncated

	source := referrer.Parse(referrerInput, hostname)

	// Step 3. Classification, not deletion: the row is still written with its
	// reason attached, and the customer gets a toggle. Deleting it means a
	// wrongly-classified visitor is gone forever, and a self-hoster is frozen
	// at whatever list their build shipped with.
	botReason := p.classify(rawUserAgent, client.Addr, source.Referrer)
	result.Debug.BotReason = botReason

	site, known := p.Sites.Lookup(payload.Domain)
	if !known {
		result.DropReason = ReasonUnknownSite
		result.Debug.DropReason = result.DropReason
		return result, nil
	}

	result.Debug.SiteID = site.ID
	result.Debug.AccountID = site.AccountID

	now := p.clock()

	// A lapsed account keeps sending until its grace period runs out, because
	// dropping a paying customer's traffic the instant a card fails loses data
	// they can never get back.
	if site.AcceptTrafficUntil > 0 && now.Unix() > site.AcceptTrafficUntil {
		result.DropReason = ReasonAccountDormant
		result.Debug.DropReason = result.DropReason
		return result, nil
	}

	shard, routed := p.Shards.Shard(site.AccountID)
	if !routed {
		// Not in the routing map means drop, by design: shards are the source
		// of truth for what they own, and forwarding to a shard that does not
		// hold the account would write the data into the wrong file.
		result.DropReason = ReasonSiteDeleted
		result.Debug.DropReason = result.DropReason
		return result, nil
	}
	result.Debug.Shard = shard

	// Step 5. The customer's blocked-IP list has to run here and nowhere else.
	if p.Shield != nil && p.Shield.Blocked(site.ID, client.Addr) {
		result.DropReason = ReasonShieldIP
		result.Debug.DropReason = result.DropReason
		return result, nil
	}

	// Step 6.
	agent := p.Agents.Parse(rawUserAgent)

	// Step 7. Geolocate, and then the address is gone: nothing below this line
	// may reference client.Addr, and nothing on the Event has anywhere to put
	// it. The IP address never reaches disk.
	location := p.locate(client.Addr, botReason)

	// Step 8. The fingerprint, and the two things about it that can never be
	// changed: a bare concatenation with no separators, and the registrable
	// domain as the fourth term.
	pair, err := p.Salts.Pair(ctx)
	if err != nil {
		return result, err
	}

	rootDomain := RootDomain(hostname)
	userID := Fingerprint(pair.Current, rawUserAgent, clientIP, payload.Domain, rootDomain)

	var previousUserID int64
	if len(pair.Previous) == salts.Size {
		previousUserID = Fingerprint(pair.Previous, rawUserAgent, clientIP, payload.Domain, rootDomain)
	}

	props, propTruncation, err := ParseProps(payload.Props)
	if err != nil {
		return result, err
	}
	result.Truncation.PropsDropped = propTruncation.PropsDropped
	result.Truncation.PropNamesTruncated = propTruncation.PropNamesTruncated
	result.Truncation.PropValuesTruncated = propTruncation.PropValuesTruncated

	revenue, err := ParseRevenue(payload.Revenue)
	if err != nil {
		return result, err
	}

	engagement, clamped := payload.EngagementTime()
	result.Truncation.EngagementClamped = clamped

	// Steps 10 and 11. The acquisition tags win over the referrer when they are
	// present, because somebody set them deliberately.
	acquisition := resolveAcquisition(params, payload, source)

	event := &Event{
		UUID:      uuid.New(),
		Shard:     shard,
		AccountID: site.AccountID,
		SiteID:    site.ID,
		Timestamp: now.Unix(),

		Name:           payload.Name,
		UserID:         userID,
		PreviousUserID: previousUserID,

		Hostname:  hostname,
		Pathname:  pathname,
		PageTitle: strings.TrimSpace(payload.Title),

		Referrer:     source.Referrer,
		Source:       acquisition.source,
		Channel:      acquisition.channel,
		UTMSource:    acquisition.utmSource,
		UTMMedium:    acquisition.utmMedium,
		UTMCampaign:  acquisition.utmCampaign,
		UTMContent:   acquisition.utmContent,
		UTMTerm:      acquisition.utmTerm,
		ClickIDParam: acquisition.clickID,

		Country: location.Country,
		Region:  location.Subdivision1,
		City:    location.City,

		DeviceType:     agent.Device,
		Browser:        agent.Browser,
		BrowserVersion: agent.BrowserVersion,
		OS:             agent.OS,
		OSVersion:      agent.OSVersion,
		Language:       primaryLanguage(r.Header.Get("Accept-Language")),

		ScrollDepth:    payload.ScrollDepth(),
		EngagementTime: engagement,

		BotReason:   botReason,
		Interactive: payload.IsInteractive(),

		Props:   props,
		Revenue: revenue,
	}

	result.Event = event
	fillDebug(&result.Debug, event, pair.Day, rootDomain, location.Subdivision2)
	result.Debug.Truncation = result.Truncation

	// pageURL is kept only so the debug view can show what we parsed. It is not
	// stored: full-URL capture is an opt-in setting that lives at the shard.
	_ = pageURL

	return result, nil
}

// clock returns the pipeline's time source, defaulting to the real one so a
// caller that forgets to set it still works.
func (p *Pipeline) clock() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}

	return p.Now().UTC()
}

// classify runs the three bot lists in the order that costs least. A user-agent
// substring scan is cheaper than a binary search over thirty thousand ranges,
// and both are cheaper than the referrer lookup that has to resolve a domain.
func (p *Pipeline) classify(userAgent string, addr netip.Addr, referrerHost string) string {
	if p.Bots == nil {
		return ""
	}

	if p.Bots.IsBotUserAgent(userAgent) {
		return ReasonBot
	}

	if p.Bots.IsDatacenterIP(addr) {
		return ReasonDatacenterIP
	}

	if host, _, found := strings.Cut(referrerHost, "/"); found || referrerHost != "" {
		if p.Bots.IsReferrerSpam(host) {
			return ReasonReferrerSpam
		}
	}

	return ""
}

// locate geolocates an address, bucketing datacentre traffic separately.
// Commercial VPN exits are datacentre addresses, so geolocating them reports
// the exit node's country rather than the visitor's — naming the bucket keeps
// the visitor counted and tells the truth about what we know.
func (p *Pipeline) locate(addr netip.Addr, botReason string) geo.Location {
	if p.Geo == nil {
		return geo.Location{}
	}

	if botReason == ReasonDatacenterIP {
		return geo.Location{Country: geo.AnonymousVPNCountry}
	}

	return p.Geo.Lookup(addr)
}

// acquisition is the resolved campaign attribution for one event.
type acquisition struct {
	source      string
	channel     string
	utmSource   string
	utmMedium   string
	utmCampaign string
	utmContent  string
	utmTerm     string
	clickID     string
}

// resolveAcquisition combines the query parameters, the server-side overrides
// and the referrer into a source and a channel. A UTM source wins over the
// referrer because somebody typed it on purpose, and a campaign that says it is
// Facebook is Facebook even when the click came through a link shortener.
func resolveAcquisition(params url.Values, payload *Payload, source referrer.Result) acquisition {
	out := acquisition{
		source:      source.Source,
		utmSource:   firstNonEmpty(payload.OverrideUTMSource, params.Get("utm_source"), params.Get("source"), params.Get("ref")),
		utmMedium:   firstNonEmpty(payload.OverrideUTMMedium, params.Get("utm_medium")),
		utmCampaign: firstNonEmpty(payload.OverrideUTMCampaign, params.Get("utm_campaign")),
		utmContent:  firstNonEmpty(payload.OverrideUTMContent, params.Get("utm_content")),
		utmTerm:     firstNonEmpty(payload.OverrideUTMTerm, params.Get("utm_term")),
	}

	for _, name := range clickIDParams {
		if params.Get(name) != "" {
			out.clickID = name
			break
		}
	}

	category := source.Category

	if out.utmSource != "" {
		if resolved, ok := referrer.SourceFromUTM(out.utmSource); ok {
			out.source = resolved.Name
			category = resolved.Category
		}
	}

	// Direct is a real row rather than an omission. It is usually the largest
	// bucket, and leaving it out makes every total on the page look wrong.
	if out.source == "" {
		out.source = referrer.Direct
	}

	out.channel = referrer.Channel(referrer.Input{
		Source:         out.source,
		Category:       category,
		Medium:         out.utmMedium,
		Campaign:       out.utmCampaign,
		CampaignSource: out.utmSource,
		ClickIDParam:   out.clickID,
	})

	// The stored source is empty for direct traffic, so that the dimension
	// table's id 0 — the empty string, which every schema column already
	// defaults to — is the Direct row rather than a second synonym for it.
	if out.source == referrer.Direct {
		out.source = ""
	}

	return out
}

// parseEventURL splits the event URL into the parts that are stored and the
// parts that are read and thrown away. Every query parameter except the seven
// acquisition ones is stripped from the stored path, because a site that puts
// an email address or a session token in its query string must not have us keep
// it — and the customer cannot un-store it afterwards.
func parseEventURL(raw string) (parsed *url.URL, hostname, pathname string, params url.Values, truncated bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil {
		return nil, NoneHostname, "/", url.Values{}, false
	}

	hostname = strings.ToLower(parsed.Hostname())
	if hostname == "" {
		// A URL with no host still has to hash and group consistently, and an
		// empty string there would collide with a genuinely missing hostname.
		hostname = NoneHostname
	}
	hostname = strings.TrimPrefix(hostname, "www.")

	pathname = parsed.EscapedPath()
	if pathname == "" {
		pathname = "/"
	}

	// A trailing slash on anything but the root splits one page into two rows
	// on every report.
	if len(pathname) > 1 {
		pathname = strings.TrimSuffix(pathname, "/")
	}

	// The limit is on the path, excluding the domain and the query string,
	// because the path is what grows without bound.
	if len(pathname) > MaxURLLength {
		pathname = pathname[:MaxURLLength]
		truncated = true
	}

	// url.Query already URI-decodes, which is what makes
	// utm_source=Android%20App display as "Android App".
	params = parsed.Query()

	return parsed, hostname, pathname, params, truncated
}

// primaryLanguage takes the first tag from an Accept-Language header. The
// quality-ordered list is the browser's preference order, so the first entry is
// the answer and the rest is noise on a dashboard.
func primaryLanguage(header string) string {
	if header == "" {
		return ""
	}

	first, _, _ := strings.Cut(header, ",")
	first, _, _ = strings.Cut(first, ";")
	first = strings.TrimSpace(first)

	// A malformed header should not become a dimension value that never
	// repeats, which is how a dim table grows one row per request.
	if len(first) > 35 {
		return ""
	}

	return first
}

// firstNonEmpty returns the first value that is set. It is what implements the
// precedence between a server-side override, a UTM tag and its short aliases.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}

	return ""
}

// fillDebug copies a derived event into the debug view. It is separate so that
// the hot path builds it only when X-Debug-Request asked for it, and so that a
// new field on the event is one line here rather than a silent omission.
func fillDebug(debug *Debug, event *Event, saltDay int64, rootDomain, subdivision2 string) {
	debug.Timestamp = event.Timestamp
	debug.UserID = event.UserID
	debug.PreviousUserID = event.PreviousUserID
	debug.RootDomain = rootDomain
	debug.SaltDay = saltDay

	debug.Hostname = event.Hostname
	debug.Pathname = event.Pathname
	debug.PageTitle = event.PageTitle

	debug.Referrer = event.Referrer
	debug.Source = event.Source
	debug.Channel = event.Channel
	debug.UTMSource = event.UTMSource
	debug.UTMMedium = event.UTMMedium
	debug.UTMCampaign = event.UTMCampaign
	debug.UTMContent = event.UTMContent
	debug.UTMTerm = event.UTMTerm
	debug.ClickIDParam = event.ClickIDParam

	debug.Country = event.Country
	debug.Region = event.Region
	debug.Subdivision2 = subdivision2
	debug.City = event.City

	debug.DeviceType = event.DeviceType
	debug.Browser = event.Browser
	debug.BrowserVersion = event.BrowserVersion
	debug.OS = event.OS
	debug.OSVersion = event.OSVersion
	debug.Language = event.Language

	debug.ScrollDepth = event.ScrollDepth
	debug.EngagementTime = event.EngagementTime
	debug.Interactive = event.Interactive
}
