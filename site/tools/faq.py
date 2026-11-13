#!/usr/bin/env python3
"""The FAQ: one list that generates both the visible FAQ section and its
schema.org FAQPage markup in index.html, so the two can't drift apart.

Edit FAQ below, then run: python3 site/tools/faq.py
Answers are HTML (links allowed); the JSON-LD gets the same text with tags
stripped. The script also writes the rest of the JSON-LD graph.
"""
import html
import json
import re
from pathlib import Path

SITE = "https://kindling.lantern.io/"
REPO = "https://github.com/getlantern/kindling"

FAQ = [
    ("What is Kindling?",
     "Kindling is an open-source Go library from <a href=\"https://lantern.io\">Lantern</a> "
     "that sends and receives small amounts of data, like configuration files, through "
     "censoring firewalls. It races several circumvention techniques (domain fronting, "
     "proxyless dialing and Google's AMP cache, with the Soar DNS tunnel held back as a last "
     "resort) and sends the request over whichever connects first. Your code gets back an ordinary Go "
     "<code>http.Client</code>."),
    ("Is Kindling a VPN?",
     "No. Kindling doesn't carry your browsing. It gets a circumvention app's first "
     "requests, such as fetching its configuration or its list of servers, through a "
     "censored network, so the app can start its own tunnel. If you want a VPN, "
     "<a href=\"https://lantern.io/download\">download Lantern</a>."),
    ("Which circumvention techniques does Kindling use?",
     "Four, plus any you add. Domain fronting reaches the real origin through a large CDN "
     "under another server name. Proxyless dialing uses the Outline SDK's smart dialer, which "
     "tries DNS over HTTPS, TCP stream splitting and TLS record fragmentation to get past DNS "
     "and SNI blocking. The AMP cache transport uses Lantern's "
     "<a href=\"https://github.com/getlantern/amp\">amp</a> library to go through Google's AMP "
     "cache. <a href=\"https://soar.lantern.io\">Soar</a>, a DNS tunnel, is the last resort."),
    ("How does Kindling choose which technique to use?",
     "It doesn't choose in advance. Every transport in the first tier without a start delay "
     "begins connecting at the same time, so the race measures what actually works on the network the app is on, "
     "and the first to connect carries the request. If every transport in the tier fails, "
     "the next tier joins at once, and a transport with a start delay joins when its delay "
     "passes. <code>WithRaceObserver</code> reports how each race went: "
     "which transport won, and what happened to each one tried."),
    ("When does Kindling use the DNS tunnel?",
     "Only as a last resort. DNS tunnelling keeps working under heavy censorship but is slow, "
     "so Soar joins the race 8 seconds into a request, or as soon as every faster transport "
     "has failed, whichever comes first. The delay is there because censored networks often "
     "drop packets instead of refusing connections, so the faster transports can hang "
     "rather than fail. <code>WithStartDelay</code> changes it."),
    ("Will Kindling send a POST request twice?",
     "Not unless you say it's safe. A request that isn't safe to replay is sent once, on the first transport to "
     "connect, and its response is returned whatever it is. Failing to connect always moves "
     "on to the next transport, because no body was sent. GET and HEAD requests, and requests "
     "with the <code>X-Kindling-Idempotent</code> header, also move on after a transport "
     "error, a 5xx or a 403, so set that header only on a POST the server can safely receive "
     "twice. Other 4xx responses are returned as they are."),
    ("How much data can Kindling send?",
     "Kindling is built for small requests, such as config fetches and status checks, not "
     "bulk traffic. Each transport declares its own limits: the AMP cache takes request "
     "bodies up to 6,000 bytes and can't stream, so Kindling skips it for larger bodies and "
     "for streaming requests (those with <code>Accept: text/event-stream</code>)."),
    ("How does Kindling relate to the Outline SDK?",
     "Kindling's proxyless transport is the "
     "<a href=\"https://github.com/Jigsaw-Code/outline-sdk/tree/main/x/smart\">Outline SDK "
     "smart dialer</a>, which connects straight to the destination and searches for a DNS and "
     "TLS strategy that gets through. Kindling races it alongside domain fronting, the AMP "
     "cache and DNS tunnelling, so the request has other routes on networks where no "
     "proxyless strategy works."),
    ("Why not just use domain fronting?",
     "Each technique beats a different kind of blocking, and none works everywhere. A domain "
     "front fails if that CDN path is blocked or refuses the client's country. Proxyless "
     "dialing works best for widely used services whose addresses aren't blocked. The AMP "
     "cache only carries small requests. Kindling runs them together, so one blocked route "
     "doesn't fail the request."),
    ("How do I use Kindling in a Go program?",
     "Run <code>go get github.com/getlantern/kindling</code>, create a client with "
     "<code>kindling.NewKindling</code> and the transports you want "
     "(<code>WithDomainFronting</code>, <code>WithProxyless</code>, <code>WithAMPCache</code>, "
     "<code>WithDNSTunnel</code>), then call <code>NewHTTPClient()</code> and use the result "
     "like any <code>http.Client</code>. See <a href=\"#code\">the code examples</a>."),
    ("Can I add my own circumvention technique?",
     "Yes. Anything that can return a pre-connected <code>http.RoundTripper</code> can join "
     "the race: implement the <code>Transport</code> interface and pass it to "
     "<code>WithTransport</code>. Add <code>Priority() int</code> to race in a later tier, or "
     "<code>StartDelay() time.Duration</code> to join late. Pull requests for new techniques "
     "are welcome on <a href=\"" + REPO + "\">GitHub</a>."),
    ("Does Lantern use Kindling?",
     "Yes. Lantern's client backend, <a href=\"https://github.com/getlantern/radiance\">"
     "radiance</a>, uses Kindling to fetch its configuration. Kindling is meant for any "
     "circumvention tool written in Go that needs to fetch configuration reliably on "
     "startup."),
    ("Is Kindling open source?",
     "Yes. Kindling is written in Go and released under the Apache License 2.0 at "
     "<a href=\"" + REPO + "\">github.com/getlantern/kindling</a>."),
    ("Who makes Kindling?",
     "The <a href=\"https://lantern.io\">Lantern</a> team, at Brave New Software Project, "
     "Inc., which makes the Lantern VPN and censorship-circumvention app."),
]


