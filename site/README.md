# kindling.lantern.io

The Kindling website: one static `index.html` with no build step, served by Cloudflare Pages.

Cloudflare Pages settings: framework preset **None**, build command empty, build output directory
`site`, production branch `main`. `_headers` sets the response headers.

To preview locally, open `index.html` in a browser, or run `python3 -m http.server -d site`.
