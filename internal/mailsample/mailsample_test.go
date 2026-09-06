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

// TestTheAddressIsAWholeAddress is what stops the guard below being circular.
//
// That guard builds what it looks for out of mail.Company, so an empty
// mail.Company satisfies it on every message. This is the one place a test
// writes the address out, and it is what a move of office has to change.
func TestTheAddressIsAWholeAddress(t *testing.T) {
	if mail.Company.Name != "Cloudmanic Labs, LLC" {
		t.Errorf("the company is %q", mail.Company.Name)
	}

	want := []string{"901 Brutscher Street, D112", "Newberg, OR 97132", "United States"}

	if len(mail.Company.AddressLines) != len(want) {
		t.Fatalf("the address has %d lines, want %d: %q", len(mail.Company.AddressLines), len(want),
			mail.Company.AddressLines)
	}

	for i, line := range want {
		if mail.Company.AddressLines[i] != line {
			t.Errorf("address line %d is %q, want %q", i+1, mail.Company.AddressLines[i], line)
		}
	}
}

// TestEveryMessageCarriesTheCompanyAndAddress is the assertion CAN-SPAM needs,
// on every message rather than on whichever ones have a test of their own.
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
// a snapshot. A tag with no sample fails, instead of quietly not being checked.
func TestTheSampleCoversEveryTag(t *testing.T) {
	messages, err := Messages()
	if err != nil {
		t.Fatal(err)
	}

	built := make([]string, 0, len(messages))
	for tag := range messages {
		built = append(built, tag)
	}

	if missing, extra := compare(built, mail.Tags()); len(missing) > 0 || len(extra) > 0 {
		t.Errorf("the sample and mail.Tags disagree. Not sampled: %q. Not in Tags: %q.", missing, extra)
	}
}

// TestAnIncompleteSampleIsCaught shows the guard guarding, since a completeness
// check that cannot fail is the whole failure mode being defended against.
func TestAnIncompleteSampleIsCaught(t *testing.T) {
	all := mail.Tags()

	missing, extra := compare(all[1:], all)
	if len(missing) != 1 || missing[0] != all[0] {
		t.Errorf("dropping %q from the sample was not reported: missing=%q", all[0], missing)
	}

	if len(extra) != 0 {
		t.Errorf("nothing was added, but %q was reported as extra", extra)
	}

	missing, extra = compare(append(all, "brand_new"), all)
	if len(extra) != 1 || extra[0] != "brand_new" {
		t.Errorf("a sample with no tag was not reported: extra=%q", extra)
	}

	if len(missing) != 0 {
		t.Errorf("nothing was dropped, but %q was reported as missing", missing)
	}
}

// compare reports what is in one list and not the other, both ways round.
func compare(sample, tags []string) (missing, extra []string) {
	sampled := map[string]bool{}
	for _, tag := range sample {
		sampled[tag] = true
	}

	listed := map[string]bool{}
	for _, tag := range tags {
		listed[tag] = true

		if !sampled[tag] {
			missing = append(missing, tag)
		}
	}

	for _, tag := range sample {
		if !listed[tag] {
			extra = append(extra, tag)
		}
	}

	sort.Strings(missing)
	sort.Strings(extra)

	return missing, extra
}

// TestMovingTheAddressMovesBothFooters proves there is one definition and not
// two: move it, and both footers follow.
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

		// The company name and the street, not the country: a report can
		// legitimately list "United States" as a top country.
		for _, old := range []string{was.Name, was.AddressLines[0]} {
			if strings.Contains(message.HTML, old) {
				t.Errorf("%s: the HTML still carries the old %q", tag, old)
			}

			if strings.Contains(message.Text, old) {
				t.Errorf("%s: the text still carries the old %q", tag, old)
			}
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
