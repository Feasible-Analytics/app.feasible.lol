//
// visits.go
// What one visit looks like: where it came from, who it was, and what it did.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/ingest"
)

// emitDay generates one site's traffic for one day. The carried-over events go
// first because they are the earliest events of the day and because the visits
// they belong to are still in the session cache.
func (g *generator) emitDay(ctx context.Context, site *siteRun, day int, dayStart time.Time) error {
	carried := site.carry
	site.carry = nil

	budget := g.budget[day][site.index]

	// A day with no traffic has no traffic. The visits that were still going at
	// midnight would otherwise trickle into it, and a gap in the graph filled
	// with forty events is not the case anyone needs to see rendered.
	if budget == 0 {
		return nil
	}

	for _, item := range carried {
		if err := g.emit(ctx, site, item.payload, item.visitor, item.at); err != nil {
			return err
		}
	}

	dayEnd := dayStart.Add(24 * time.Hour)

	// The last day stops at the current time. A "today" that already holds a
	// full day of traffic makes every today-so-far number wrong.
	if dayEnd.After(g.now) {
		dayEnd = g.now
	}

	for budget > 0 {
		// A checkout walk decides its own length, because a funnel step that
		// fell off the end of a one-pageview visit would be a drop-off the
		// generator invented rather than one the visitor made.
		walk := g.checkoutWalk(site)

		length := g.sessionLength(site)
		if len(walk) > length {
			length = len(walk)
		}

		if int64(length) > budget {
			length = int(budget)
		}
		budget -= int64(length)

		if err := g.emitSession(ctx, site, day, dayStart, dayEnd, length, walk); err != nil {
			return err
		}

		if site.account.writer.full() {
			if err := site.account.writer.flush(ctx); err != nil {
				return err
			}
		}
	}

	return nil
}

// sessionLength samples how many pageviews a visit has. One session in the run
// is forced to the maximum: the tail of a sampled distribution is thin enough
// that a small run can miss it entirely, and a thirty-pageview visit is exactly
// the case the session fold has to get right.
func (g *generator) sessionLength(site *siteRun) int {
	if site.index == 0 && g.longSessions == 0 {
		g.longSessions++
		return maxSessionPageviews
	}

	return g.lengths.pick(g.rng.Float64()) + 1
}

