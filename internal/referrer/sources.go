//
// sources.go
// The referrer-host to canonical-source map, and what kind of site each one is.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import "strings"

// Category is what kind of site a referrer is. It exists because the acquisition
// channel is a function of the category rather than the name: every search
// engine behaves the same way in the channel rules, and enumerating them by name
// in the channel function would put the same list in two places.
type Category int

// The categories. Unknown is the common case — most referrers are somebody's
// blog — and it maps to the Referral channel.
const (
	CategoryUnknown Category = iota
	CategorySearch
	CategorySocial
	CategoryVideo
	CategoryShopping
	CategoryEmail
	CategoryAI
	CategoryAudio
)

// Source is one resolved referrer: the name a report shows and the category the
// channel rules read.
type Source struct {
	Name     string
	Category Category
}

// hosts maps a referrer hostname to its canonical source. This is our own
// compilation, written from the public shape of these products rather than
// lifted from anyone's file: the incumbent's equivalent is AGPL and
// hand-curated, and a hand-curated list is a copyrightable compilation whatever
// the surrounding licence says.
//
// Keys are stored without a leading "www." and lower-cased, because that is how
// they are looked up. A hostname that is not here falls back to its registrable
// domain, and then to the hostname itself, so an unknown referrer is still a
// usable row rather than a blank.
var hosts = map[string]Source{
	// Search.
	"google.com":          {"Google", CategorySearch},
	"bing.com":            {"Bing", CategorySearch},
	"duckduckgo.com":      {"DuckDuckGo", CategorySearch},
	"yahoo.com":           {"Yahoo!", CategorySearch},
	"search.yahoo.com":    {"Yahoo!", CategorySearch},
	"yandex.ru":           {"Yandex", CategorySearch},
	"yandex.com":          {"Yandex", CategorySearch},
	"baidu.com":           {"Baidu", CategorySearch},
	"ecosia.org":          {"Ecosia", CategorySearch},
	"search.brave.com":    {"Brave Search", CategorySearch},
	"startpage.com":       {"Startpage", CategorySearch},
	"qwant.com":           {"Qwant", CategorySearch},
	"naver.com":           {"Naver", CategorySearch},
	"seznam.cz":           {"Seznam", CategorySearch},
	"ask.com":             {"Ask", CategorySearch},
	"aol.com":             {"AOL", CategorySearch},
	"kagi.com":            {"Kagi", CategorySearch},
	"mojeek.com":          {"Mojeek", CategorySearch},
	"lite.duckduckgo.com": {"DuckDuckGo", CategorySearch},

	// Social.
	"facebook.com":         {"Facebook", CategorySocial},
	"m.facebook.com":       {"Facebook", CategorySocial},
	"l.facebook.com":       {"Facebook", CategorySocial},
	"lm.facebook.com":      {"Facebook", CategorySocial},
	"instagram.com":        {"Instagram", CategorySocial},
	"l.instagram.com":      {"Instagram", CategorySocial},
	"threads.net":          {"Threads", CategorySocial},
	"threads.com":          {"Threads", CategorySocial},
	"twitter.com":          {"X", CategorySocial},
	"t.co":                 {"X", CategorySocial},
	"x.com":                {"X", CategorySocial},
	"linkedin.com":         {"LinkedIn", CategorySocial},
	"lnkd.in":              {"LinkedIn", CategorySocial},
	"reddit.com":           {"Reddit", CategorySocial},
	"old.reddit.com":       {"Reddit", CategorySocial},
	"out.reddit.com":       {"Reddit", CategorySocial},
	"news.ycombinator.com": {"Hacker News", CategorySocial},
	"pinterest.com":        {"Pinterest", CategorySocial},
	"tumblr.com":           {"Tumblr", CategorySocial},
	"mastodon.social":      {"Mastodon", CategorySocial},
	"bsky.app":             {"Bluesky", CategorySocial},
	"t.me":                 {"Telegram", CategorySocial},
	"telegram.org":         {"Telegram", CategorySocial},
	"discord.com":          {"Discord", CategorySocial},
	"slack.com":            {"Slack", CategorySocial},
	"vk.com":               {"VK", CategorySocial},
	"weibo.com":            {"Weibo", CategorySocial},
	"quora.com":            {"Quora", CategorySocial},
	"medium.com":           {"Medium", CategorySocial},
	"substack.com":         {"Substack", CategorySocial},
	"whatsapp.com":         {"WhatsApp", CategorySocial},
	"messenger.com":        {"Facebook Messenger", CategorySocial},
	"snapchat.com":         {"Snapchat", CategorySocial},
	"tiktok.com":           {"TikTok", CategorySocial},
	"github.com":           {"GitHub", CategorySocial},

	// Video.
	"youtube.com":     {"YouTube", CategoryVideo},
	"m.youtube.com":   {"YouTube", CategoryVideo},
	"youtu.be":        {"YouTube", CategoryVideo},
	"vimeo.com":       {"Vimeo", CategoryVideo},
	"twitch.tv":       {"Twitch", CategoryVideo},
	"dailymotion.com": {"Dailymotion", CategoryVideo},

	// Shopping.
	"amazon.com":          {"Amazon", CategoryShopping},
	"ebay.com":            {"eBay", CategoryShopping},
	"etsy.com":            {"Etsy", CategoryShopping},
	"shopify.com":         {"Shopify", CategoryShopping},
	"walmart.com":         {"Walmart", CategoryShopping},
	"alibaba.com":         {"Alibaba", CategoryShopping},
	"aliexpress.com":      {"AliExpress", CategoryShopping},
	"shopping.google.com": {"Google Shopping", CategoryShopping},

	// Email. Web mail clients send a referrer; native clients usually do not,
	// which is why the Email channel also keys off utm_medium.
	"mail.google.com":       {"Gmail", CategoryEmail},
	"outlook.live.com":      {"Outlook", CategoryEmail},
	"outlook.office.com":    {"Outlook", CategoryEmail},
	"outlook.office365.com": {"Outlook", CategoryEmail},
	"mail.yahoo.com":        {"Yahoo! Mail", CategoryEmail},
	"mail.proton.me":        {"Proton Mail", CategoryEmail},

	// AI assistants. A newer channel than the rest and growing fast enough that
	// folding it into Referral would hide the fastest-moving line on the report.
	"chatgpt.com":           {"ChatGPT", CategoryAI},
	"chat.openai.com":       {"ChatGPT", CategoryAI},
	"claude.ai":             {"Claude", CategoryAI},
	"perplexity.ai":         {"Perplexity", CategoryAI},
	"www.perplexity.ai":     {"Perplexity", CategoryAI},
	"gemini.google.com":     {"Gemini", CategoryAI},
	"bard.google.com":       {"Gemini", CategoryAI},
	"copilot.microsoft.com": {"Microsoft Copilot", CategoryAI},
	"copilot.com":           {"Microsoft Copilot", CategoryAI},
	"you.com":               {"You.com", CategoryAI},
	"poe.com":               {"Poe", CategoryAI},

	// Audio.
	"open.spotify.com":   {"Spotify", CategoryAudio},
	"spotify.com":        {"Spotify", CategoryAudio},
	"podcasts.apple.com": {"Apple Podcasts", CategoryAudio},
}

