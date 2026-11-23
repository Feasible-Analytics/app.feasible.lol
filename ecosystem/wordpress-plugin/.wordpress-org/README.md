<!--
  README.md
  The plugin directory's page art: what each file is, and how it reaches wordpress.org.

  Created: 2026-11-23
  Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
-->

# Plugin directory art

These files dress the plugin's page at https://wordpress.org/plugins/feasible-analytics/. They are
never in the plugin zip: the directory reads them from the top-level `assets/` folder of the SVN
repository, beside `trunk/` and `tags/`, not from inside the plugin.

| File | What it is |
|---|---|
| `icon.svg`, `icon-128x128.png`, `icon-256x256.png` | The icon in search results and on the page. The directory uses the SVG where it can. |
| `banner-772x250.png`, `banner-1544x500.png` | The page header, normal and high-density. Rendered from `banner.html`. |
| `screenshot-1.png` to `screenshot-5.png` | The gallery. Each caption is the matching numbered line under `== Screenshots ==` in `readme.txt`. |

The screenshots are real screens. 1, 2 and 4 are the plugin on a test WordPress. 3 is the
Dashboard page framing a shared link to the public demo site, whose numbers are generated, and the
caption says so. 5 is the Feasible report for that test WordPress site after a few visits.

## Publishing a change

Copy the files into the SVN checkout's `assets/` folder and commit. PNGs need their MIME type set,
or a browser downloads them instead of showing them:

```bash
svn co https://plugins.svn.wordpress.org/feasible-analytics svn-feasible
cp .wordpress-org/*.png .wordpress-org/icon.svg svn-feasible/assets/
cd svn-feasible
svn add --force assets
svn propset svn:mime-type image/png assets/*.png
svn propset svn:mime-type image/svg+xml assets/icon.svg
svn ci -m "Update the plugin directory art" --username spicermatthews
```

The page picks up new art within a few minutes. Browsers and the directory's CDN can hold the old
images longer.