// emitSession generates one visit: where it came from, who it was, and the
// pages, engagement pings and custom events it produced.
func (g *generator) emitSession(ctx context.Context, site *siteRun, day int, dayStart, dayEnd time.Time, pageviews int, walk []string) error {
	hour := site.hourChooser.pick(g.rng.Float64())
	start := dayStart.Add(time.Duration(hour)*time.Hour +
		time.Duration(g.rng.IntN(3600))*time.Second)

	if !start.Before(dayEnd) {
		// The hour that was drawn has not happened yet on the last day. Pulling
		// the visit back into the part of the day that has happened keeps the
		// budget spent rather than silently dropping it.
		span := dayEnd.Sub(dayStart)
		if span <= 0 {
			return nil
		}
		start = dayStart.Add(time.Duration(g.rng.Int64N(int64(span))))
	}

	person := visitorFor(pickVisitor(g.rng.Float64(), site.pool), g.agents, g.langs)

	// A small share of traffic is automated. It is classified rather than
	// dropped, so the bot toggle on the dashboard has something to toggle.
	bot := g.rng.Float64() < botShare
	if bot {
		person.UA = botAgents[g.rng.IntN(len(botAgents))]
	}

	entry := site.pages[site.entryChooser.pick(g.rng.Float64())]

	// A checkout visit starts where a checkout starts. Everything after the
	// walk is ordinary browsing, so a visit that abandoned at payment still
	// looks like somebody who carried on reading rather than one who vanished.
	if len(walk) > 0 {
		entry = walk[0]
	}

	// The page that has exactly one pageview in the whole dataset. It is what
	// breaks a report that divides by a previous period, and no amount of
	// sampling reliably produces one.
	if !g.singletonDone && site.index == 0 && day == g.days/3 {
		entry = singletonPath
		g.singletonDone = true
	}

	referrer, tags := g.acquisition(site, day)

	at := start.Unix()
	previous := ""

	for i := 0; i < pageviews; i++ {
		path := entry
		if i > 0 {
			path = site.pages[site.pageChooser.pick(g.rng.Float64())]
		}

		if i < len(walk) {
			path = walk[i]
		}

		host := site.domain

		// Traffic from somewhere the site does not own — a preview build, a
		// staging copy, a mirror. Hostname validation belongs to the shard and
		// is a later milestone; the rows it will have to classify can exist now.
		if g.rng.Float64() < unvalidated {
			host = unvalidatedHostname
		}

		pageURL := "https://" + host + path
		if i == 0 && tags != "" {
			pageURL += "?" + tags
		}

		// Only the first pageview of a visit carries the outside referrer.
		// Every one after it carries the page before it, which is what a
		// browser actually sends and what the same-site rule has to recognise
		// as internal rather than as an acquisition.
		from := previous
		if i == 0 {
			from = referrer
		}

		payload := &ingest.Payload{
			Name:     ingest.EventPageview,
			URL:      pageURL,
			Domain:   site.domain,
			Referrer: from,
			Title:    pageTitle(path, site.name),
			Width:    numberPtr(int64(person.Width)),
		}

		if err := g.emitOrCarry(ctx, site, payload, person, at, dayEnd); err != nil {
			return err
		}

		stay := dwell(g.rng.Float64())

		// The engagement ping is what carries time on page and scroll depth. It
		// is a real event kind with its own fold rule — it refreshes the end of
		// a visit without counting towards it — and a dataset without any never
		// exercises that rule.
		if g.rng.Float64() < engagementShare {
			ping := &ingest.Payload{
				Name:     ingest.EventEngagement,
				URL:      pageURL,
				Domain:   site.domain,
				Referrer: from,
				Title:    payload.Title,
				Width:    numberPtr(int64(person.Width)),
				Scroll:   numberPtr(int64(20 + g.rng.IntN(80))),
				Engage:   numberPtr(min64(stay, 1800) * 1000),
			}

			if err := g.emitOrCarry(ctx, site, ping, person, at+stay*4/5, dayEnd); err != nil {
				return err
			}
		}

		if g.rng.Float64() < customShare {
			if err := g.emitCustom(ctx, site, person, pageURL, from, at+stay/2, dayEnd); err != nil {
				return err
			}
		}

		previous = pageURL
		at += stay
	}

	return nil
}

// emitCustom fires one custom event: a signup, a purchase with money on it, an
// outbound click. They are what the goals, funnels and property filters are
// built against, and they are also the only rows that reach the cold table.
func (g *generator) emitCustom(ctx context.Context, site *siteRun, person visitor, pageURL, from string, at int64, dayEnd time.Time) error {
	event := customEvents[g.events.pick(g.rng.Float64())]

	// The catalogue's map is copied rather than written to: it is shared by
	// every event of that name in the whole run, and adding a key to it would
	// put the last visitor's variant on every event already generated.
	props := make(map[string]string, len(event.Props)+1)
	for name, value := range event.Props {
		props[name] = value
	}

	// The visitor's A/B group goes on every event of the visit, which is what
	// a session-scoped property looks like on the wire: one value, repeated,
	// for as long as the visit lasts.
	props["ab_test_group"] = person.Variant

	// One event in the run carries exactly thirty properties, which is the cap.
	// Nothing below it exercises the boundary, and the boundary is where the
	// incumbent silently drops the thirty-first with no error and no warning.
	if !g.maxPropsDone && site.index == 0 {
		props = maxPropsEvent()
		g.maxPropsDone = true
	}

	encoded, err := json.Marshal(props)
	if err != nil {
		return fmt.Errorf("seed: encode props: %w", err)
	}

	payload := &ingest.Payload{
		Name:     event.Name,
		URL:      pageURL,
		Domain:   site.domain,
		Referrer: from,
		Props:    encoded,
		Width:    numberPtr(int64(person.Width)),
	}

	if event.Revenue {
		// Money is a string here because that is one of the two shapes the
		// wire format allows, and the one that does not lose cents to a float.
		amount := fmt.Sprintf("%d.%02d", 9+g.rng.IntN(490), g.rng.IntN(100))
		payload.Revenue = json.RawMessage(fmt.Sprintf(`{"amount":"%s","currency":"%s"}`, amount, event.Currency))
	}

	return g.emitOrCarry(ctx, site, payload, person, at, dayEnd)
}

