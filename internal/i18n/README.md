<!--
  README.md
  How the message catalogue works, and how to add a language.

  Created: 2026-08-30
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Translations

Every user-facing string in the product lives here, in one catalogue, read by every front end.

```
locales/
  en/            the source language — always complete
    common.json    strings shared by every screen
    auth.json      sign-in, account, settings and site management
    pages.json     pricing, billing, checkout and the documentation shell
    dashboard.json the React dashboard
  de/            the same files, translated
```

## Why one catalogue and not two

The product has three rendering surfaces: Go templates for the account screens, Go templates for the
pricing, billing and documentation screens, and React for the stats screen. They read the same files.

The server negotiates the language, merges the chosen locale's strings over English, and hands the
result to whichever surface is answering — a template function for the Go screens, and the
dashboard's existing bootstrap blob for the React one. The merge happens once, on the server, so the
browser receives a map in which every id it can ask for is already present and needs no fallback
logic of its own.

Two catalogues would drift the first time somebody translated a button on one screen and not the
identical button on the other, and nothing would ever tell us. One catalogue also means the
completeness check is a single test rather than one per surface.

The strings travel with the page rather than being fetched, for the same reason the site list does:
they are needed before the first paint, and a round trip for them would put a frame of untranslated
interface in front of every load.

## The format

Flat JSON: a dotted id to a string. Nothing else — no nesting, no comments, no metadata.

```json
{
  "auth.login.title": "Sign in",
  "auth.login.subtitle": "Welcome back.",
  "auth.sites.count_one": "{count} site",
  "auth.sites.count_other": "{count} sites"
}
```

**Ids** are `<surface>.<screen>.<element>`, lowercase and dotted. They are stable: renaming one
orphans every translation of it.

**Placeholders** are named — `{email}`, `{count}` — never positional. A positional verb cannot be
reordered by a translator whose grammar puts the object first, and the result reads backwards in
exactly the languages nobody on the team can check. An unknown placeholder is left on the page with
its braces intact, so the gap is visible rather than silently blank.

**Never build a sentence out of two translated fragments** around a value. Word order is not
universal; a single string with a placeholder in it is the only shape a translator can work with.

**Plurals** are a suffix on the id — `_one` and `_other` — selected by `N()` / `n()` from a count
that is always available to the string as `{count}`. The categories currently implemented are the
ones the shipped languages use: French treats zero as singular, everything else splits on one.
Adding Polish, Russian or Arabic means adding `_few` and `_many` strings and the matching rule in
`pluralCategory`.

## Which language a request gets

In order: a `?lang=` query parameter, then the `feasible_lang` cookie, then the browser's
`Accept-Language` header, then English.

The query parameter beats the cookie so a link into a specific language works for a reader who has
already chosen a different one. The cookie beats `Accept-Language` because a deliberate choice must
outrank a browser default — a site that keeps resetting to the operating system's language is the
commonest complaint about language switching there is.

The choice is a cookie rather than a column on the user, because it has to work on the signed-out
screens too. Somebody who cannot read the sign-in page in their own language never gets far enough
to have a preference stored.

`Accept-Language` is parsed properly, q-values and all. The order of the tags in that header is not
the preference order: `en;q=0.2, de;q=0.9` asks for German, and reading it left to right serves
English.

## What ships today

**English is complete** and is the source language. It is the only one that is guaranteed to be.

**German** is a working second locale rather than a finished one. It covers the shared strings and
every account, sign-in, settings and site-management screen — the ones somebody hits before they
have decided whether to stay — and neither the stats dashboard nor the pricing and billing screens.
`Coverage("de")` is the honest number, and it is a little under a half: `common.json` and
`auth.json` are complete, `dashboard.json` and `pages.json` are not started. Anything it has not
translated renders in English on the same screen, which is what makes a partial locale worth shipping
at all.

It has **not been reviewed by a native speaker**, and it should be before German is advertised
anywhere.

The other tags in the `names` table — French, Spanish — are labels waiting for a catalogue. A tag
with no directory under `locales/` is not offered.

There is no language picker on a screen yet. Switching is `?lang=de` on any URL, which sets the
cookie and sticks. A picker is a control on somebody's screen rather than a piece of this package,
and it belongs in whichever screen ends up owning account preferences.

## Adding a language

1. `mkdir internal/i18n/locales/<tag>` and copy the `en/*.json` files into it.
2. Translate the values. Leave the ids alone. An id you have not done yet can be left out entirely —
   fallback is per string, so a locale with the sign-in screen finished shows a translated sign-in
   screen and English elsewhere. It does not have to be complete to be useful.
3. Add the tag to the `names` table in `i18n.go` with its English name and its name in itself. A
   language picker that lists "German" to somebody who only reads German is a picker they cannot use.
4. If the language needs plural categories beyond one and other, add its rule to `pluralCategory` and
   the extra `_few` / `_many` strings.
5. `go test ./internal/i18n/` — the tests check that the catalogue loads, that no two files claim the
   same id, and that every id a screen references has a string.

## What the tests enforce

- **Every id in use has a string.** The test scans the templates, the Go handlers and the TypeScript
  sources on all three surfaces for call sites and fails on any id with nothing behind it. A missing
  string renders as the raw id on a customer's screen, with a 200 and nothing in any log.
- **Every string is used.** The other direction matters too: a string nobody renders is a string a
  translator is paid to translate for a screen that does not exist.
- **A missing id is recorded**, not swallowed, so the gap is countable as well as visible.
- **A malformed locale stops the process at start-up**, which is where a broken catalogue belongs.
  The alternative is a binary that starts happily and renders message ids to whoever opens a page
  first.

## What is not here yet

Locale-aware number and date formatting on the server. The dashboard already goes through `Intl`
with the negotiated tag; the server-rendered screens format dates in one English format. That is a
gap, and it is a smaller one than untranslated text, which is why it is second.