def strip(s: str) -> str:
    return html.unescape(re.sub(r"<[^>]+>", "", s)).replace(" ", " ")


def faq_html() -> str:
    items = []
    for q, a in FAQ:
        slug = re.sub(r"[^a-z0-9]+", "-", q.lower()).strip("-")
        items.append(
            f'      <div class="qa" id="{slug}">\n'
            f'        <h3>{html.escape(q)}</h3>\n'
            f'        <p>{a}</p>\n'
            f'      </div>'
        )
    return "\n".join(items)


def faq_jsonld() -> dict:
    return {
        "@type": "FAQPage",
        "@id": SITE + "#faq",
        "isPartOf": {"@id": SITE + "#webpage"},
        "mainEntity": [
            {"@type": "Question", "name": q,
             "acceptedAnswer": {"@type": "Answer", "text": strip(a)}}
            for q, a in FAQ
        ],
    }


def graph() -> dict:
    org = {
        "@type": "Organization",
        "@id": "https://lantern.io/#organization",
        "name": "Lantern",
        "legalName": "Brave New Software Project, Inc.",
        "url": "https://lantern.io",
        "sameAs": ["https://github.com/getlantern"],
    }
    return {
        "@context": "https://schema.org",
        "@graph": [
            org,
            {
                "@type": "WebSite",
                "@id": SITE + "#website",
                "url": SITE,
                "name": "Kindling",
                "publisher": {"@id": org["@id"]},
                "inLanguage": "en",
            },
            {
                "@type": "WebPage",
                "@id": SITE + "#webpage",
                "url": SITE,
                "name": "Kindling: a Go library for getting through censoring firewalls",
                "isPartOf": {"@id": SITE + "#website"},
                "about": {"@id": SITE + "#software"},
                "publisher": {"@id": org["@id"]},
                "primaryImageOfPage": SITE + "og.png",
                "inLanguage": "en",
            },
            {
                "@type": "SoftwareSourceCode",
                "@id": SITE + "#software",
                "name": "Kindling",
                "description": strip(FAQ[0][1]),
                "codeRepository": REPO,
                "programmingLanguage": "Go",
                "license": "https://www.apache.org/licenses/LICENSE-2.0",
                "url": SITE,
                "author": {"@id": org["@id"]},
                "keywords": "censorship circumvention, domain fronting, Outline SDK, "
                            "AMP cache, DNS tunnel, Go library, bootstrapping",
            },
            faq_jsonld(),
        ],
    }


def main() -> None:
    p = Path(__file__).resolve().parent.parent / "index.html"
    s = p.read_text()
    s = re.sub(r"(<!-- faq:start -->).*?(\n[ \t]*<!-- faq:end -->)",
               lambda m: m.group(1) + "\n" + faq_html() + m.group(2), s, flags=re.S)
    ld = json.dumps(graph(), ensure_ascii=False, indent=1)
    s = re.sub(r'(<script type="application/ld\+json">\n).*?(\n</script>)',
               lambda m: m.group(1) + ld + m.group(2), s, flags=re.S)
    p.write_text(s)


if __name__ == "__main__":
    main()
