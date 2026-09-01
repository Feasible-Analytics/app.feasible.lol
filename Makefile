#
# Makefile
# One command per process, on uncommon ports, with a Tailscale twin of each.
#
# Created: 2026-08-30
# Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
#
# Run `make` on its own for the list of targets.

# ── Ports ─────────────────────────────────────────────────────────────────────
# 193xx is public, 194xx is internal: the number says which side of the fence a
# process sits on. They are deliberately away from 3000/4000/5000/8000/8080,
# which collide with everything else on a working machine.
PORT_CADDY        ?= 19300
PORT_APP          ?= 19301
PORT_INGEST       ?= 19302
PORT_SITE         ?= 19303
PORT_APP_INTERNAL    ?= 19401
PORT_INGEST_INTERNAL ?= 19402

# ── Addresses ─────────────────────────────────────────────────────────────────
# BIND_HOST is what the processes listen on; PUBLIC_HOST is what ends up in URLs.
# They are two variables because the -ts targets have to move both together: the
# base URL is baked into the tracker snippet, every email link and the OAuth
# redirect URIs, so binding to Tailscale while leaving the URL on localhost gives
# cookies that will not set and redirects that bounce, with no useful error.
BIND_HOST   ?= 127.0.0.1
PUBLIC_HOST ?= localhost

BASE_URL      = http://$(PUBLIC_HOST):$(PORT_CADDY)
SOLO_BASE_URL = http://$(PUBLIC_HOST):$(PORT_APP)

# The health and metrics listeners never move off loopback, in any mode. Current
# main has no internal event-delivery or salt-distribution API.
INTERNAL_LISTEN        = 127.0.0.1:$(PORT_APP_INTERNAL)
INGEST_INTERNAL_LISTEN = 127.0.0.1:$(PORT_INGEST_INTERNAL)

# ── Tailscale ─────────────────────────────────────────────────────────────────
# The App Store build does not put the CLI on PATH, hence the fallback path.
# These are recursively expanded so that `make test` never pays for a Tailscale
# lookup it does not need.
TAILSCALE_BIN := $(shell command -v tailscale 2>/dev/null \
	|| echo /Applications/Tailscale.app/Contents/MacOS/Tailscale)
TAILSCALE_IP    = $(shell $(TAILSCALE_BIN) ip -4 2>/dev/null | head -1)
TAILSCALE_NAME  = $(shell $(TAILSCALE_BIN) status --json 2>/dev/null \
	| python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))' 2>/dev/null)

# Prefer the MagicDNS name over the raw IP: it survives reconnects, and Google
# rejects a bare private IP as an OAuth redirect URI, so login over Tailscale
# only works with the hostname.
TAILSCALE_HOST = $(or $(TAILSCALE_NAME),$(TAILSCALE_IP))

# ── Build ─────────────────────────────────────────────────────────────────────
BINARY  ?= feasible
PKG     := github.com/Feasible-Analytics/app.feasible.lol
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/build.Version=$(VERSION) \
	-X $(PKG)/internal/build.Commit=$(COMMIT) \
	-X $(PKG)/internal/build.Date=$(DATE)

# ── Process environments ──────────────────────────────────────────────────────
# Every run target sets the same variables from the same place, so there is one
# answer to "what is this process actually configured with".
APP_ENV = FEASIBLE_APP_LISTEN=$(BIND_HOST):$(PORT_APP) \
	FEASIBLE_APP_INTERNAL_LISTEN=$(INTERNAL_LISTEN) \
	FEASIBLE_APP_BASE_URL=$(BASE_URL)

INGEST_ENV = FEASIBLE_INGEST_LISTEN=$(BIND_HOST):$(PORT_INGEST) \
	FEASIBLE_INGEST_INTERNAL_LISTEN=$(INGEST_INTERNAL_LISTEN)

