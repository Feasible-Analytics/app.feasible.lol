//
// channel_test.go
// Tests for the nineteen acquisition channels.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import "testing"

// TestChannel walks the definitions that matter. The channel is a pure function
// of five strings, and the order the rules are evaluated in *is* the algorithm —
// several of them overlap, so reordering them silently moves traffic between
// channels with nothing to show for it.
func TestChannel(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{
			// Direct requires the absence of everything else, which is why it
			// is tested before anything a blank medium could match.
			name: "no referrer and no tags",
			in:   Input{Source: Direct},
			want: ChannelDirect,
		},
		{
			name: "direct with an explicitly unset medium",
			in:   Input{Source: Direct, Medium: "(none)"},
			want: ChannelDirect,
		},
		{
			name: "a search engine with no medium",
			in:   Input{Source: "Google", Category: CategorySearch},
			want: ChannelOrganicSearch,
		},
		{
			name: "an organic medium with no known source",
			in:   Input{Source: "somebody.example", Medium: "organic"},
			want: ChannelOrganicSearch,
		},
		{
			name: "a search engine with a paid medium",
			in:   Input{Source: "Google", Category: CategorySearch, Medium: "cpc"},
			want: ChannelPaidSearch,
		},
		{
			// Auto-tagged advertising often carries no medium at all, so the
			// click id is the only evidence the click was paid.
			name: "google with a gclid and no medium",
			in:   Input{Source: "Google", Category: CategorySearch, ClickIDParam: ClickIDGoogle},
			want: ChannelPaidSearch,
		},
		{
			name: "bing with an msclkid",
			in:   Input{Source: "Bing", Category: CategorySearch, ClickIDParam: ClickIDMicrosoft},
			want: ChannelPaidSearch,
		},
		{
			name: "a social site with no medium",
			in:   Input{Source: "Facebook", Category: CategorySocial},
			want: ChannelOrganicSocial,
		},
		{
			name: "a social medium with an unknown source",
			in:   Input{Source: "somebody.example", Medium: "social-media"},
			want: ChannelOrganicSocial,
		},
		{
			name: "the short social medium some tools emit",
			in:   Input{Source: "somebody.example", Medium: "sm"},
			want: ChannelOrganicSocial,
		},
		{
			name: "a social site with a paid medium",
			in:   Input{Source: "Facebook", Category: CategorySocial, Medium: "paid-social"},
			want: ChannelPaidSocial,
		},
		{
			name: "a video site",
			in:   Input{Source: "YouTube", Category: CategoryVideo},
			want: ChannelOrganicVideo,
		},
		{
			name: "a video site with a paid medium",
			in:   Input{Source: "YouTube", Category: CategoryVideo, Medium: "cpv"},
			want: ChannelPaidVideo,
		},
		{
			name: "a shopping site",
			in:   Input{Source: "Amazon", Category: CategoryShopping},
			want: ChannelOrganicShop,
		},
		{
			name: "a campaign that announces itself as shopping",
			in:   Input{Source: "somebody.example", Campaign: "spring-shop-2026"},
			want: ChannelOrganicShop,
		},
		{
			name: "a paid shopping campaign",
			in:   Input{Source: "somebody.example", Campaign: "shopping-feed", Medium: "cpc"},
			want: ChannelPaidShopping,
		},
		{
			// A native mail client sends no referrer at all, so the medium tag
			// is the only evidence there is.
			name: "an email medium with no referrer",
			in:   Input{Source: Direct, Medium: "email"},
			want: ChannelEmail,
		},
		{
			name: "the underscore spelling of email",
			in:   Input{Source: Direct, Medium: "e_mail"},
			want: ChannelEmail,
		},
		{
			name: "a webmail referrer",
			in:   Input{Source: "Gmail", Category: CategoryEmail},
			want: ChannelEmail,
		},
		{
			name: "an AI assistant",
			in:   Input{Source: "ChatGPT", Category: CategoryAI},
			want: ChannelAIAssistants,
		},
		{
			name: "a display medium",
			in:   Input{Source: "somebody.example", Medium: "banner"},
			want: ChannelDisplay,
		},
		{
			name: "an affiliate",
			in:   Input{Source: "somebody.example", Medium: "affiliate"},
			want: ChannelAffiliates,
		},
		{
			name: "sms",
			in:   Input{Source: Direct, Medium: "sms"},
			want: ChannelSMS,
		},
		{
			name: "a push notification",
			in:   Input{Source: Direct, Medium: "app-push"},
			want: ChannelMobilePush,
		},
		{
			name: "audio",
			in:   Input{Source: "somebody.example", Medium: "audio"},
			want: ChannelAudio,
		},
		{
			name: "a cross-network campaign",
			in:   Input{Source: "Google", Category: CategorySearch, Campaign: "pmax-cross-network"},
			want: ChannelCrossNetwork,
		},
		{
			name: "a paid medium with no recognisable source",
			in:   Input{Source: "somebody.example", Medium: "ppc"},
			want: ChannelPaidOther,
		},
		{
			name: "an ordinary referring site",
			in:   Input{Source: "somebody.example"},
			want: ChannelReferral,
		},
		{
			name: "a medium that matched nothing and no referrer",
			in:   Input{Source: Direct, Medium: "mystery"},
			want: ChannelDirect,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Channel(tc.in); got != tc.want {
				t.Fatalf("Channel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPaidMediumVariants checks the `.*cp.*` arm covers the house variants
// nobody maintains a list for.
func TestPaidMediumVariants(t *testing.T) {
	for _, medium := range []string{"cpc", "cpm", "cpv", "cpa", "ppc", "retargeting", "paid", "paid_social", "PPC"} {
		got := Channel(Input{Source: "Google", Category: CategorySearch, Medium: medium})
		if got != ChannelPaidSearch {
			t.Errorf("medium %q gave %q, want %q", medium, got, ChannelPaidSearch)
		}
	}
}

// TestChannelIsCaseInsensitive checks a tag typed in capitals lands in the same
// channel. UTM values are case-sensitive in storage, but the channel decision is
// not — otherwise "CPC" and "cpc" would be two different acquisition stories.
func TestChannelIsCaseInsensitive(t *testing.T) {
	lower := Channel(Input{Source: "Google", Category: CategorySearch, Medium: "cpc", Campaign: "spring"})
	upper := Channel(Input{Source: "GOOGLE", Category: CategorySearch, Medium: "CPC", Campaign: "SPRING"})

	if lower != upper {
		t.Fatalf("case changed the channel: %q vs %q", lower, upper)
	}
}

// TestEveryChannelIsReachable is a completeness check. A channel nothing can
// produce is either a rule that was never written or a name that is wrong, and
// both are invisible without this.
func TestEveryChannelIsReachable(t *testing.T) {
	inputs := []Input{
		{Source: Direct},
		{Source: "Google", Category: CategorySearch},
		{Source: "Google", Category: CategorySearch, Medium: "cpc"},
		{Source: "Facebook", Category: CategorySocial},
		{Source: "Facebook", Category: CategorySocial, Medium: "cpc"},
		{Source: "YouTube", Category: CategoryVideo},
		{Source: "YouTube", Category: CategoryVideo, Medium: "cpc"},
		{Source: "Amazon", Category: CategoryShopping},
		{Source: "Amazon", Category: CategoryShopping, Medium: "cpc"},
		{Source: "Gmail", Category: CategoryEmail},
		{Source: "ChatGPT", Category: CategoryAI},
		{Source: "Spotify", Category: CategoryAudio},
		{Source: "x.example", Medium: "banner"},
		{Source: "x.example", Medium: "affiliate"},
		{Source: "x.example", Medium: "sms"},
		{Source: "x.example", Medium: "app-push"},
		{Source: "x.example", Medium: "ppc"},
		{Source: "x.example"},
		{Source: "Google", Category: CategorySearch, Campaign: "cross-network"},
	}

	reached := map[string]struct{}{}
	for _, in := range inputs {
		reached[Channel(in)] = struct{}{}
	}

	for _, channel := range Channels {
		if _, ok := reached[channel]; !ok {
			t.Errorf("no input produces the %q channel", channel)
		}
	}

	if len(Channels) != 19 {
		t.Fatalf("there are %d channels, want 19", len(Channels))
	}
}