// secondLevel maps the label immediately before the public suffix onto a source,
// which is what makes one entry cover a global company's whole country-domain
// estate. Google alone runs nearly two hundred of them, and google.co.uk shares
// no registrable domain with google.com, so the host map cannot reach it.
var secondLevel = map[string]Source{
	"google":     {"Google", CategorySearch},
	"bing":       {"Bing", CategorySearch},
	"yahoo":      {"Yahoo!", CategorySearch},
	"yandex":     {"Yandex", CategorySearch},
	"baidu":      {"Baidu", CategorySearch},
	"ecosia":     {"Ecosia", CategorySearch},
	"duckduckgo": {"DuckDuckGo", CategorySearch},
	"startpage":  {"Startpage", CategorySearch},
	"qwant":      {"Qwant", CategorySearch},
	"amazon":     {"Amazon", CategoryShopping},
	"ebay":       {"eBay", CategoryShopping},
}

// androidPackages maps an Android application id to a source. Android in-app
// browsers send `android-app://<package>` as the referrer, which is not a URL
// any host lookup can resolve, so without this every in-app click lands in
// Direct — the single largest source of "where did my social traffic go".
var androidPackages = map[string]Source{
	"com.google.android.gm":                   {"Gmail", CategoryEmail},
	"com.google.android.googlequicksearchbox": {"Google", CategorySearch},
	"com.google.android.apps.bard":            {"Gemini", CategoryAI},
	"com.android.chrome":                      {"Google", CategorySearch},
	"com.facebook.katana":                     {"Facebook", CategorySocial},
	"com.facebook.orca":                       {"Facebook Messenger", CategorySocial},
	"com.facebook.lite":                       {"Facebook", CategorySocial},
	"com.instagram.android":                   {"Instagram", CategorySocial},
	"com.twitter.android":                     {"X", CategorySocial},
	"com.linkedin.android":                    {"LinkedIn", CategorySocial},
	"com.reddit.frontpage":                    {"Reddit", CategorySocial},
	"com.pinterest":                           {"Pinterest", CategorySocial},
	"com.zhiliaoapp.musically":                {"TikTok", CategorySocial},
	"com.snapchat.android":                    {"Snapchat", CategorySocial},
	"org.telegram.messenger":                  {"Telegram", CategorySocial},
	"com.whatsapp":                            {"WhatsApp", CategorySocial},
	"com.discord":                             {"Discord", CategorySocial},
	"com.google.android.youtube":              {"YouTube", CategoryVideo},
	"com.microsoft.office.outlook":            {"Outlook", CategoryEmail},
	"com.openai.chatgpt":                      {"ChatGPT", CategoryAI},
	"com.anthropic.claude":                    {"Claude", CategoryAI},
}

