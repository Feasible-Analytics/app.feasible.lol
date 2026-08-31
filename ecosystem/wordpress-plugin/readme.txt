=== feasible.lol Analytics ===
Contributors: cloudmanic
Tags: analytics, privacy, statistics, gdpr, cookieless
Requires at least: 5.9
Tested up to: 6.8
Requires PHP: 7.4
Stable tag: 1.0.0
License: GPL-2.0-or-later
License URI: https://www.gnu.org/licenses/gpl-2.0.html

Privacy-first analytics served from your own domain on randomised paths. Tracks site searches and
404s, and puts your dashboard inside wp-admin.

== Description ==

feasible.lol is cookieless, privacy-first web analytics. This plugin installs it on WordPress in one
screen, and serves the tracking script and the event endpoint **from your own domain, on paths
generated when you activate it**.

That last part is the point. A blocker's filter list names files, so a script called
`analytics.js` on your own domain goes on a list the week somebody notices it. A path nobody can
predict is not a path anybody can list — and when one finally is, rotating it is a button.

= What it measures =

* Pageviews, visitors, sessions, engagement time and scroll depth.
* **Site searches** — the term, how many results came back, and whether it found anything at all.
  WordPress knows this and a browser script cannot.
* **404 errors** — the path that was asked for, and where the visitor came from.
* Outbound link clicks, file downloads and form submissions, each of which you can switch off.

= What it does not do =

No cookies. No cross-site tracking. No personal data leaves your site beyond what the analytics
endpoint needs to count a visit, and the visitor's IP address is used to work out a country and is
then discarded rather than stored.

= The proxy =

Two routes are served from your domain on random segments generated at activation:

* the tracker script, cached for ten minutes and revalidated with an ETag, with a last known good
  copy kept for a day so an upstream outage costs you a slightly old script rather than no script;
* the event endpoint, which forwards each event upstream with the visitor's real IP and User-Agent.

Those two headers are the whole reason the event route exists. Without them the analytics server sees
your web host, and every visitor to your site is counted as one person in a datacentre.

With pretty permalinks the routes are real paths. With plain permalinks they fall back to a
query-string route, because a bare path never reaches WordPress without a rewrite rule in front of
it. The settings screen tells you which mode you are in.

== Installation ==

1. Upload the plugin folder to `/wp-content/plugins/`, or install it from the plugin directory.
2. Activate it through the **Plugins** screen.
3. Open **feasible.lol → Settings** and enter your site domain exactly as it is registered.
4. Leave the proxy switched on. The screen shows the two URLs it is now serving.
5. Choose which optional measurements you want.
6. Optional: paste a shared dashboard link to read your dashboard inside wp-admin.

== Frequently Asked Questions ==

= Do I need an account? =

Yes. The plugin sends events to a feasible.lol instance — the hosted service or your own
self-hosted binary. Both work, and the host is a setting.

= Will this get past ad blockers? =

It raises the cost, and that is the honest answer. Serving from your own domain on a path nobody can
predict defeats the list-based blocking that costs most sites their traffic data. It is not a way
around a visitor who has decided not to be counted, and it should never be sold as one.

= A path of mine got blocked. Now what? =

Press **Rotate paths**. Three new segments are generated and the old ones stop answering.

= Why do the outbound-link, download and form switches need the proxy? =

Those three are measured by the browser script itself, and nothing outside it can switch them off.
The proxy is the only place the control can honestly live: with it on, a suppressed event is answered
with a header saying it was your site's own choice. With it off, the script measures them regardless,
and the settings screen says so rather than showing you a switch that does nothing.

= Does it work with a caching plugin or a CDN? =

Yes. The script route sets its own cache headers and an ETag. The event route is a POST and is never
cached. If your CDN caches POST requests, that is a problem with the CDN.

= Can I exclude myself? =

Yes — switch on "exclude logged-in users", or exclude specific roles.

== Screenshots ==

1. The settings screen, showing the proxy URLs and the routing mode in use.
2. The measurements section.
3. The dashboard embedded in wp-admin.

== Changelog ==

= 1.0.0 =
* First release.
* Randomised-path proxy for the script and the event endpoint, served from your own domain.
* Automatic snippet injection, in the head or the footer.
* Site search and 404 tracking.
* Switches for outbound links, downloads and form submissions.
* The dashboard embedded in wp-admin through a shared link.
* Site Health checks for the configuration mistakes that otherwise fail silently.

== Upgrade Notice ==

= 1.0.0 =
First release.

== Copyright ==

readme.txt — the plugin directory listing for feasible.lol Analytics.

Created: 2026-08-30
Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.

Licensed under GPL-2.0-or-later. See LICENSE in this folder for the full text.

The plugin directory's readme format has no comment syntax, so the file header
every other file in this project carries at the top lives here instead.