CADDY_ENV = FEASIBLE_CADDY_BIND=$(BIND_HOST) \
	FEASIBLE_CADDY_PORT=$(PORT_CADDY) \
	FEASIBLE_CADDY_APP_UPSTREAM=$(BIND_HOST):$(PORT_APP) \
	FEASIBLE_CADDY_INGEST_UPSTREAM=$(BIND_HOST):$(PORT_INGEST)

# ── Seeding ───────────────────────────────────────────────────────────────────
# The size of a seeded dataset, overridable on the command line:
#
#   make seed PAGEVIEWS=250000 DAYS=90 SITES=3
#
# SEED is fixed so two runs produce the same database. Change it and you get a
# different but equally valid dataset; leave it and "this query got slower" can
# never mean "this is different data".
PAGEVIEWS   ?= 120000
DAYS        ?= 42
SITES       ?= 5
SEED        ?= 20260830
HTTP_EVENTS ?= 200

# FRESH deletes the seeded databases before generating. Clear it — FRESH= — to
# add another run's worth of traffic to what is already there.
FRESH ?= --fresh

.DEFAULT_GOAL := help

.PHONY: help assets tracker-deps tracker web-deps ui-css build test test-race test-web test-tracker test-integration test-ecosystem \
	bench lint check-env \
	migrate migrate-fresh seed seed-big seed-http caddy app ingest testsite dev dev-solo \
	caddy-ts app-ts ingest-ts testsite-ts dev-ts dev-solo-ts require-tailscale

# ── Help ──────────────────────────────────────────────────────────────────────

## help: list the targets worth typing
help:
	@echo "feasible.lol — make targets"
	@echo
	@echo "  Run one process each, in its own terminal, so its logs stay readable:"
	@echo "    make caddy      reverse proxy on :$(PORT_CADDY) — the only port you open in a browser"
	@echo "    make app        the app on :$(PORT_APP) (internal :$(PORT_APP_INTERNAL), loopback only)"
	@echo "    make ingest     the ingest tier on :$(PORT_INGEST)"
	@echo "    make testsite   a page with the snippet installed, on :$(PORT_SITE)"
	@echo
	@echo "  Combined:"
	@echo "    make dev        all three processes, http transport, production-shaped"
	@echo "    make dev-solo   one process, direct transport, the self-hoster path"
	@echo
	@echo "  Every runnable target has a -ts twin that binds to Tailscale instead"
	@echo "  of loopback: make app-ts, make dev-ts, and so on."
	@echo
	@echo "  Toolchain:"
	@echo "    make build      build ./$(BINARY) (runs the asset build first)"
	@echo "    make tracker    build the browser script and check it fits the size budget"
	@echo "    make test       unit tests, including the tracker size budget"
	@echo "    make test-web   the dashboard's unit tests on their own"
	@echo "    make test-tracker       the tracker's end-to-end suite in a real browser"
	@echo "    make test-integration   end-to-end tests through Caddy"
	@echo "    make test-ecosystem     the SDKs and the plugin, in whichever toolchains you have"
	@echo "    make bench      the write and read benchmarks (minutes, seeds its own data)"
	@echo "    make lint       go vet and golangci-lint"
	@echo "    make test-race  the same tests under the race detector"
	@echo "    make check-env  every environment variable is in .env.sample"
	@echo "    make migrate    migrate control.db and every account database"
	@echo "    make migrate-fresh      drop everything and rebuild"
	@echo
	@echo "  Data to build and measure against:"
	@echo "    make seed       ~$(PAGEVIEWS) pageviews over $(DAYS) days across $(SITES) sites"
	@echo "    make seed-big   one site, a million pageviews in a month"
	@echo "    make seed-http  ~$(HTTP_EVENTS) events over real HTTP, end to end"
	@echo "                    make seed PAGEVIEWS=250000 DAYS=90 SITES=3"

# ── Toolchain ─────────────────────────────────────────────────────────────────

## tracker-deps: install the exact tracker dependency graph from its lockfile
tracker-deps:
	@if command -v npm >/dev/null 2>&1; then \
		npm --prefix tracker ci --silent; \
	elif command -v node >/dev/null 2>&1; then \
		echo "node is installed but npm is not — cannot install tracker dependencies"; exit 1; \
	else \
		echo "node is not installed — skipping tracker dependencies"; \
	fi

