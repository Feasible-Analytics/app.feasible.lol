//
// sources_test.go
// Canonical source metadata used by acquisition imports.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package referrer

import "testing"

// TestCanonicalSourceCategoriesAreRecoverable checks the labels an aggregate
// analytics export carries can be put back through the channel classifier.
func TestCanonicalSourceCategoriesAreRecoverable(t *testing.T) {
	cases := map[string]Category{
		"Google":            CategorySearch,
		"Brave":             CategorySearch,
		"Facebook":          CategorySocial,
		"X (Twitter)":       CategorySocial,
		"Yahoo!":            CategorySearch,
		"Microsoft Copilot": CategoryAI,
		"copilot.com":       CategoryAI,
		"Google Gemini":     CategoryAI,
		"chatgpt.com":       CategoryAI,
		"Newsletter":        CategoryEmail,
		"unknown.example":   CategoryUnknown,
	}

	for name, want := range cases {
		if got := CategoryForSource(name); got != want {
			t.Errorf("CategoryForSource(%q) = %d, want %d", name, got, want)
		}
	}
}
