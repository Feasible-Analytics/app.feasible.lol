=== Feasible Analytics ===
Contributors: spicermatthews
Tags: analytics, privacy, statistics, cookieless, site search
Requires at least: 5.9
Tested up to: 7.1
Requires PHP: 7.4
Stable tag: 1.0.1
License: GPLv2 or later
License URI: https://www.gnu.org/licenses/gpl-2.0.html

Cookieless analytics for WordPress, served from your own domain. Counts site searches and 404s too.

== Description ==

Feasible is web analytics with no cookies. This plugin connects a WordPress site to it from one settings screen. You don't edit a theme file.

You need a Feasible account, or your own self-hosted copy of Feasible. The plugin stores no statistics in your WordPress database.

Nothing happens when you activate the plugin. It adds nothing to your pages and sends nothing anywhere until you confirm your site domain and turn tracking on in **Feasible → Settings**.

= What it does =

* Adds the tracking script to every page, in the head or the footer, once you turn tracking on.
* Serves the tracking script, which ships inside the plugin, and the event endpoint from your own domain, on random paths generated for your site.
* Counts site searches: the term, how many results came back, and whether it found anything.
* Counts 404 errors: the path that was asked for and where the visitor came from.
* Lets you switch off outbound link clicks, file downloads and form submissions, one by one.
* Leaves out logged-in users, or only the roles you pick.
* Shows your dashboard inside wp-admin when you paste a shared dashboard link.
* Adds Site Health checks for the setup mistakes that otherwise fail without a word.

= What Feasible measures =

Visitors, visits, pageviews, bounce rate, visit duration and scroll depth. Sources, pages, countries, devices, browsers and operating systems. Outbound link clicks, file downloads and form submissions.

= What it does not do =

No cookies. No cross-site tracking. The visitor's IP address is used to work out a location and is then discarded. It is never written to disk.

= The proxy =

The proxy is on by default. With it on, two routes are served from your domain on random path segments:

* The tracker script. It is the file in the plugin's `assets/js` folder, served with an ETag. The plugin never downloads code from anywhere.
* The event endpoint. It checks each event, keeps only the fields Feasible accepts, cleans and size-limits each one, and forwards the rebuilt event to your Feasible host with the visitor's IP address and User-Agent.

The event route needs those two headers. Without them the analytics server sees your web host, and every visitor is counted as one person in a datacenter.

With pretty permalinks the routes are real paths. With plain permalinks they fall back to a query-string route, because a bare path never reaches WordPress without a rewrite rule in front of it. The settings screen tells you which mode you are in.

You can turn the proxy off. The snippet then loads the script from your Feasible host directly, and visitors' browsers send events there.

= Source code =

The tracking script in `assets/js/feasible.js` is minified. Its source, and the build that produces it, are in the `tracker` folder of the Feasible repository: https://github.com/Feasible-Analytics/app.feasible.lol/tree/main/tracker. The script is licensed AGPL-3.0-or-later. The plugin's own source is in the same repository under `ecosystem/wordpress-plugin`.

== External services ==

This plugin connects your site to Feasible, a web analytics service. It does nothing useful without it. The service is the hosted one at https://app.feasible.lol unless you change the host setting to your own self-hosted copy.

Nothing is sent until you turn tracking on. Activating the plugin contacts no server. Until you enter your site domain and tick "Turn on tracking" in the settings, the plugin adds no script to your pages and forwards nothing.

What is sent once tracking is on, and when:

* **On each page a visitor loads,** the visitor's browser runs the Feasible script. It sends the page URL, the referrer, the page title, the browser window width and the site domain you entered. Later it sends how long the visitor stayed and how far they scrolled.
* **When a visitor clicks an outbound link, downloads a file or submits a form,** the script sends an event with the link or file URL. The form event carries no field values. You can switch each of these off.
* **When a visitor searches your site or lands on a 404,** the plugin sends the search term and result count, or the missing path.
* **With the proxy on** (the default), the script comes from the plugin itself and those requests go to your own site first. Your server then forwards each event to the Feasible host, with the visitor's IP address and User-Agent. It never downloads code from the host.
* **With the proxy off,** the visitor's browser loads the script from the Feasible host and sends events to it directly. The host sees the visitor's IP address and User-Agent as any web server does.

Feasible uses the IP address and User-Agent to count a visitor and to work out a country, region and city. The IP address is then discarded and never stored. No cookies are set.

The dashboard page in wp-admin loads your shared dashboard link from the Feasible host in a frame. It does that only after you paste a link.

The hosted service is run by Cloudmanic Labs, LLC. [Terms of service](https://feasible.lol/terms/). [Privacy policy](https://feasible.lol/privacy/).

== Installation ==

1. Install the plugin from the plugin directory, or upload the folder to `/wp-content/plugins/`.
2. Activate it on the **Plugins** screen.
3. Open **Feasible → Settings**. Check the site domain matches the site in your Feasible account.
4. Tick **Turn on tracking** and save. Until you do, the plugin adds nothing to your pages.
5. Leave the proxy switched on. The screen shows the two URLs it now serves.
6. Choose the optional measurements you want.
7. Optional: paste a shared dashboard link to read your dashboard inside wp-admin.

== Frequently Asked Questions ==

= Do I need an account? =

Yes. The plugin sends events to Feasible, either the hosted service or your own self-hosted copy. Both work, and the host is a setting. The hosted service is paid, with a 30-day free trial. Self-hosting is free.

= Does it set cookies? =

No.

= Does activating the plugin start tracking? =

No. Tracking stays off until you confirm your site domain and tick "Turn on tracking" in the settings.

= Why serve the script from my own domain? =

Some browsers and extensions refuse requests to third-party analytics hosts. Served from your own domain, the script and its events are first-party requests. It does not get around a visitor who has chosen not to be counted, and it isn't meant to.

= What does Rotate paths do? =

It generates three new random segments. The old ones stop answering.

= Why do the outbound link, download and form switches need the proxy? =

Those three are measured by the browser script itself. The proxy is where the plugin can stop them: with it on, a switched-off event is answered with a header saying your site chose not to send it. With it off, the script measures them regardless, and the settings screen says so.

= Does it work with a caching plugin or a CDN? =

Yes. The script route sets its own cache headers and an ETag. The event route is a POST and should never be cached.

Don't add the script to a "delay JavaScript" list in your caching plugin. It is already deferred, and delaying it again misses visitors who read one page and leave.

= Can I exclude myself? =

Yes. Switch on "exclude logged-in users", or exclude specific roles.

= Does it work on multisite? =

Yes. Each site has its own settings and its own paths.

== Changelog ==

= 1.0.1 =
* Activation no longer turns tracking on or fills in the site domain. Confirm the domain and turn tracking on in the settings.
* The tracking script now ships inside the plugin. The proxy serves that file and no longer fetches or caches the script from the Feasible host.
* The proxy checks every event, keeps only the fields Feasible accepts, sanitizes and size-limits each one, and forwards the rebuilt event.
* Request headers the proxy reads are sanitized.

= 1.0.0 =
* First release.

== Upgrade Notice ==

= 1.0.1 =
Tracking is now off until you turn it on in Feasible → Settings. If you used 1.0.0, open the settings, check the domain and tick "Turn on tracking".

= 1.0.0 =
First release.
