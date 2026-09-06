//
// mailsample_test.go
// The guard: every message the product sends says who sent it.
//
// Created: 2026-09-06
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package mailsample

import (
	"sort"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/mail"
)

// TestEveryMessageCarriesTheCompanyAndAddress is the assertion the postal
// address existed for and never had.
//
// It was checked on two messages out of twenty-four, per message, which is how
// six of them came to be sent with no sender identity at all. Here it is
// checked on every one, in both bodies.
func TestEveryMessageCarriesTheCompanyAndAddress(t *testing.T) {
	messages, err := Messages()
	if err != nil {
		t.Fatal(err)
	}

	want := append([]string{mail.Company.Name}, mail.Company.AddressLines...)

	for tag, message := range messages {
		for _, line := range want {
			if !strings.Contains(message.HTML, line) {
				t.Errorf("%s: the HTML is missing %q", tag, line)
			}

			if !strings.Contains(message.Text, line) {
				t.Errorf("%s: the text is missing %q", tag, line)
			}
		}
	}
}

// TestTheSampleCoversEveryTag is what makes the test above a guard rather than
// a snapshot. A new sender that is not added here fails, instead of quietly
// not being checked.
func TestTheSampleCoversEveryTag(t *testing.T) {
	messages, err := Messages()
	if err != nil {
		t.Fatal(err)
	}

	built := make([]string, 0, len(messages))
	for tag := range messages {
		built = append(built, tag)
	}

	want := mail.Tags()

	sort.Strings(built)
	sort.Strings(want)

	if strings.Join(built, "\n") != strings.Join(want, "\n") {
		t.Errorf("the sample and mail.Tags disagree.\n--- sample ---\n%s\n--- tags ---\n%s",
			strings.Join(built, "\n"), strings.Join(want, "\n"))
	}
}

// TestMovingTheAddressMovesBothFooters proves there is one definition and not
// two. Before this, the HTML footer held its own copy of the four lines.
func TestMovingTheAddressMovesBothFooters(t *testing.T) {
	was := mail.Company
	t.Cleanup(func() { mail.Company = was })

	mail.Company = mail.Business{
		Name:         "A Different Company, LLC",
		AddressLines: []string{"1 Nowhere Lane", "Antarctica"},
	}

	messages, err := Messages()
	if err != nil {
		t.Fatal(err)
	}

	for tag, message := range messages {
		for _, line := range append([]string{mail.Company.Name}, mail.Company.AddressLines...) {
			if !strings.Contains(message.HTML, line) {
				t.Errorf("%s: the HTML footer did not follow the address to %q", tag, line)
			}

			if !strings.Contains(message.Text, line) {
				t.Errorf("%s: the text footer did not follow the address to %q", tag, line)
			}
		}

		if strings.Contains(message.HTML, was.AddressLines[0]) {
			t.Errorf("%s: the HTML still carries the old street", tag)
		}
	}
}

// TestEveryMessageNamesItselfInTheSubject keeps a message with an empty subject
// line out of an inbox.
func TestEveryMessageNamesItselfInTheSubject(t *testing.T) {
	messages, err := Messages()
	if err != nil {
		t.Fatal(err)
	}

	for tag, message := range messages {
		if strings.TrimSpace(message.Subject) == "" {
			t.Errorf("%s has no subject", tag)
		}

		if message.Tag != tag {
			t.Errorf("%s was addressed with the tag %q", tag, message.Tag)
		}
	}
}
