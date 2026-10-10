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
is the number of failed checks (capped at 1).
"""
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

BLYGD = os.environ["BLYGD_URL"].rstrip("/")
ALICE = os.environ["BLYGD_ORIGIN"]
TOKEN = os.environ["BLYGD_TOKEN"]
REF = os.environ["REF_URL"]
CONTROL = os.environ["REF_CONTROL"]
REF_PASSWORD = os.environ.get("REF_PASSWORD", "interop-password")
DEADLINE = 45  # seconds for any one condition

failures = []


def check(name, ok, detail=""):
    print(("  PASS " if ok else "  FAIL ") + name + ("" if ok else f"\n       {detail}"))
    if not ok:
        failures.append(name)
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

    print(f"\n{len(failures)} failed" + (": " + "; ".join(failures) if failures else ""))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
