//
// core.cjs
// The whole client: the two headers you cannot forget, and a retry that knows what not to retry.
//
// Created: 2026-08-30
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//
// This file is CommonJS and the ESM entry re-exports it. That is not an
// accident and not a build artefact: `require()` of an ES module only works
// from Node 20.19 onwards, and this package supports Node 18, so a CJS core
// with a thin ESM wrapper is the only shape that gives both `import` and
// `require` with no build step and no duplicated logic.
//

"use strict";

// The hosted service. A self-hoster passes their own host and nothing else
// changes, which is why the endpoint path is not configurable — there is only
// one, and a setting for it would only be something to get wrong.
const DEFAULT_HOST = "https://app.feasible.lol";

// The defaults that are right for almost everybody. Five seconds is longer than
// the endpoint has ever needed and short enough that an unreachable host cannot
// hold a request handler open.
const DEFAULT_TIMEOUT = 5000;
const DEFAULT_ATTEMPTS = 3;
const DEFAULT_BASE_BACKOFF = 100;
const DEFAULT_MAX_BACKOFF = 2000;

// The headers that carry the answer back out of the ingest endpoint. A drop is
// a classification rather than a failure, so it is surfaced on the result and
// never turned into a thrown error.
const HEADER_DROPPED = "x-feasible-dropped";
const HEADER_DEBUG = "X-Debug-Request";

// The environment switch that turns the client into a recorder. It exists so a
// CI run or a local development server can exercise the real code path without
// a single packet leaving the machine.
const DISABLED_ENV = "FEASIBLE_DISABLED";

// FeasibleValidationError is a refusal to send something the server would only
// reject or misattribute. It carries the field and a `code` so a caller can
// branch on the specific mistake, and a message that says why the field matters
// — the person reading it is the only person who can fix it.
class FeasibleValidationError extends Error {
	// The constructor builds the whole sentence up front so the message is
	// useful in a log where nobody will click through to documentation.
	constructor(field, code, rule, reason) {
		super(`feasible: ${field}: ${rule}. ${reason}`);

		this.name = "FeasibleValidationError";
		this.field = field;
		this.code = code;
		this.reason = reason;
	}
}

// FeasibleApiError is a response the server understood and refused. The body is
// kept verbatim because the endpoint answers a 400 with a sentence naming what
// is wrong, and paraphrasing it would lose the only diagnosis there is.
class FeasibleApiError extends Error {
	// Status, body and attempt count are all on the error because a log line
	// with only "request failed" starts every support conversation from nothing.
	constructor(statusCode, body, attempts) {
		super(
			body
				? `feasible: server returned ${statusCode} after ${attempts} attempt(s): ${body}`
				: `feasible: server returned ${statusCode} after ${attempts} attempt(s)`,
		);

		this.name = "FeasibleApiError";
		this.statusCode = statusCode;
		this.body = body;
		this.attempts = attempts;
	}
}

// FeasibleTransportError is a request that never reached a server. It is a
// separate class from the API error because the retry decision turns on the
// difference: nothing was counted, so nothing can be duplicated by trying again.
class FeasibleTransportError extends Error {
	// The cause is kept because a DNS failure and a refused connection are
	// different problems with different fixes.
	constructor(attempt, cause) {
		super(`feasible: request failed on attempt ${attempt}: ${cause && cause.message}`);

		this.name = "FeasibleTransportError";
		this.attempt = attempt;
		this.cause = cause;
	}
}

// text returns a trimmed string for anything, so that a number or an accidental
// object cannot slip past a required-field check as a truthy value.
function text(value) {
	return typeof value === "string" ? value.trim() : "";
}

// envDisabled reads the no-op switch. It accepts the obvious spellings because
// the value is typed into a CI configuration by hand, and "true" failing where
// "1" works is a wasted afternoon.
function envDisabled() {
	const raw = typeof process !== "undefined" && process.env ? process.env[DISABLED_ENV] : "";

	return ["1", "true", "yes", "on"].includes(String(raw || "").trim().toLowerCase());
}

// hostOnly strips the port from a socket address in either family. Node's
// `socket.remoteAddress` has no port, but a forwarded header often does, and
// sending "203.0.113.9:54321" as an address produces an event with no country.
function hostOnly(value) {
	const address = text(value);
	if (!address) return "";

	// A bracketed IPv6 literal keeps everything inside the brackets.
	if (address.startsWith("[")) {
		const end = address.indexOf("]");
		return end === -1 ? address : address.slice(1, end);
	}

	// A bare IPv6 address has several colons and no port; only a single colon
	// means host:port.
	const colons = address.split(":").length - 1;
	if (colons === 1) return address.split(":")[0];

	return address;
}