## tracker: build the browser script and enforce its size budget
# The bundle is minified and gzipped here, and the build fails outright if it is
# over budget. A tracking script that grows without a ceiling eventually costs a
# customer their Core Web Vitals score, and there is no natural moment to start
# caring — failing the build is the only enforcement that works.
#
# The same budget is enforced by a Go test over the embedded copy, which is what
# catches an over-budget bundle on a machine with no Node at all. This target is
# what catches it before it is ever committed.
tracker: tracker-deps
	@if [ -d tracker/node_modules ]; then \
		npm --prefix tracker run build; \
	else \
		echo "node is not installed — skipping the tracker build (go test still enforces the budget)"; \
	fi

## web-deps: install the exact dashboard dependency graph from its lockfile
web-deps:
	@if [ ! -f web/package-lock.json ]; then \
		echo "no web/ directory yet — no dashboard dependencies to install"; \
	elif command -v npm >/dev/null 2>&1; then \
		npm --prefix web ci --silent; \
	elif command -v node >/dev/null 2>&1; then \
		echo "node is installed but npm is not — cannot install dashboard dependencies"; exit 1; \
	else \
		echo "node is not installed — skipping dashboard dependencies"; \
	fi

## assets: build the front-end assets Go embeds
# Building needs Node; running does not. From a clean checkout `go build` alone
# is not enough once the dashboard exists, which is why build depends on this.
#
# The compiled output is committed to the repository, written by CI on release
# rather than by hand. It keeps `go build` and `go install` working for anyone
# without a JavaScript toolchain, which is the whole promise of a single binary;
# the JavaScript sources stay the source of truth.
assets: tracker web-deps ui-css
	@if [ -d web/node_modules ]; then \
		echo "building front-end assets"; \
		npm --prefix web run build; \
	else \
		echo "no web/ directory yet — nothing to build"; \
	fi

## ui-css: rebuild the stylesheet the server-rendered screens embed
# The output is committed, like every other compiled asset, so `go build` works
# on a machine with no JavaScript toolchain. A missing Tailwind binary is a note
# rather than a failure for the same reason: the committed file is still there.
ui-css: web-deps
	@if [ -x web/node_modules/.bin/tailwindcss ]; then \
		NODE_PATH="$(CURDIR)/web/node_modules" web/node_modules/.bin/tailwindcss \
			-i internal/auth/tailwind.css -o internal/auth/assets/app.css --minify; \
	elif command -v tailwindcss >/dev/null 2>&1; then \
		tailwindcss -i internal/auth/tailwind.css -o internal/auth/assets/app.css --minify; \
	else \
		echo "tailwindcss is not installed — keeping the committed internal/auth/assets/app.css"; \
	fi

## build: compile the single binary
build: assets
	@go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/$(BINARY)
	@echo "built ./$(BINARY) $(VERSION) ($(COMMIT))"

## test: unit tests, and the tracker size budget
# The tracker is rebuilt first so that the budget is measured against the source
# in the working tree rather than against whatever was last committed.
test: tracker test-web
	@go test ./...

## test-web: the dashboard's unit tests
# The URL filter encoding, the comparison arithmetic and the period stepping are
# the pieces of the dashboard that are pure functions with a contract, and all
# three are the kind of thing a rendering test would pass while getting wrong. A
# machine with no JavaScript toolchain skips them rather than failing, the same
# way the tracker build does — the Go suite still has to pass there.
test-web: web-deps
	@if [ -d web/node_modules ]; then \
		npm --prefix web test; \
	else \
		echo "npm is not installed — skipping the dashboard unit tests"; \
	fi

## test-race: the same tests under the race detector
# Its own target with its own timeout. The detector slows every test by roughly
# an order of magnitude, so the ten-minute per-package default expires in the
# packages that matter most here — the roll-up worker and the ingest pipeline are
# the concurrent code, and a detector that times out reports nothing at all.
test-race:
	@go test -race -timeout 40m ./...