// acquisition decides where a visit came from: nothing at all, a referrer, or a
// tagged campaign. The tags are returned as a query string because that is how
// they arrive in production — read off the URL by the pipeline — rather than
// set on the event by the generator.
func (g *generator) acquisition(site *siteRun, day int) (referrer, tags string) {
	// The spike day is one link on one aggregator, which is what a spike
	// actually is. A spike spread evenly across every source is a shape no
	// alert would ever have to explain.
	if day == spikeDay(g.days) && site.index == 0 && g.rng.Float64() < 0.55 {
		return "https://news.ycombinator.com/", ""
	}

	roll := g.rng.Float64()

	switch {
	case roll < directShare:
		return "", ""

	case roll < directShare+campaignShare:
		item := campaigns[site.campaignChooser.pick(g.rng.Float64())]

		values := url.Values{}
		values.Set("utm_source", item.Source)
		values.Set("utm_medium", item.Medium)
		values.Set("utm_campaign", item.Name)
		if item.Content != "" {
			values.Set("utm_content", item.Content)
		}
		if item.Term != "" {
			values.Set("utm_term", item.Term)
		}
		if item.ClickID != "" {
			// The value is never stored — a click id is a per-click identifier
			// we have no consent to keep — but its presence is what separates a
			// paid click from an organic one.
			values.Set(item.ClickID, "seed-"+strconv.Itoa(g.rng.IntN(1_000_000)))
		}

		return item.Referrer, values.Encode()

	default:
		return site.sources[site.sourceChooser.pick(g.rng.Float64())], ""
	}
}

// emitOrCarry sends an event now, or parks it for tomorrow when it falls past
// midnight. Parking rather than clamping is what lets a visit span midnight —
// the case where a visitor's fingerprint changes underneath them and the
// previous salt is the only thing that keeps them one person.
func (g *generator) emitOrCarry(ctx context.Context, site *siteRun, payload *ingest.Payload, person visitor, at int64, dayEnd time.Time) error {
	if at < dayEnd.Unix() {
		return g.emit(ctx, site, payload, person, at)
	}

	// Past the end of the run there is no tomorrow to carry into, and a visit
	// that runs off the end of history is simply one that is still going.
	if !dayEnd.Before(g.now) {
		return nil
	}

	// A visit that would run more than a day long is not a visit. The session
	// timeout would have ended it long before, so it is dropped rather than
	// carried into a day it does not belong in.
	if at >= dayEnd.Add(24*time.Hour).Unix() {
		return nil
	}

	site.carry = append(site.carry, pending{payload: payload, visitor: person, at: at})

	return nil
}

// emit derives one event through the real pipeline and folds it into its
// session. This is the function the whole package exists to call: everything
// above it decides what a visitor did, and everything below it is the same code
// that runs when a browser posts to /api/event.
func (g *generator) emit(ctx context.Context, site *siteRun, payload *ingest.Payload, person visitor, at int64) error {
	g.clock = time.Unix(at, 0).UTC()

	g.request.RemoteAddr = person.IP + ":51000"
	g.request.Header.Set("User-Agent", person.UA)
	g.request.Header.Set("Accept-Language", person.Language)

	result, err := g.pipeline.Derive(ctx, g.request, payload)
	if err != nil {
		return fmt.Errorf("seed: derive: %w", err)
	}

	if result.DropReason != "" || result.Event == nil {
		// A dropped event is not a failure. Deliberate policy fixtures, such as
		// dormant-account traffic and unvalidated hostnames, are meant to be
		// dropped; counting them proves they did not disappear silently.
		g.stats.Dropped++
		return nil
	}

	if payload.Name == ingest.EventPageview {
		g.stats.Pageviews++
	}

	if site.account.writer.add(result.Event) {
		g.stats.Events++
	}

	return nil
}