// pickForwarded takes the client out of an X-Forwarded-For chain. The FIRST
// entry is the client: every proxy appends itself, so taking the last one —
// which several frameworks do — reports your own load balancer as the visitor
// and collapses every visit into one.
function pickForwarded(value) {
	const header = Array.isArray(value) ? value[0] : value;

	return hostOnly(text(header).split(",")[0]);
}

// visitorFromNodeRequest lifts the visitor out of a Node IncomingMessage. It
// assumes the application edge has stripped client-supplied forwarding headers;
// unlike the ingest service, this helper has no trusted-proxy configuration.
function visitorFromNodeRequest(request) {
	if (!request || !request.headers) return { clientIp: "", userAgent: "" };

	const headers = request.headers;
	const userAgent = text(headers["user-agent"]);

	const cloudflare = text(headers["cf-connecting-ip"]);
	if (cloudflare) return { clientIp: cloudflare, userAgent };

	const forwarded = pickForwarded(headers["x-forwarded-for"]);
	if (forwarded) return { clientIp: forwarded, userAgent };

	const socket = request.socket || request.connection || {};

	return { clientIp: hostOnly(socket.remoteAddress), userAgent };
}

// visitorFromWebRequest does the same for a WHATWG Request — a Next.js route
// handler, a Remix loader, a worker. A Request carries no socket, so a platform
// that does not set a forwarding header leaves the IP empty and the SDK refuses
// the call rather than sending an event that would be attributed to nobody.
function visitorFromWebRequest(request) {
	if (!request || !request.headers || typeof request.headers.get !== "function") {
		return { clientIp: "", userAgent: "" };
	}

	const headers = request.headers;
	const userAgent = text(headers.get("user-agent"));

	const cloudflare = text(headers.get("cf-connecting-ip"));
	if (cloudflare) return { clientIp: cloudflare, userAgent };

	return { clientIp: pickForwarded(headers.get("x-forwarded-for")), userAgent };
}

// wait sleeps between attempts, and gives up early when the caller's signal
// aborts. A retry loop that ignores an abort holds a request handler open long
// after whoever asked for it has gone.
function wait(ms, signal) {
	return new Promise((resolve, reject) => {
		const timer = setTimeout(() => {
			if (signal) signal.removeEventListener("abort", onAbort);
			resolve();
		}, ms);

		function onAbort() {
			clearTimeout(timer);
			reject(signal.reason || new Error("aborted"));
		}

		if (signal) {
			if (signal.aborted) return onAbort();
			signal.addEventListener("abort", onAbort, { once: true });
		}
	});
}

// FeasibleClient sends events. Build one for the life of the process: the value
// of reusing it is the keep-alive connection pool underneath `fetch`, which is
// the difference between one TLS handshake and one per event.
//
// Two things are required on every event and neither can be guessed by the
// server: the visitor's IP and their User-Agent. A call from your server
// carrying neither arrives from a datacentre address with no visitor in it, and
// the endpoint answers 400 rather than quietly attributing the visit to your
// hosting provider. That is why `clientIp` and `userAgent` are required
// properties, validated at call time, and why the two visitor helpers exist.
class FeasibleClient {
	// The constructor validates the domain immediately, because the domain
	// comes from configuration and a typo there is a silent nothing-recorded
	// that nobody notices for a week.
	constructor(options = {}) {
		const domain = text(options.domain);

		if (!domain) {
			throw new FeasibleValidationError(
				"options.domain",
				"missing_domain",
				"a site domain is required",
				"It is the site identifier every event carries, exactly as the site is registered.",
			);
		}

		this.domain = domain;
		this.endpoint = `${text(options.host) || DEFAULT_HOST}`.replace(/\/+$/, "") + "/api/event";
		this.timeout = options.timeout > 0 ? options.timeout : DEFAULT_TIMEOUT;
		this.attempts = options.attempts > 0 ? options.attempts : DEFAULT_ATTEMPTS;
		this.baseBackoff = options.baseBackoff > 0 ? options.baseBackoff : DEFAULT_BASE_BACKOFF;
		this.maxBackoff = options.maxBackoff > 0 ? options.maxBackoff : DEFAULT_MAX_BACKOFF;
		this.disabled = options.disabled === true || envDisabled();

		// The fetch implementation is injectable for the same reason the host
		// is: a test wants to point somewhere else. It defaults to the built-in
		// one, which is why this package has no runtime dependencies at all.
		this.fetch = options.fetch || globalThis.fetch;
		this.events = [];
	}

	// recorded returns the events a no-op client accepted, oldest first. This is
	// how a test asserts that the code under test reported what it should have,
	// with no network and no stub server.
	recorded() {
		return this.events.slice();
	}

	// reset clears the recorded events, so one test's assertions cannot be
	// satisfied by another test's events.
	reset() {
		this.events = [];
	}

	// pageview reports a pageview. The event object still requires `clientIp`
	// and `userAgent`; spread a visitor helper into it.
	pageview(event = {}, options = {}) {
		return this.send({ ...event, name: "pageview" }, options);
	}

	// track reports a custom event under the given name.
	track(name, event = {}, options = {}) {
		return this.send({ ...event, name }, options);
	}

	// send delivers one event. Validation runs before the no-op check on
	// purpose: a test suite that never sends anything is exactly where a missing
	// IP would otherwise hide until production.
	async send(event = {}, options = {}) {
		const body = this.payload(event);

		if (this.disabled) {
			this.events.push({ ...event });

			return { statusCode: 0, dropReason: "", attempts: 0, skipped: true };
		}

		const { result } = await this.deliver(event, body, false, options.signal);

		return result;
	}

	// debug asks the server what it would derive from this event and returns
	// that JSON instead of writing anything. It is free of side effects and safe
	// against production, which is what makes "my numbers look wrong" answerable
	// in one call.
	async debug(event = {}, options = {}) {
		const body = this.payload(event);

		if (this.disabled) {
			throw new Error("feasible: debug() needs a live request and this client is in no-op mode");
		}

		const { text: raw } = await this.deliver(event, body, true, options.signal);

		return raw ? JSON.parse(raw) : null;
	}

	// payload validates an event and encodes it. Absent values are omitted
	// rather than sent as null, because a null in this body is a value the
	// server has to decide about, and every such decision is a place the two
	// sides can disagree.
	payload(event) {
		const name = text(event && event.name);
		const url = text(event && event.url);
		const clientIp = text(event && event.clientIp);
		const userAgent = text(event && event.userAgent);

		if (!name) {
			throw new FeasibleValidationError(
				"event.name",
				"missing_name",
				"an event name is required",
				'Use "pageview" for a pageview, or any other name for a custom event.',
			);
		}

		if (!url) {
			throw new FeasibleValidationError(
				"event.url",
				"missing_url",
				"an event URL is required",
				"It is the full URL of the page the event happened on, and every page-level report is derived from it.",
			);
		}

		if (!clientIp) {
			throw new FeasibleValidationError(
				"event.clientIp",
				"missing_client_ip",
				"the visitor's client IP is required",
				"Without it the event arrives from your datacentre address, is classified as a bot and is dropped. Use visitorFromNodeRequest(req) or visitorFromWebRequest(request) to take it from the inbound request.",
			);
		}

		if (!userAgent) {
			throw new FeasibleValidationError(
				"event.userAgent",
				"missing_user_agent",
				"the visitor's User-Agent is required",
				"It is what the browser, device and operating system columns are derived from, and a request without one is treated as a bot. Use visitorFromNodeRequest(req) or visitorFromWebRequest(request) to take it from the inbound request.",
			);
		}

		// The keys are assigned in wire order so a captured body is diffable by
		// eye against the documented contract.
		const body = { n: name, u: url, d: text(event.domain) || this.domain };

		if (text(event.referrer)) body.r = event.referrer;
		if (event.props && Object.keys(event.props).length) body.p = event.props;
		if (text(event.title)) body.t = event.title;
		if (typeof event.interactive === "boolean") body.i = event.interactive;
		if (typeof event.scrollDepth === "number") body.sd = event.scrollDepth;
		if (typeof event.engagementTime === "number") body.e = event.engagementTime;
		if (typeof event.viewportWidth === "number") body.w = event.viewportWidth;

		if (event.revenue && typeof event.revenue.amount === "number") {
			body.$ = { amount: event.revenue.amount, currency: text(event.revenue.currency) };
		}

		// The attribution overrides. A delayed or offline conversion has no
		// referrer of its own, so without these it is Direct forever and the
		// campaign that actually paid for it gets no credit.
		const attribution = event.attribution || {};

		if (text(attribution.referrer)) body.referrer = attribution.referrer;
		if (text(attribution.utmSource)) body.utm_source = attribution.utmSource;
		if (text(attribution.utmMedium)) body.utm_medium = attribution.utmMedium;
		if (text(attribution.utmCampaign)) body.utm_campaign = attribution.utmCampaign;
		if (text(attribution.utmContent)) body.utm_content = attribution.utmContent;
		if (text(attribution.utmTerm)) body.utm_term = attribution.utmTerm;

		return JSON.stringify(body);
	}