## test-tracker: the tracker's end-to-end suite, in a real browser
# Playwright starts and stops its own fixture server, so this target leaves
# nothing listening. The only honest test of a tracking script is a browser
# loading a real page from a real origin.
test-tracker: tracker
	@npm --prefix tracker test

## bench: the write and read benchmarks behind every capacity claim
# Not part of `make test`: a run seeds its own data and takes minutes, and a
# number measured on a machine that is also running a test suite is not a
# number. Point the read half at a database you have already seeded with
#
#   make bench BENCH_DATA_DIR=./data
#
# and it measures that instead of generating its own. internal/bench/RESULTS.md
# records what the numbers were when they were last taken.
bench:
	@go test ./internal/bench/ -run '^$$' -bench . -benchtime 1x -timeout 60m \
		$(if $(BENCH_DATA_DIR),-bench.data-dir $(BENCH_DATA_DIR),)

## test-integration: start everything, send an event, assert it landed
# Tagged so `make test` stays fast and hermetic. The tests themselves arrive with
# the ingest pipeline; the target exists now so CI and the docs have one name.
test-integration: build
	@go test -tags=integration -count=1 ./...

## test-ecosystem: every SDK's own tests, in whichever toolchains are installed
# Each package under ecosystem/ is destined for its own repository, so none of
# them is part of `go test ./...` and none of them can be tested by the Go
# toolchain alone. A missing toolchain is a note rather than a failure: nobody
# should need PHP, Python, Ruby and Node installed to work on the Go binary, and
# a target that fails on a machine without them is a target people stop running.
test-ecosystem:
	@./scripts/test-ecosystem.sh

## lint: go vet, plus golangci-lint when it is installed
# golangci-lint is not required to work on this project — `make app` has to run
# with nothing but the Go toolchain — so a missing binary is a note, not a
# failure. CI installs it, so it always runs there.
lint:
	@go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed — ran go vet only"; \
	fi

## check-env: fail if the source reads a variable .env.sample does not document
check-env:
	@./scripts/check-env.sh

## migrate: migrate control.db and every account database
migrate: build
	@$(APP_ENV) ./$(BINARY) db migrate

## migrate-fresh: drop everything and rebuild
migrate-fresh: build
	@$(APP_ENV) ./$(BINARY) db migrate --fresh

# ── Seed data ─────────────────────────────────────────────────────────────────
# Nothing in the product can be built or measured against an empty database, and
# the performance numbers in the plan stay estimates until something generates
# enough rows to time. The generator calls the same functions the ingest path
# calls and skips only the network.

## seed: realistic fake traffic across the whole fixture
seed: build
	@$(APP_ENV) ./$(BINARY) seed $(FRESH) \
		--pageviews $(PAGEVIEWS) --days $(DAYS) --sites $(SITES) --seed $(SEED)

## seed-big: one site, a million pageviews in a month
# The budget is two minutes. A twenty-minute seed is a seed nobody runs, and a
# dataset nobody generates measures nothing.
seed-big:
	@$(MAKE) --no-print-directory seed PAGEVIEWS=1000000 DAYS=30 SITES=1

## seed-http: a couple of hundred events over the real wire
# A different tool for a different job: correctness, not volume. It starts an
# ingest listener on an ephemeral loopback port, sends events through the real
# handler over real HTTP, then stops it — including when something fails.
# Point it at an instance you are already running with --url instead.
seed-http: build
	@$(APP_ENV) ./$(BINARY) seed --http --http-events $(HTTP_EVENTS)

# ── Processes, one per terminal ───────────────────────────────────────────────

## caddy: the reverse proxy, and the only port you open in a browser
caddy:
	@command -v caddy >/dev/null 2>&1 || { \
		echo "caddy is not installed — brew install caddy"; exit 1; }
	@echo "listening on $(BASE_URL)"
	@$(CADDY_ENV) caddy run --config Caddyfile --adapter caddyfile

