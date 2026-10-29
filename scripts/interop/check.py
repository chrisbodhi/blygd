#!/usr/bin/env python3
"""Live interop check: blygd and the reference client, both ways.

Run by run.sh, which starts both studios and sets:
  BLYGD_URL      blygd host root, e.g. http://127.0.0.1:18090
  BLYGD_ORIGIN   the test user's origin, e.g. http://127.0.0.1:18090/alice/
  BLYGD_TOKEN    that user's owner token
  REF_URL        the reference's origin, e.g. http://127.0.0.1:18787/
  REF_CONTROL    the reference's cron control, e.g. http://127.0.0.1:18788/
  REF_PASSWORD   the reference's owner password

Each scenario publishes on one side and checks what the other side made of
it. Waits are conditions with a deadline, never fixed sleeps. Exit status
is the number of failed checks (capped at 1). A gap is a check of
something the spec lets the reference not do yet: it is reported, and
never fails the run.

Scenario 6 needs a third origin whose surface is relocated (§6.1), which
neither studio publishes, so this script serves one itself (SOL_PORT,
default 18789).
"""
import hashlib
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

BLYGD = os.environ["BLYGD_URL"].rstrip("/")
ALICE = os.environ["BLYGD_ORIGIN"]
TOKEN = os.environ["BLYGD_TOKEN"]
REF = os.environ["REF_URL"]
CONTROL = os.environ["REF_CONTROL"]
REF_PASSWORD = os.environ.get("REF_PASSWORD", "interop-password")
DEADLINE = 45  # seconds for any one condition

SOL_PORT = int(os.environ.get("SOL_PORT", "18789"))
failures = []
gaps = []


def check(name, ok, detail=""):
    print(("  PASS " if ok else "  FAIL ") + name + ("" if ok else f"\n       {detail}"))
    if not ok:
        failures.append(name)
    return ok


def gap(name, ok, why):
    """A check the reference may fail without failing the run: why says
    what it does not do yet."""
    print(("  PASS " if ok else "  GAP  ") + name + ("" if ok else f"\n       {why}"))
    if not ok:
        gaps.append(name)
    return ok


def section(title):
    print(f"\n== {title}")


# ── HTTP ──────────────────────────────────────────────────────────────────

class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def request(method, url, body=None, headers=None, form=False, follow=True):
    h = dict(headers or {})
    data = None
    if body is not None:
        if form:
            data = urllib.parse.urlencode(body).encode()
            h["Content-Type"] = "application/x-www-form-urlencoded"
        else:
            data = json.dumps(body).encode()
            h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, method=method, headers=h)
    opener = urllib.request.build_opener() if follow else urllib.request.build_opener(_NoRedirect)
    try:
        with opener.open(req, timeout=60) as r:
            raw, code, hdrs = r.read(), r.status, r.headers
    except urllib.error.HTTPError as e:
        raw, code, hdrs = e.read(), e.code, e.headers
    try:
        return code, json.loads(raw), hdrs
    except ValueError:
        return code, raw.decode(errors="replace"), hdrs


def blygd(method, path, body=None):
    code, j, _ = request(method, BLYGD + "/api" + path, body, {"Authorization": "Bearer " + TOKEN})
    return code, j


SESSION = {}


def ref_login():
    # The session cookie is Secure, which a cookie jar over plain http
    # drops, so it is carried by hand.
    code, _, hdrs = request("POST", REF + "studio/login", {"password": REF_PASSWORD}, form=True, follow=False)
    SESSION["cookie"] = (hdrs.get("Set-Cookie") or "").split(";")[0]
    if not SESSION["cookie"]:
        sys.exit(f"reference login failed ({code})")


def ref(method, path, body=None):
    code, j, _ = request(method, REF.rstrip("/") + path, body,
                         {"Origin": REF.rstrip("/"), "Cookie": SESSION.get("cookie", "")})
    return code, j


def get(url):
    code, j, _ = request("GET", url)
    return code, j


def ref_tick():
    """The reference's quarter-hour cron: its poll and outbound drain."""
    urllib.request.urlopen(CONTROL + "?cron=" + urllib.parse.quote("*/15 * * * *"), timeout=60).read()
    ref("POST", "/api/subscriptions/poll", {})


def blygd_poll(sid):
    """POST …/poll, honoring Retry-After: the floor and backoff are real."""
    for _ in range(10):
        code, j, hdrs = request("POST", f"{BLYGD}/api/subscriptions/{sid}/poll", None,
                                {"Authorization": "Bearer " + TOKEN})
        if code != 429:
            return code, j
        time.sleep(min(int(hdrs.get("Retry-After") or 1), 10))
    return code, j


def wait(what, fn, nudge=None, evidence=None, deadline=DEADLINE):
    """Until fn() is truthy, nudging between tries, or the deadline. On a
    timeout, evidence() says what the two sides hold."""
    end = time.time() + deadline
    while True:
        v = fn()
        if v:
            check(what, True)
            return v
        if time.time() > end:
            check(f"{what} (within {deadline}s)", False, "timed out" + (": " + json.dumps(evidence(), indent=1)[:3000] if evidence else ""))
            return v
        if nudge:
            nudge()
        time.sleep(1)


def items(j):
    """The reference's list endpoints page under "items"."""
    return j["items"] if isinstance(j, dict) and "items" in j else j


# ── Sol: a static blyg with a relocated surface ──────────────────────────

class Sol:
    """One pinned fragment at http://127.0.0.1:SOL_PORT/sol/, laid out
    nowhere near the defaults (§6.1). Both studios subscribe through a home
    page off the origin whose rel="blyg" link names /sol/manifest.json, so
    resolution takes §12.1 step 4 and the identity comes from that URL
    (§12.2); the same manifest is also at /sol/blyg.json, the one fixed
    name (§4), where a studio that holds only the identity looks. Item
    documents and pins are templated, one relative and one absolute; the
    index and feed are renamed; and the item names no page, so a mention's
    target is its item document (§15.2, #85). The webmention endpoint
    records what it is sent and verifies nothing."""

    def __init__(self, port):
        self.host = f"http://127.0.0.1:{port}"
        self.origin = self.host + "/sol/"
        self.home = self.host + "/home/"
        self.id = "01ks0re1c47edsvrface000001"
        self.md = "Sol keeps its files where it likes.\n\nThe manifest says where."
        self.received = []  # (source, target)
        self.hits = {}
        stamp = "2026-10-01T00:00:00Z"
        html = "<p>Sol keeps its files where it likes.</p>\n<p>The manifest says where.</p>"
        hash_ = "sha256:" + hashlib.sha256(self.md.encode()).hexdigest()
        doc = {"blyg": "0.4", "id": self.id, "kind": "fragment", "origin": self.origin,
               "created": stamp, "updated": stamp, "version": 1, "content_md": self.md,
               "content_html": html, "content_hash": hash_, "media": [],
               "changelog": [{"version": 1, "at": stamp, "note": None, "pinned": True}]}
        pin = {"blyg": "0.4", "id": self.id, "kind": "fragment", "version": 1, "at": stamp,
               "note": None, "pinned": True, "origin": self.origin, "content_md": self.md,
               "content_html": html, "content_hash": hash_}
        self.item_url = f"{self.origin}docs/{self.id}.json"
        self.pin_url = f"{self.origin}docs/{self.id}/pinned-1.json"
        manifest = {"blyg": "0.4", "level": 1, "site": self.origin, "title": "Sol, relocated",
                    "feed": "updates.rss", "items": "docs/all.json", "item": "docs/{id}.json",
                    "pin": self.origin + "docs/{id}/pinned-{n}.json", "webmention": self.origin + "wm"}
        feed = (f'<?xml version="1.0" encoding="UTF-8"?><rss version="2.0" xmlns:blyg="https://blygger.org/ns/0.1">'
                f'<channel><title>Sol, relocated</title><link>{self.origin}</link>'
                f'<blyg:manifest>{self.origin}manifest.json</blyg:manifest>'
                f'<item><guid isPermaLink="false">blyg:{self.id}:v1</guid><link>{self.item_url}</link>'
                f'<title>Sol</title><description></description><blyg:id>{self.id}</blyg:id>'
                f'<blyg:kind>fragment</blyg:kind><blyg:version>1</blyg:version></item></channel></rss>')
        self.files = {
            "/home/": ("text/html", '<!doctype html><title>Sol</title><link rel="blyg" href="/sol/manifest.json"><p>Sol.</p>'),
            "/sol/manifest.json": ("application/json", json.dumps(manifest)),
            "/sol/blyg.json": ("application/json", json.dumps(manifest)),
            "/sol/updates.rss": ("application/rss+xml", feed),
            "/sol/docs/all.json": ("application/json", json.dumps({"updated": stamp, "items": [
                {"id": self.id, "kind": "fragment", "created": stamp, "updated": stamp, "version": 1}]})),
            f"/sol/docs/{self.id}.json": ("application/json", json.dumps(doc)),
            f"/sol/docs/{self.id}/pinned-1.json": ("application/json", json.dumps(pin)),
        }

    def serve(self, port):
        sol = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                path = self.path.split("?")[0]
                sol.hits[path] = sol.hits.get(path, 0) + 1
                f = sol.files.get(path)
                if not f:
                    self.send_response(404)
                    self.end_headers()
                    return
                body = f[1].encode()
                self.send_response(200)
                self.send_header("Content-Type", f[0])
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_HEAD(self):
                self.send_response(404 if self.path.split("?")[0] not in sol.files else 200)
                self.end_headers()

            def do_POST(self):
                if self.path != "/sol/wm":
                    self.send_response(404)
                    self.end_headers()
                    return
                form = urllib.parse.parse_qs(self.rfile.read(int(self.headers.get("Content-Length") or 0)).decode())
                sol.received.append((form.get("source", [""])[0], form.get("target", [""])[0]))
                self.send_response(202)
                self.end_headers()

        server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()

    def default_path_hits(self):
        """Requests for where a default layout would put Sol's files."""
        return {p: n for p, n in self.hits.items() if p.startswith(("/sol/items/", "/sol/feed.xml"))}


