//
// channel.go
// The acquisition channel: a pure function of source, medium, campaign and click id.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import (
	"regexp"
	"strings"
)

// The nineteen channels. They are aligned with GA4's definitions so that
// somebody migrating sees the same numbers under the same names on day one;
// inventing our own taxonomy would make every comparison during a migration
// look like data loss.
const (
	ChannelAffiliates    = "Affiliates"
	ChannelAIAssistants  = "AI Assistants"
	ChannelAudio         = "Audio"
	ChannelCrossNetwork  = "Cross-network"
	ChannelDirect        = "Direct"
	ChannelDisplay       = "Display"
	ChannelEmail         = "Email"
	ChannelMobilePush    = "Mobile Push Notifications"
	ChannelOrganicSearch = "Organic Search"
	ChannelOrganicShop   = "Organic Shopping"
	ChannelOrganicSocial = "Organic Social"
	ChannelOrganicVideo  = "Organic Video"
	ChannelPaidOther     = "Paid Other"
	ChannelPaidSearch    = "Paid Search"
	ChannelPaidShopping  = "Paid Shopping"
	ChannelPaidSocial    = "Paid Social"
	ChannelPaidVideo     = "Paid Video"
	ChannelReferral      = "Referral"
	ChannelSMS           = "SMS"
)

// Channels is every channel, used by tests and by the dashboard's filter list.
var Channels = []string{
	ChannelAffiliates, ChannelAIAssistants, ChannelAudio, ChannelCrossNetwork,
	ChannelDirect, ChannelDisplay, ChannelEmail, ChannelMobilePush,
	ChannelOrganicSearch, ChannelOrganicShop, ChannelOrganicSocial,
	ChannelOrganicVideo, ChannelPaidOther, ChannelPaidSearch,
	ChannelPaidShopping, ChannelPaidSocial, ChannelPaidVideo, ChannelReferral,
	ChannelSMS,
}

// Click id parameter names. Their values are never stored — a click id is a
// unique per-click identifier and keeping it would be a personal identifier we
// have no consent for — but the parameter's presence is what separates a paid
// click from an organic one when the advertiser forgot their UTM tags.
const (
	ClickIDGoogle    = "gclid"
	ClickIDMicrosoft = "msclkid"
)

// paidMedium is GA4's own paid-medium pattern. The `.*cp.*` arm is what makes
// cpc, cpm, cpv, cpa and every house variant of them count as paid without
// anyone maintaining a list.
var paidMedium = regexp.MustCompile(`^(.*cp.*|ppc|retargeting|paid.*)$`)

// shoppingCampaign is GA4's pattern for a campaign name that announces itself as
// shopping. The awkward first arm is deliberate: it matches "shop" and
// "shopping" while excluding words that merely contain them.
var shoppingCampaign = regexp.MustCompile(`^(.*(([^a-df-z]|^)shop|shopping).*)$`)

// videoMedium catches any medium with "video" in it, which is how video
// campaigns are tagged in practice.
var videoMedium = regexp.MustCompile(`^(.*video.*)$`)

// Input is everything the channel decision reads. It is a struct because the
// function takes five strings that are trivially transposable, and a transposed
// pair here would silently mislabel a whole account's traffic.
type Input struct {
	// Source is the canonical source name, or Direct.
	Source string

	// Category is the source's category, which is what the channel rules
	// actually branch on.
	Category Category

	// Medium, Campaign and CampaignSource are the raw UTM values, exactly as
	// they were sent.
	Medium         string
	Campaign       string
	CampaignSource string

	// ClickIDParam is the name of the click-id parameter that was present, or
	// empty. Only the name; never the value.
	ClickIDParam string
}