	// deliver runs the attempt loop. The retry rules are the point of it: a 400
	// is the caller's bug and retrying changes nothing, and a 202 carrying a
	// drop reason is a decision the server made rather than a failure, so
	// resending it would only duplicate a classification.
	async deliver(event, body, debug, signal) {
		let lastError;

		for (let attempt = 1; attempt <= this.attempts; attempt++) {
			try {
				return await this.attempt(event, body, debug, attempt, signal);
			} catch (error) {
				lastError = error;

				if (!retryable(error) || attempt === this.attempts) throw error;

				await wait(this.backoff(attempt), signal);
			}
		}

		throw lastError;
	}

	// attempt performs one request. The body is always read, even when it is
	// thrown away, so the connection can go back in the keep-alive pool rather
	// than being torn down and re-handshaked for the next event.
	async attempt(event, body, debug, attempt, signal) {
		const controller = new AbortController();
		const timer = setTimeout(() => controller.abort(new Error("timed out")), this.timeout);

		const onAbort = () => controller.abort(signal.reason);
		if (signal) {
			if (signal.aborted) controller.abort(signal.reason);
			else signal.addEventListener("abort", onAbort, { once: true });
		}

		const headers = {
			// text/plain is deliberate. It is what keeps a browser from sending
			// a CORS preflight, the endpoint accepts it, and using it everywhere
			// means the server-side path and the browser path are one request.
			"Content-Type": "text/plain",
			"X-Forwarded-For": event.clientIp,
			"User-Agent": event.userAgent,
		};

		if (debug) headers[HEADER_DEBUG] = "true";

		let response;

		try {
			response = await this.fetch(this.endpoint, {
				method: "POST",
				headers,
				body,
				signal: controller.signal,
			});
		} catch (error) {
			throw new FeasibleTransportError(attempt, error);
		} finally {
			clearTimeout(timer);
			if (signal) signal.removeEventListener("abort", onAbort);
		}

		const raw = await response.text();

		if (response.ok) {
			return {
				result: {
					statusCode: response.status,
					dropReason: response.headers.get(HEADER_DROPPED) || "",
					attempts: attempt,
					skipped: false,
				},
				text: raw,
			};
		}

		throw new FeasibleApiError(response.status, raw.trim(), attempt);
	}

	// backoff is exponential with equal jitter, capped. The jitter matters when
	// a deploy restarts a fleet at once: without it every instance retries on
	// the same millisecond and the server gets the spike it was backing off from.
	backoff(attempt) {
		const wanted = this.baseBackoff * Math.pow(2, attempt - 1);
		const capped = Math.min(wanted, this.maxBackoff);

		return Math.round(capped / 2 + Math.random() * (capped / 2));
	}
}

// retryable decides whether another attempt could plausibly go differently. A
// 429 and a 5xx are the server asking for time; everything else at the HTTP
// level is a request that will be rejected identically forever.
function retryable(error) {
	if (error instanceof FeasibleTransportError) return true;
	if (error instanceof FeasibleApiError) return error.statusCode === 429 || error.statusCode >= 500;

	return false;
}

// createClient is the function form, for callers who would rather not write
// `new`. It is the same object either way.
function createClient(options) {
	return new FeasibleClient(options);
}

module.exports = {
	DEFAULT_HOST,
	DISABLED_ENV,
	HEADER_DROPPED,
	HEADER_DEBUG,
	FeasibleClient,
	FeasibleValidationError,
	FeasibleApiError,
	FeasibleTransportError,
	createClient,
	visitorFromNodeRequest,
	visitorFromWebRequest,
};
