# kindling.lantern.io

The Kindling website: one static `index.html` with no build step, served by Cloudflare Pages.

`.github/workflows/site.yml` deploys this directory to Cloudflare Pages with wrangler on every push
to `main` that changes it, creating the Pages project on its first run. It needs the
`CLOUDFLARE_API_TOKEN` (Pages: Edit) and `CLOUDFLARE_ACCOUNT_ID` repository secrets. `_headers` sets
the response headers.

To preview locally, open `index.html` in a browser, or run `python3 -m http.server -d site`.