// utmSourceAliases folds the naming variants people type into UTM tags onto one
// canonical name. `facebook`, `fb` and `facebook-ads` are the same company and
// belong in one row on the Sources tab; Channels and Campaigns keep them apart,
// because consolidation is a display concern and the raw tag is what somebody
// set deliberately.
var utmSourceAliases = map[string]Source{
	"fb":           {"Facebook", CategorySocial},
	"facebook":     {"Facebook", CategorySocial},
	"facebook-ads": {"Facebook", CategorySocial},
	"facebook_ads": {"Facebook", CategorySocial},
	"meta":         {"Facebook", CategorySocial},
	"ig":           {"Instagram", CategorySocial},
	"instagram":    {"Instagram", CategorySocial},
	"twitter":      {"X", CategorySocial},
	"x":            {"X", CategorySocial},
	"linkedin":     {"LinkedIn", CategorySocial},
	"reddit":       {"Reddit", CategorySocial},
	"pinterest":    {"Pinterest", CategorySocial},
	"tiktok":       {"TikTok", CategorySocial},
	"google":       {"Google", CategorySearch},
	"google-ads":   {"Google", CategorySearch},
	"googleads":    {"Google", CategorySearch},
	"adwords":      {"Google", CategorySearch},
	"bing":         {"Bing", CategorySearch},
	"microsoft":    {"Bing", CategorySearch},
	"duckduckgo":   {"DuckDuckGo", CategorySearch},
	"yahoo":        {"Yahoo!", CategorySearch},
	"youtube":      {"YouTube", CategoryVideo},
	"newsletter":   {"Newsletter", CategoryEmail},
	"email":        {"Email", CategoryEmail},
	"e-mail":       {"Email", CategoryEmail},
	"e_mail":       {"Email", CategoryEmail},
	"e mail":       {"Email", CategoryEmail},
	"gmail":        {"Gmail", CategoryEmail},
	"chatgpt":      {"ChatGPT", CategoryAI},
	"openai":       {"ChatGPT", CategoryAI},
	"claude":       {"Claude", CategoryAI},
	"perplexity":   {"Perplexity", CategoryAI},
	"amazon":       {"Amazon", CategoryShopping},
	"spotify":      {"Spotify", CategoryAudio},
}