// Channel derives the acquisition channel. It is a pure function with no state
// and no I/O, which is what lets the same code run at ingest and be re-run over
// stored columns when the rules change.
//
// The order of the checks is the algorithm. GA4 evaluates these rules in a
// fixed sequence and several of them overlap — a paid campaign from a search
// engine matches both Paid Search and Paid Other — so reordering them silently
// moves traffic between channels.
func Channel(in Input) string {
	source := strings.ToLower(strings.TrimSpace(in.Source))
	medium := strings.ToLower(strings.TrimSpace(in.Medium))
	campaign := strings.ToLower(strings.TrimSpace(in.Campaign))
	utmSource := strings.ToLower(strings.TrimSpace(in.CampaignSource))

	if source == "" {
		source = Direct
	}

	// Direct is the only rule that requires the absence of everything else, so
	// it has to be tested before anything that a blank medium could match.
	if source == Direct && (medium == "" || medium == "(not set)" || medium == "(none)") {
		return ChannelDirect
	}

	// A cross-network campaign spans several ad products at once, so it cannot
	// be attributed to any single one and GA4 gives it its own channel.
	if strings.Contains(campaign, "cross-network") {
		return ChannelCrossNetwork
	}

	paid := paidMedium.MatchString(medium)

	// A click id is proof of a paid click even when the campaign carries no
	// medium at all, which is the common case for auto-tagged advertising.
	switch in.ClickIDParam {
	case ClickIDGoogle:
		if in.Category == CategorySearch || source == "google" || utmSource == "google" {
			return ChannelPaidSearch
		}
	case ClickIDMicrosoft:
		if in.Category == CategorySearch || source == "bing" || utmSource == "bing" {
			return ChannelPaidSearch
		}
	}

	if paid {
		switch {
		case in.Category == CategoryShopping || shoppingCampaign.MatchString(campaign):
			return ChannelPaidShopping
		case in.Category == CategorySearch:
			return ChannelPaidSearch
		case in.Category == CategorySocial:
			return ChannelPaidSocial
		case in.Category == CategoryVideo:
			return ChannelPaidVideo
		}

		return ChannelPaidOther
	}

	// Display is a medium, not a source: the same ad network serves display and
	// search, and only the tag says which one this click was.
	switch medium {
	case "display", "banner", "expandable", "interstitial", "cpm":
		return ChannelDisplay
	}

	if medium == "affiliate" {
		return ChannelAffiliates
	}

	if source == "sms" || medium == "sms" || utmSource == "sms" {
		return ChannelSMS
	}

	if isMobilePush(medium) || utmSource == "firebase" {
		return ChannelMobilePush
	}

	// Email is checked by both source and medium because a native mail client
	// sends no referrer at all — the medium tag is the only evidence there is.
	if in.Category == CategoryEmail || isEmailToken(source) || isEmailToken(medium) || isEmailToken(utmSource) {
		return ChannelEmail
	}

	if in.Category == CategoryAI {
		return ChannelAIAssistants
	}

	if medium == "audio" || in.Category == CategoryAudio {
		return ChannelAudio
	}

	if in.Category == CategoryShopping || shoppingCampaign.MatchString(campaign) {
		return ChannelOrganicShop
	}

	if in.Category == CategorySocial || isSocialMedium(medium) {
		return ChannelOrganicSocial
	}

	if in.Category == CategoryVideo || videoMedium.MatchString(medium) {
		return ChannelOrganicVideo
	}

	if in.Category == CategorySearch || medium == "organic" {
		return ChannelOrganicSearch
	}

	// Everything left with a referrer is a referral. Reaching here with no
	// referrer at all means a medium was tagged but matched nothing, and Direct
	// is the honest answer for that.
	if source == Direct {
		return ChannelDirect
	}

	return ChannelReferral
}

// isEmailToken reports whether a token is one of the spellings of "email"
// people use in UTM tags. All five appear in the wild and treating them
// differently splits one newsletter across five rows.
func isEmailToken(token string) bool {
	switch token {
	case "email", "e-mail", "e_mail", "e mail", "gmail", "newsletter":
		return true
	}

	return false
}

// isSocialMedium reports whether a medium names social traffic. The short "sm"
// is included because it is what several marketing tools emit by default.
func isSocialMedium(medium string) bool {
	switch medium {
	case "social", "social-network", "social-media", "sm", "social network", "social media":
		return true
	}

	return false
}

// isMobilePush reports whether a medium names a push notification. The suffix
// test is what catches "app-push" and "web push" without listing every prefix
// somebody might put in front.
func isMobilePush(medium string) bool {
	if medium == "" {
		return false
	}

	return strings.HasSuffix(medium, "push") ||
		strings.Contains(medium, "mobile") ||
		strings.Contains(medium, "notification")
}
