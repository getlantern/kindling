# kindling.lantern.io

The Kindling website: one static `index.html` with no build step, served by Cloudflare Pages.

Cloudflare Pages builds this directory straight from GitHub: every push to `main` deploys it, with
no build command and `site` as the output directory. `_headers` sets the response headers.

To preview locally, open `index.html` in a browser, or run `python3 -m http.server -d site`.