// hostsByName is the reverse of `hosts`: a canonical source name to the shortest
// hostname that resolves to it. It is built once at start-up rather than
// searched linearly, because the favicon proxy asks this question once per
// distinct source on every dashboard load.
//
// Shortest wins so that a source with several hostnames — google.com,
// google.co.uk, www.google.de — resolves to the one that is actually the
// company's front door, which is also the one whose icon exists.
var hostsByName = buildHostsByName()

// categoriesByName is the reverse category lookup used when an import already
// carries a canonical source label but not the referrer URL that originally
// produced it. Both source tables contribute because UTM-only names such as
// Newsletter may never appear in the hostname table.
var categoriesByName = buildCategoriesByName()

// buildCategoriesByName indexes every canonical source name case-insensitively.
// Repeated canonical names carry the same category in both source tables, so
// assignment order cannot change the answer.
func buildCategoriesByName() map[string]Category {
	categories := make(map[string]Category, len(hosts)+len(utmSourceAliases))

	for _, source := range hosts {
		key := strings.ToLower(source.Name)
		categories[key] = source.Category
	}

	for _, source := range utmSourceAliases {
		key := strings.ToLower(source.Name)
		categories[key] = source.Category
	}

	// Plausible exports a few canonical display labels that differ from the
	// shorter labels Feasible uses. Keeping them here lets an aggregate export
	// retain the category Plausible assigned before the original referrer was
	// discarded.
	categories["brave"] = CategorySearch
	categories["google gemini"] = CategoryAI
	categories["x (twitter)"] = CategorySocial

	return categories
}

// CategoryForSource resolves the category of an already-canonical source
// label. Unknown labels deliberately return CategoryUnknown, which the channel
// classifier treats as ordinary referral traffic rather than dropping it.
func CategoryForSource(name string) Category {
	name = strings.ToLower(strings.TrimSpace(name))
	if category, ok := categoriesByName[name]; ok {
		return category
	}

	// Plausible sometimes exports a hostname in utm_source. Resolve that shape
	// through the same host table used by native ingestion so an explicit tag
	// such as chatgpt.com does not erase the AI category carried by its source.
	if source, ok := lookupHost(normaliseHost(name)); ok {
		return source.Category
	}

	return CategoryUnknown
}

// buildHostsByName inverts the source table.
func buildHostsByName() map[string]string {
	byName := make(map[string]string, len(hosts))

	for host, source := range hosts {
		existing, seen := byName[source.Name]
		if !seen || len(host) < len(existing) || (len(host) == len(existing) && host < existing) {
			byName[source.Name] = host
		}
	}

	return byName
}

// HostFor resolves a source name back to a hostname an icon can be fetched for.
//
// A report groups by the canonical source name — "Hacker News", not
// "news.ycombinator.com" — so anything wanting an icon for a row has only the
// display name to work from. Names that are not in the table are usually a
// hostname already, because that is what an unrecognised referrer falls back
// to, and those are returned unchanged.
//
// It returns an empty string for a name that is neither, which is the honest
// answer for "Direct" or a channel name: there is no site to fetch an icon from.
func HostFor(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	if host, ok := hostsByName[name]; ok {
		return host
	}

	lowered := strings.ToLower(name)
	if host, ok := hostsByName[lowered]; ok {
		return host
	}

	// An unrecognised referrer is stored as its own hostname, so a name with a
	// dot in it and nothing else in it is one.
	if strings.Contains(lowered, ".") && !strings.ContainsAny(lowered, " /?#@:") {
		return strings.TrimPrefix(lowered, "www.")
	}

	return ""
}