# ── scenarios ─────────────────────────────────────────────────────────────

def main():
    ref_login()
    ref("PATCH", "/api/settings", {"site_title": "Rae on the reference", "site_url": REF, "author_name": "Rae"})

    def ref_publish(md, kind="fragment", **extra):
        code, j = ref("POST", "/api/items", {"content_md": md, "kind": kind, **extra})
        assert code == 201, (code, j)
        code, p = ref("POST", f"/api/items/{j['id']}/publish", {})
        assert code in (200, 201), (code, p)
        return j["id"]

    def blygd_publish(md, kind="fragment"):
        code, j = blygd("POST", "/items", {"content_md": md, "kind": kind})
        assert code == 201, (code, j)
        code, p = blygd("POST", f"/items/{j['id']}/publish")
        assert code == 200, (code, p)
        return j["id"], p

    # 1 ─ the reference publishes; blygd subscribes and imports.
    section("1. blygd imports the reference")
    r1 = ref_publish("Stigmergy is what a protocol looks like from inside.\n\nSecond *paragraph* & more.")
    ref("PUT", f"/api/items/{r1}/versions/1/pin", {})
    r2 = ref_publish("An unpinned thought, with a [link](https://example.com/).")
    rt = ref_publish(f"Ref thread.\n\n![[{r1}]]\n> a protocol looks like from inside.\n\nMy own words after.", "thread")
    code, sub = blygd("POST", "/subscriptions", {"url": REF})
    check("subscribe resolves the reference", code == 201, sub)
    sid = sub["subscription"]["id"]
    s = wait("blygd imports all three", lambda: (blygd("GET", f"/subscriptions/{sid}")[1].get("items") == 3) or None,
             lambda: blygd_poll(sid))
    code, s = blygd("GET", f"/subscriptions/{sid}")
    check("no discrepancies (content_hash verified)", s["discrepancies"] == 0 and not s["last_error"], s)
    code, it = blygd("GET", f"/reader/items/{sid}/{r1}")
    check("the reference's pin is retained", [p["version"] for p in it["item"].get("pins") or []] == [1], it)
    check("the reference's own page is kept", it["item"]["page"] == f"{REF}f/{r1}/", it["item"]["page"])

    # 2 ─ blygd quotes the reference; the reference verifies the mentions.
    section("2. blygd quotes the reference")
    md = (f"Alice replies, linking [[{r2}]].\n\n![[{r1}]]\n> Second paragraph & more.\n\n"
          f"![[{r2}]]\n\n![[{rt}]]\n\nDone.")
    at, res = blygd_publish(md, "thread")
    check("unpinned remote quotes are warned about (§10.2)", len(res.get("warnings") or []) == 2, res.get("warnings"))
    _, doc = get(f"{ALICE}items/{at}.json")
    h = doc["content_html"]
    check("remote bakes carry data-blyg-origin",
          h.count(f'data-blyg-origin="{REF}"') == 3, h)
    check("the nested own-origin bake keeps no origin attribute",
          f'<blockquote class="blyg-transclusion blyg-partial" data-blyg-id="{r1}" data-blyg-version="1">' in h, h)
    check("the [[id]] link is the reference's page", f'href="{REF}f/{r2}/"' in h, h)
    check("each remote entry has a frozen cited from the manifest title",
          all((t.get("cited") or {}).get("source") == "Rae on the reference" for t in doc["transclusions"]), doc["transclusions"])
    def alice_quotes():
        ms = [m for m in items(ref("GET", "/api/mentions")[1])
              if m["source"].startswith(ALICE) and m["status"] == "verified" and m["relation"] == "transclusion"]
        return ms if len(ms) >= 3 else None
    got = wait("the reference verifies blygd's three quotes", alice_quotes)
    check("three verified transclusion mentions", got is not None and len(got) == 3, got)

    # 3 ─ the reference subscribes to blygd, quotes it back, and stubs it.
    section("3. the reference imports, quotes and stubs blygd")
    a1, _ = blygd_publish("Alice's claim: *stigmergy* beats planning.\n\nWith a second line, & an ampersand.")
    blygd("POST", f"/items/{a1}/versions/1/pin")
    code, s = ref("POST", "/api/subscriptions", {"url": ALICE, "confirm": True})
    check("the reference subscribes to a path-mounted blygd origin", code in (200, 201) and s.get("origin") == ALICE, s)
    ref_sub = s["id"]
    wait("the reference imports alice's items",
         lambda: all(ref("GET", f"/api/imports/{ref_sub}/{i}")[0] == 200 for i in (a1, at)) or None, ref_tick)
    rq = ref_publish(f"Rae answers [[{a1}]].\n\n![[{a1}]]\n> stigmergy beats planning.\n\n![[{at}]]\n\nCycle closed.", "thread")
    rs = ref_publish(f"![[{a1}]]\n\nI disagree.", "thread", stub_of={"origin": ALICE, "id": a1, "version": 1})
    _, rdoc = get(f"{REF}items/{rq}.json")
    check("the reference's partial check passes over blygd's HTML",
          (rdoc["transclusions"][0].get("selector") or {}).get("exact") == "stigmergy beats planning.", rdoc["transclusions"])
    check("the quote cycle bakes finitely, three layers deep",
          rdoc["content_html"].count('class="blyg-transclusion') == 6, rdoc["content_html"])
    want = {("stub", a1), ("transclusion", a1), ("transclusion", at)}
    got = wait("blygd verifies the reference's stub and quotes",
               lambda: (lambda ms: ms if want <= ms else None)(
                   {(m["relation"], m["target_item_id"]) for m in blygd("GET", "/mentions")[1]["mentions"]
                    if m["status"] == "verified" and m["source_url"].startswith(REF)}), ref_tick)
    check("stub and transclusion mentions verified by blygd", bool(got), got)

    # 4 ─ forks of the same pinned thread, made by each side.
    section("4. forks")
    ref("PUT", f"/api/items/{rt}/versions/1/pin", {})
    blygd("POST", f"/items/{at}/versions/1/pin")
    forks = {}
    for name, origin, iid in (("rt", REF, rt), ("at", ALICE, at)):
        src = {"origin": origin, "id": iid, "version": 1}
        cb, b = blygd("POST", "/items", {"forked_from": src})
        cr, r = ref("POST", "/api/items", {"mode": "fork", "source": src})
        rmd = r.get("content_md") or r.get("draft_md") or r.get("working_md")
        check(f"both fork the pinned {name} (201/201)", cb == 201 and cr == 201, (cb, b, cr, r))
        check(f"the flattened forks of {name} are byte-identical", b.get("draft_md") == rmd,
              f"\nblygd:\n{b.get('draft_md')}\nreference:\n{rmd}")
        forks[name] = (b["id"], r["id"])
    blygd("POST", f"/items/{forks['rt'][0]}/publish")
    ref("POST", f"/api/items/{forks['at'][1]}/publish", {})
    wait("the reference verifies blygd's fork",
         lambda: any(m["relation"] == "fork" and m["status"] == "verified" for m in items(ref("GET", "/api/mentions")[1])) or None)
    wait("blygd verifies the reference's fork",
         lambda: any(m["relation"] == "fork" and m["status"] == "verified"
                     for m in blygd("GET", "/mentions")[1]["mentions"]) or None, ref_tick)
    _, fdoc = get(f"{ALICE}items/{forks['rt'][0]}.json")
    check("blygd's fork carries forked_from and no transclusions (§5.6 rule 6)",
          fdoc["forked_from"]["id"] == rt and fdoc["transclusions"] == [], fdoc)

    # 5 ─ withdrawal across the boundary.
    section("5. withdrawal")
    ref("POST", f"/api/items/{rs}/withdraw", {})
    ref("POST", f"/api/items/{r2}/withdraw", {})
    blygd("POST", f"/items/{at}/withdraw")
    wait("blygd marks the withdrawn stub's mention gone",
         lambda: any(m["relation"] == "stub" and m["status"] == "gone" for m in blygd("GET", "/mentions")[1]["mentions"]) or None,
         ref_tick)
    wait("blygd rolls the withdrawn import to null",
         lambda: blygd("GET", f"/reader/items/{sid}/{r2}")[1]["item"]["state"] == "withdrawn" or None,
         lambda: (ref_tick(), blygd_poll(sid)))
    code, it = blygd("GET", f"/reader/items/{sid}/{r2}")
    check("a withdrawn import keeps no content (§13.4)", it["item"]["doc"] is None, it)
    # The re-sends follow the first sends within seconds, so the reference
    # refuses them for its 60 s pair cooldown (429, Retry-After); blygd
    # comes back when told, at least a minute later.
    wait("the reference marks blygd's withdrawn quotes gone", deadline=150, fn=
         lambda: all(m["status"] == "gone" for m in items(ref("GET", "/api/mentions")[1])
                     if at in m["source"] and m["relation"] == "transclusion") or None, nudge=ref_tick,
         evidence=lambda: {"blygd outbound": [(o["source_item_id"], o["relation"], o["status"], o["last_result"]) for o in blygd("GET", "/outbound")[1]["outbound"]],
                  "reference mentions": [(m["source"], m["relation"], m["status"], m.get("error")) for m in items(ref("GET", "/api/mentions")[1])]})
    wait("the reference rolls blygd's withdrawn thread to null",
         lambda: ref("GET", f"/api/imports/{ref_sub}/{at}")[1].get("state") == "tombstone" or None, ref_tick)
    _, rdoc = get(f"{REF}items/{rq}.json")
    check("the reference's thread keeps its bake of the withdrawn thread (§10.4)", at in rdoc["content_html"], "")
    blygd("PATCH", f"/items/{at}", {"content_md": f"Back.\n\n![[{r2}]]"})
    code, p = blygd("POST", f"/items/{at}/publish")
    check("blygd refuses to quote the withdrawn import",
          code == 422 and any("withdrawn" in x["reason"] for x in p.get("problems", [])), (code, p))

    # 6 ─ both sides read, quote and fork a relocated surface.
    section("6. a relocated surface (§6.1)")
    sol = Sol(SOL_PORT)
    sol.serve(SOL_PORT)
    code, sub = blygd("POST", "/subscriptions", {"url": sol.home})
    check("blygd resolves Sol through its rel=\"blyg\" link", code == 201 and sub["subscription"].get("origin") == sol.origin, sub)
    bsol = sub["subscription"]["id"]
    wait("blygd imports Sol's item",
         lambda: (blygd("GET", f"/subscriptions/{bsol}")[1].get("items") == 1) or None, lambda: blygd_poll(bsol),
         evidence=lambda: {"subscription": blygd("GET", f"/subscriptions/{bsol}")[1], "sol hits": sol.hits})
    code, it = blygd("GET", f"/reader/items/{bsol}/{sol.id}")
    check("blygd retains Sol's pin from its pin template", [p["version"] for p in (it.get("item") or {}).get("pins") or []] == [1], it)
    code, s = ref("POST", "/api/subscriptions", {"url": sol.home, "confirm": True})
    check("the reference resolves Sol through its rel=\"blyg\" link", code in (200, 201) and s.get("origin") == sol.origin, s)
    rsol = s["id"]
    wait("the reference imports Sol's item", lambda: ref("GET", f"/api/imports/{rsol}/{sol.id}")[0] == 200 or None, ref_tick)

    sq, _ = blygd_publish(f"Alice quotes Sol.\n\n![[{sol.id}]]", "thread")
    rsq = ref_publish(f"Rae quotes Sol.\n\n![[{sol.id}]]", "thread")
    src = {"origin": sol.origin, "id": sol.id, "version": 1}
    cb, b = blygd("POST", "/items", {"forked_from": src})
    cr, r = ref("POST", "/api/items", {"mode": "fork", "source": src})
    rmd = r.get("content_md") or r.get("draft_md") or r.get("working_md")
    check("both fork Sol's pin (201/201)", cb == 201 and cr == 201, (cb, b, cr, r))
    check("the flattened forks of Sol are byte-identical", b.get("draft_md") == rmd,
          f"\nblygd:\n{b.get('draft_md')}\nreference:\n{rmd}")
    blygd("POST", f"/items/{b['id']}/publish")
    ref("POST", f"/api/items/{r['id']}/publish", {})
    for who, prefix, quote, fork in (("blygd", ALICE, sq, b["id"]), ("the reference", REF, rsq, r["id"])):
        for what, iid in (("quote", quote), ("fork", fork)):
            wait(f"{who}'s {what} reaches Sol's endpoint, naming its item document (#85)",
                 lambda: any(s_.startswith(prefix) and iid in s_ and t == sol.item_url for s_, t in sol.received) or None,
                 ref_tick, evidence=lambda: sol.received)
    check("nobody looked for Sol's files at the default paths", not sol.default_path_hits(), sol.default_path_hits())

    # 7 ─ the 0.4 directive forms: labels, links[] and remote sources.
    section("7. labels, links and remote generation sources")
    r3 = ref_publish("Rae's third: a link carries none of my words.")
    r4 = ref_publish("Rae's fourth: models eat words and should say whose.")
    wait("blygd imports Rae's new items",
         lambda: all(blygd("GET", f"/reader/items/{sid}/{i}")[0] == 200 for i in (r3, r4)) or None,
         lambda: blygd_poll(sid))
    al, _ = blygd_publish(
        f"Alice links [[{r3} | Rae on notices]].\n\n![[{r1} | the opening]]\n\n"
        f"[TK]summarize ![[{r4}]][=]Models should say whose words they ate.[/TK]\n", "thread")
    _, doc = get(f"{ALICE}items/{al}.json")
    check("links[] names the linked item, and only it (§10.3a)", doc.get("links") == [{"origin": REF, "id": r3}], doc.get("links"))
    check("the link label is the anchor text", f'href="{REF}f/{r3}/">Rae on notices</a>' in doc["content_html"], doc["content_html"])
    check("the directive label is baked as data-blyg-label", 'data-blyg-label="the opening"' in doc["content_html"], doc["content_html"])
    srcs = [s_ for g in doc.get("generated") or [] for s_ in g.get("sources") or []]
    check("the remote source carries its origin and a frozen cited (§5.7 rule 8)",
          len(srcs) == 1 and srcs[0].get("origin") == REF and srcs[0].get("id") == r4 and (srcs[0].get("cited") or {}).get("source") == "Rae on the reference", srcs)

    def ref_mention(target):
        ms = [m for m in items(ref("GET", "/api/mentions")[1]) if al in m["source"] and m["target_item_id"] == target]
        return ms[0] if ms and ms[0]["status"] != "pending" else None
    got = wait("the reference verifies blygd's source mention (§15.4)",
               lambda: (lambda m: m if m and m["status"] == "verified" and m["relation"] == "source" else None)(ref_mention(r4)),
               ref_tick, evidence=lambda: ref_mention(r4))
    got = wait("the reference verifies blygd's labelled quote",
               lambda: (lambda m: m if m and m["status"] == "verified" and m["relation"] == "transclusion" else None)(ref_mention(r1)),
               ref_tick, evidence=lambda: ref_mention(r1))
    def link_sent():
        os_ = [o for o in blygd("GET", "/outbound")[1]["outbound"]
               if o["source_item_id"] == al and o["target_url"] == f"{REF}f/{r3}/" and o["status"] != "pending"]
        return os_[0] if os_ else None
    o = wait("blygd sends the reference a link mention (§15.2)",
             lambda: (lambda o: o if o and o["relation"] == "link" else None)(link_sent()))
    # The reference keeps no row for a claim that fails, so the only thing
    # to see is a verified one turning up.
    m, end = None, time.time() + 15
    while o and o["status"] == "sent" and not m and time.time() < end:
        ref_tick()
        m = ref_mention(r3)
        time.sleep(1)
    gap("the reference verifies a link mention as a notice (§15.4, #79)", bool(m) and m["status"] == "verified",
        f"blygd's send: {o and (o['status'], o.get('last_result'))}; the reference kept {m and (m['status'], m.get('error'))}")

    rl = ref_publish(f"Rae links [[{a1} | Alice on planning]].\n\n![[{a1} | Alice's claim]]\n\nAnd answers.", "thread")
    _, rdoc = get(f"{REF}items/{rl}.json")
    check("the reference renders a link label as the anchor text", ">Alice on planning</a>" in rdoc["content_html"], rdoc["content_html"])
    gap("the reference discloses its link in links[] (§10.3a)", rdoc.get("links") == [{"origin": ALICE, "id": a1}],
        f"links: {rdoc.get('links')}")
    gap("the reference bakes its directive label (§10.2, #81)", "data-blyg-label=" in rdoc["content_html"],
        "no data-blyg-label in its content_html")
    wait("blygd verifies the reference's labelled quote",
         lambda: any(m["relation"] == "transclusion" and m["status"] == "verified" and rl in m["source_url"]
                     for m in blygd("GET", "/mentions")[1]["mentions"]) or None, ref_tick)

    if gaps:
        print(f"\n{len(gaps)} gap(s), not failures: " + "; ".join(gaps))
    print(f"\n{len(failures)} failed" + (": " + "; ".join(failures) if failures else ""))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
