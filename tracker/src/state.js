//
// state.js
// The globals every module needs, and the one mutable record of the current page.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Aliasing the browser globals once is not a style preference: the minifier can
// shorten a module-level binding but cannot shorten `window`, and these names
// appear dozens of times across the bundle. It is worth several hundred bytes.
export const win = window;
export const doc = document;
export const loc = location;
export const nav = navigator;

// The tracker version reported as `v`. It is an integer because the server
// stores it as one, and it exists so that "which script is this site running"
// is a support answer rather than a guess — old scripts sit in browser caches
// for months and are the usual explanation for a payload that looks wrong.
export const VERSION = 1;

// page is what we currently believe the visitor is looking at. It is shared
// mutable state rather than a parameter passed everywhere because engagement,
// navigation and the click handlers all have to agree on which URL an event
// belongs to, and an engagement event that reports the URL the visitor has
// already navigated *to* attributes the reading time to the wrong page.
export const page = {
	// url is the URL the current pageview was reported with, not whatever
	// location says right now. During an SPA navigation those differ for the
	// moment it takes to flush the engagement event for the page being left.
	url: "",

	// key is the deduplication key for the current pageview. It is the full URL
	// rather than the pathname: deduplicating on the pathname is what silently
	// swallowed bfcache pageviews and query-string-only navigations.
	key: "",

	// tracked records that a pageview has been sent for this document. Manual
	// mode depends on it — a bfcache restore must only re-fire a pageview if
	// the site actually tracked one in the first place.
	tracked: false,

	// domain is the site identifier every event carries as `d`. It lives here
	// so that the modules which only ever send events do not each have to be
	// handed the whole configuration.
	domain: "",
};