## app: the dashboard, the API and the tracker
app: build
	@echo "listening on $(BASE_URL) (app on $(BIND_HOST):$(PORT_APP))"
	@$(APP_ENV) FEASIBLE_APP_TRANSPORT=http ./$(BINARY) serve

## ingest: the ingest tier
ingest: build
	@echo "listening on http://$(PUBLIC_HOST):$(PORT_INGEST)"
	@$(INGEST_ENV) ./$(BINARY) ingest

## testsite: a real page with the snippet installed, for exercising the tracker
testsite:
	@echo "listening on http://$(PUBLIC_HOST):$(PORT_SITE)"
	@go run ./cmd/testsite \
		-listen $(BIND_HOST):$(PORT_SITE) \
		-base-url $(BASE_URL) \
		-dir testsite

## dev: all three processes, http transport, production-shaped
dev: build
	@echo "listening on $(BASE_URL)"
	@$(MAKE) --no-print-directory -j3 \
		BIND_HOST=$(BIND_HOST) PUBLIC_HOST=$(PUBLIC_HOST) caddy app ingest

## dev-solo: one process, direct transport, the self-hoster path
# No Caddy and no ingest tier: this is what someone running the binary on their
# own box gets, and it has to be exercised as often as the production shape.
dev-solo: build
	@echo "listening on $(SOLO_BASE_URL)"
	@FEASIBLE_APP_LISTEN=$(BIND_HOST):$(PORT_APP) \
		FEASIBLE_APP_INTERNAL_LISTEN=$(INTERNAL_LISTEN) \
		FEASIBLE_APP_BASE_URL=$(SOLO_BASE_URL) \
		FEASIBLE_APP_TRANSPORT=direct \
		./$(BINARY) serve

# ── Tailscale twins ───────────────────────────────────────────────────────────
# These are the normal path, not a convenience: work often happens from another
# machine, and a server bound to loopback is invisible from there. Each one sets
# the bind address and the base URL together — see the note on BIND_HOST above
# for why they can never be set apart.

## require-tailscale: fail early, and usefully, when Tailscale is not up
require-tailscale:
	@if [ -z "$(TAILSCALE_IP)" ]; then \
		echo "No Tailscale IP found — is Tailscale running?"; \
		echo "Falling back to localhost is: make dev"; \
		exit 1; \
	fi

caddy-ts: BIND_HOST = $(TAILSCALE_IP)
caddy-ts: PUBLIC_HOST = $(TAILSCALE_HOST)
## caddy-ts: caddy, bound to the Tailscale address
caddy-ts: require-tailscale caddy
	@:

app-ts: BIND_HOST = $(TAILSCALE_IP)
app-ts: PUBLIC_HOST = $(TAILSCALE_HOST)
## app-ts: the app, bound to the Tailscale address
app-ts: require-tailscale app
	@:

ingest-ts: BIND_HOST = $(TAILSCALE_IP)
ingest-ts: PUBLIC_HOST = $(TAILSCALE_HOST)
## ingest-ts: the ingest tier, bound to the Tailscale address
ingest-ts: require-tailscale ingest
	@:

testsite-ts: BIND_HOST = $(TAILSCALE_IP)
testsite-ts: PUBLIC_HOST = $(TAILSCALE_HOST)
## testsite-ts: the test site, bound to the Tailscale address
testsite-ts: require-tailscale testsite
	@:

dev-ts: BIND_HOST = $(TAILSCALE_IP)
dev-ts: PUBLIC_HOST = $(TAILSCALE_HOST)
## dev-ts: all three processes, bound to the Tailscale address
dev-ts: require-tailscale dev
	@:

dev-solo-ts: BIND_HOST = $(TAILSCALE_IP)
dev-solo-ts: PUBLIC_HOST = $(TAILSCALE_HOST)
## dev-solo-ts: single-process mode, bound to the Tailscale address
dev-solo-ts: require-tailscale dev-solo
	@:
