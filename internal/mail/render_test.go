//
// render_test.go
// What the shared layout puts on the page, and what it leaves off.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mail

import (
	"strings"
	"testing"
)

// TestAContentWithNoCodeRendersNoCodeBlock keeps the verification email's one
// block from appearing, empty, on the other twenty-two.
func TestAContentWithNoCodeRendersNoCodeBlock(t *testing.T) {
	plain, err := Content{Subject: "S", Heading: "H", Body: []string{"B"}}.HTML()
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(plain, "letter-spacing:6px") {
		t.Errorf("a content with no code rendered the code block:\n%s", plain)
	}

	coded, err := Content{Subject: "S", Heading: "H", Code: "12345678"}.HTML()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(coded, "12345678") || !strings.Contains(coded, "letter-spacing:6px") {
		t.Errorf("the code was not rendered prominently:\n%s", coded)
	}

	if !strings.Contains(Content{Code: "12345678"}.Text(), "12345678") {
		t.Error("the plain-text part lost the code")
	}
}
