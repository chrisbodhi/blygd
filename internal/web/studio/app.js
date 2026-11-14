// blygd studio: a deliberately plain client for phones and laptops. It talks
// only to /api/ on this host. A pasted token that may mint is traded at once
// for the studio's own: only the scopes below, expiring in 30 days, kept in
// this browser's localStorage. The pasted one is never stored. Everything shown is built as DOM nodes with text
// in them, never parsed from a string: imported content is someone else's
// HTML (§14): it is parsed inert and rebuilt node by node from an
// allowlist (richOf), never assigned as markup (studio_test.go enforces it).
"use strict";

const main = document.getElementById("main");
const tabs = document.getElementById("tabs");
const toastEl = document.getElementById("toast");
const updatesEl = document.getElementById("updates");
const TOKEN_KEY = "blygd.token";
// What the studio calls, and nothing more: no tokens:write (a stolen studio
// token cannot mint), no origin or hopper settings.
const STUDIO_SCOPES = ["items:read", "items:write", "items:publish", "items:withdraw", "items:pin",
  "reader:read", "reader:write", "mentions:read", "mentions:write"];
const STUDIO_TTL = 30 * 24 * 3600;

// ── small helpers ─────────────────────────────────────────────────────────

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

function show(...nodes) {
  main.replaceChildren(...nodes.flat().filter((n) => n != null && n !== false));
  window.scrollTo(0, 0);
}

let toastTimer;
function toast(msg) {
  toastEl.textContent = msg;
  toastEl.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (toastEl.hidden = true), 3500);
}

// Plain text from someone's HTML. A DOMParser document is inert: no
// scripts run and nothing loads, and only textContent is read.
function textOf(html) {
  if (!html) return "";
  return new DOMParser().parseFromString(html, "text/html").body.textContent.replace(/\s+/g, " ").trim();
}

// Someone's HTML rebuilt as fresh nodes. The DOMParser document is inert;
// from it only allowlisted elements are recreated, with no attributes but
// an http(s) href, and everything else is kept as text. Bare URLs in text
// become links.
const RICH_TAGS = new Set(["p", "br", "blockquote", "em", "i", "strong", "b", "code", "pre", "ul", "ol", "li",
  "h1", "h2", "h3", "h4", "h5", "h6", "hr", "del", "s", "sup", "sub", "a"]);

function richOf(html) {
  const frag = document.createDocumentFragment();
  if (html) copyRich(new DOMParser().parseFromString(html, "text/html").body, frag, false);
  return frag;
}

function copyRich(from, to, inLink) {
  for (const n of from.childNodes) {
    if (n.nodeType === Node.TEXT_NODE) {
      to.append(inLink ? document.createTextNode(n.data) : linkify(n.data));
      continue;
    }
    if (n.nodeType !== Node.ELEMENT_NODE) continue;
    const tag = n.localName;
    if (tag === "script" || tag === "style" || tag === "template") continue;
    if (!RICH_TAGS.has(tag) || (tag === "a" && inLink)) {
      copyRich(n, to, inLink);
      continue;
    }
    let el;
    if (tag === "a") {
      const href = safeHref(n.getAttribute("href"));
      if (!href) {
        copyRich(n, to, inLink);
        continue;
      }
      el = extLink(href);
    } else {
      el = document.createElement(tag);
    }
    copyRich(n, el, inLink || tag === "a");
    to.append(el);
  }
}

function safeHref(href) {
  try {
    const u = new URL(href || "");
    return u.protocol === "https:" || u.protocol === "http:" ? u.href : null;
  } catch {
    return null;
  }
}

function extLink(href, text) {
  return h("a", { href, target: "_blank", rel: "noopener noreferrer nofollow" }, text);
}

function linkify(text) {
  const frag = document.createDocumentFragment();
  const re = /\bhttps?:\/\/[^\s<>"]+/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    // Trailing punctuation is more often prose than URL.
    const url = m[0].replace(/[.,;:!?)\]'"]+$/, "");
    const href = safeHref(url);
    if (!href) continue;
    frag.append(text.slice(last, m.index), extLink(href, url));
    last = m.index + url.length;
    re.lastIndex = last;
  }
  frag.append(text.slice(last));
  return frag;
}

// Clipped rich text fades out only when there is more below the fold.
const overflowWatch = new ResizeObserver((entries) => {
  for (const { target } of entries) target.classList.toggle("more", target.scrollHeight > target.clientHeight + 1);
});

function clip(s, n) {
  s = s || "";
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

function when(t) {
  if (!t) return "";
  const d = new Date(t);
  const mins = Math.round((Date.now() - d) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins} min ago`;
  if (mins < 1440) return `${Math.round(mins / 60)} h ago`;
  return d.toLocaleDateString();
}

// ── API ───────────────────────────────────────────────────────────────────

class APIError extends Error {
  constructor(status, body) {
    super((body && body.message) || `HTTP ${status}`);
    this.status = status;
    this.body = body || {};
  }
}

async function api(method, path, body) {
  const res = await fetch("/api" + path, {
    method,
    headers: Object.assign(
      { Authorization: "Bearer " + localStorage.getItem(TOKEN_KEY) },
      body === undefined ? {} : { "Content-Type": "application/json" },
    ),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) {
    localStorage.removeItem(TOKEN_KEY);
    route();
    throw new APIError(401, { message: "signed out: the token was refused" });
  }
  const text = await res.text();
  const json = text ? JSON.parse(text) : null;
  if (!res.ok) throw new APIError(res.status, json);
  return json;
}

// Runs an action, showing what went wrong instead of throwing.
async function attempt(fn, okMsg) {
  try {
    const r = await fn();
    if (okMsg) toast(okMsg);
    return r;
  } catch (e) {
    toast(e.message);
    return undefined;
  }
}

let me = null;

// ── routing ───────────────────────────────────────────────────────────────

const views = { write: viewEditor, items: viewItems, item: viewEditor, reader: viewReader, mentions: viewMentions };

async function route() {
  if (!localStorage.getItem(TOKEN_KEY)) {
    tabs.hidden = true;
    return viewSignIn();
  }
  if (!me) {
    try {
      me = await api("GET", "/me");
    } catch (e) {
      if (e.status !== 401) show(h("p", {}, "Could not reach blygd: " + e.message));
      return;
    }
  }
  tabs.hidden = false;
  startStream();
  const [name, arg] = (location.hash.slice(1) || "items").split("/");
  current = name === "item" ? "items" : name;
  updatesEl.hidden = true;
  tabs.querySelector(`a[href="#${current}"]`)?.classList.remove("dot");
  for (const a of tabs.querySelectorAll("a")) a.classList.toggle("on", a.getAttribute("href") === "#" + (name === "item" ? "items" : name));
  show(h("p", { class: "muted" }, "Loading…"));
  await (views[name] || viewItems)(arg);
}

window.addEventListener("hashchange", route);
updatesEl.addEventListener("click", route);

// ── live hints (GET /api/events) ──────────────────────────────────────────
// Server-Sent Events, read with fetch() so the token goes in the header.
// An event only says what changed: the studio re-fetches when the person
// asks (the bar), never under their thumb. Missed events are not replayed,
// so a reconnect just offers a refresh.

let current = "";
let streaming = false;
let retryDelay = 1000;
let wake = null; // resolves the reconnect wait early when the page returns
const importing = {}; // subscription id → {items, more, error}
const subTitles = {}; // subscription id → title, from the last list

async function startStream() {
  if (streaming) return;
  streaming = true;
  while (localStorage.getItem(TOKEN_KEY)) {
    let connected = false;
    try {
      const res = await fetch("/api/events", { headers: { Authorization: "Bearer " + localStorage.getItem(TOKEN_KEY) } });
      if (!res.ok || !res.body) throw new Error("HTTP " + res.status);
      connected = true;
      retryDelay = 1000;
      await readEvents(res.body);
    } catch {
      // network gone, phone asleep, server restarting: wait and retry
    }
    if (connected) offerRefresh(current); // events may have been missed
    await new Promise((resolve) => {
      wake = resolve;
      setTimeout(resolve, retryDelay);
    });
    wake = null;
    retryDelay = Math.min(retryDelay * 2, 30000);
  }
  streaming = false;
}

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible" && wake) {
    retryDelay = 1000;
    wake();
  }
});

async function readEvents(body) {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += decoder.decode(value, { stream: true });
    let end;
    while ((end = buf.indexOf("\n\n")) >= 0) {
      const block = buf.slice(0, end);
      buf = buf.slice(end + 2);
      let type = "", data = "";
      for (const line of block.split("\n")) {
        if (line.startsWith("event: ")) type = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (type) {
        try {
          onEvent(type, JSON.parse(data));
        } catch {
          // a hint we cannot read is a hint we can skip
        }
      }
    }
  }
}

function onEvent(type, d) {
  if (type === "import") {
    if (d.more) importing[d.subscription_id] = d;
    else delete importing[d.subscription_id];
    renderImporting();
    if (!d.more) offerRefresh("reader");
  } else if (type === "subscribe") {
    offerRefresh("reader"); // an imported entry was tried
  } else if (type === "label") {
    offerRefresh("reader"); // the classifier answered: a post may now fold, hide or show
  } else if (type === "mention") {
    offerRefresh("mentions");
  } else if (type === "outbound" && d.status !== "sent") {
    toast(`A mention could not be sent yet: ${d.last_result || d.status}`);
  }
}

// offerRefresh marks a tab as having news, and offers a refresh when it is
// the one being looked at.
function offerRefresh(view) {
  if (!view) return;
  if (view === current) updatesEl.hidden = false;
  else tabs.querySelector(`a[href="#${view}"]`)?.classList.add("dot");
}

function renderImporting() {
  const el = document.getElementById("importing");
  if (!el) return;
  el.replaceChildren(...Object.values(importing).map((d) =>
    h("div", { class: "importing" }, `Importing ${subTitles[d.subscription_id] || "a subscription"}: ${d.items} so far, more in about a minute…`)));
}


// ── sign in ───────────────────────────────────────────────────────────────

function viewSignIn() {
  const input = h("input", { id: "token", name: "token", type: "password", autocomplete: "off", placeholder: "blygd_…" });
  show(
    h("h1", {}, "blygd studio"),
    h("p", { class: "muted" }, "Paste your token. If it can mint tokens, the studio keeps a 30-day token of its own, with only the access it needs, and forgets the one you pasted."),
    h("form", {
      onsubmit: async (ev) => {
        ev.preventDefault();
        try {
          localStorage.setItem(TOKEN_KEY, await studioToken(input.value.trim()));
        } catch (e) {
          toast(e.message);
          return;
        }
        input.value = "";
        me = null;
        location.hash = "#items";
        route();
      },
    }, h("label", { for: "token" }, "Token"), input, h("div", { class: "row" }, h("button", { class: "primary" }, "Sign in"))),
    h("p", { class: "muted small" }, "Writing a tool of your own? See the ", h("a", { href: "/studio/api/" }, "API reference"), "."),
  );
}

// studioToken trades a pasted token for the studio's own, when the pasted
// one may mint; otherwise the pasted one is already as narrow as it gets.
async function studioToken(pasted) {
  const call = async (method, path, body) => {
    const res = await fetch("/api" + path, {
      method,
      headers: Object.assign({ Authorization: "Bearer " + pasted }, body ? { "Content-Type": "application/json" } : {}),
      body: body ? JSON.stringify(body) : undefined,
    });
    const json = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(res.status === 401 ? "That token was refused." : json.message || "HTTP " + res.status);
    return json;
  };
  const who = await call("GET", "/me");
  const all = who.scopes.includes("*");
  if (!all && !who.scopes.includes("tokens:write")) return pasted;
  const device = (/iPhone|iPad|Android|Mac|Windows|Linux/.exec(navigator.userAgent) || ["browser"])[0];
  const minted = await call("POST", "/tokens", {
    name: `studio on ${device}, ${new Date().toISOString().slice(0, 10)}`,
    scopes: STUDIO_SCOPES.filter((sc) => all || who.scopes.includes(sc)),
    expires_in: STUDIO_TTL,
  });
  return minted.token;
}

async function signOut() {
  try {
    await api("DELETE", "/tokens/self");
  } catch (e) {
    if (e.status !== 401) return toast(e.message);
  }
  localStorage.removeItem(TOKEN_KEY);
  me = null;
  route();
}

// ── items ─────────────────────────────────────────────────────────────────

let itemFilter = "";

async function viewItems() {
  const r = await attempt(() => api("GET", "/items" + (itemFilter ? "?state=" + itemFilter : "")));
  if (!r) return;
  const chip = (state, label) =>
    h("button", { class: itemFilter === state ? "on" : "", onclick: () => { itemFilter = state; route(); } }, label);
  show(
    h("h1", {}, "Your items"),
    h("p", { class: "muted small" }, me.origin),
    h("p", { class: "muted small" }, me.expires_at ? `Signed in until ${new Date(me.expires_at).toLocaleDateString()} · ` : "Signed in · ",
      h("a", { href: "#items", onclick: (e) => { e.preventDefault(); signOut(); } }, "Sign out")),
    h("div", { class: "chips" }, chip("", "All"), chip("draft", "Drafts"), chip("published", "Published"), chip("withdrawn", "Withdrawn")),
    h("div", { class: "row" }, h("button", { class: "primary", onclick: () => (location.hash = "#write") }, "New fragment"),
      h("button", { onclick: () => newDraft({ kind: "thread", content_md: "" }) }, "New thread")),
    r.items.length ? r.items.map(itemCard) : h("p", { class: "muted" }, "Nothing here yet."),
  );
}

// A card shows what readers see: the current version as rendered. A draft
// never published, or a withdrawal, has nothing rendered and shows its
// markdown. Tapping opens the editor; a link opens the link.
function itemCard(it) {
  let text;
  if (it.content_html) {
    text = h("div", { class: "rich clipped" }, richOf(it.content_html));
    overflowWatch.observe(text);
  } else {
    text = h("div", { class: "text" }, clip(it.draft_md, 240) || h("span", { class: "muted" }, "(empty)"));
  }
  return h("div", { class: "card tap", onclick: (e) => { if (!e.target.closest("a")) location.hash = "#item/" + it.id; } },
    h("div", {}, h("span", { class: "badge " + it.state }, it.state), h("span", { class: "badge" }, it.kind),
      it.version ? h("span", { class: "badge" }, "v" + it.version) : null,
      it.unpublished_changes ? h("span", { class: "badge draft" }, "edited") : null),
    text,
    h("div", { class: "meta" }, "edited " + when(it.draft_updated_at)));
}

async function newDraft(body) {
  const it = await attempt(() => api("POST", "/items", body));
  if (it) location.hash = "#item/" + it.id;
}

// The editor: a new fragment when there is no id, else an existing item.
async function viewEditor(id) {
  let it = { kind: "fragment", draft_md: "", draft_note: null, state: "draft", version: 0 };
  if (id) {
    it = await attempt(() => api("GET", "/items/" + id));
    if (!it) return;
  }
  let kind = it.kind;
  let rev = it.draft_rev; // the draft this editor saw: saves and publishes are of it alone
  const placeholder = () => (kind === "thread"
    ? "Long-form markdown. Quote with a line of ![[id]]; link with [[id]]." : "Say one thing.");
  const text = h("textarea", { id: "md", name: "md", placeholder: placeholder() });
  text.value = it.draft_md;
  const note = h("input", { id: "note", name: "note", type: "text", placeholder: "What changed (optional)" });
  const title = h("h1", {}, id ? (it.version ? `${it.kind} · v${it.version}${it.withdrawn ? " · withdrawn" : ""}` : `Draft ${it.kind}`) : "New");
  note.value = it.draft_note || "";
  const count = h("span", { class: "muted small" });
  const problems = h("div");
  const previewed = h("div");
  const updateCount = () => (count.textContent = kind === "fragment" ? `${text.value.length} / 2000` : `${text.value.length} characters`);
  text.addEventListener("input", updateCount);
  updateCount();

  const kindSeg = !id ? h("div", { class: "seg" }, ["fragment", "thread"].map((k) =>
    h("button", { type: "button", class: k === kind ? "on" : "", onclick: (ev) => {
      kind = k;
      for (const b of ev.target.parentNode.children) b.classList.toggle("on", b === ev.target);
      text.placeholder = placeholder();
      updateCount();
    } }, k))) : null;

  async function save() {
    if (!id) {
      const created = await api("POST", "/items", { kind, content_md: text.value });
      id = created.id;
      rev = created.draft_rev;
      history.replaceState(null, "", "#item/" + id);
      title.textContent = `Draft ${kind}`;
      if (kindSeg) kindSeg.remove(); // the kind is fixed once the item exists
    }
    rev = (await api("PATCH", "/items/" + id, { content_md: text.value, note: note.value, draft_rev: rev })).draft_rev;
  }

  // preview shows what publishing the text in the editor would put out now,
  // saved or not. A new item is saved first: a preview needs its id.
  async function preview() {
    problems.replaceChildren();
    previewed.replaceChildren();
    try {
      if (!id) await save();
      const p = await api("POST", `/items/${id}/preview`, { content_md: text.value });
      const notes = [
        ...p.problems.map((x) => h("li", {}, h("code", {}, x.at), " ", x.reason)),
        ...p.warnings.map((w) => h("li", {}, w))];
      if (notes.length) {
        problems.replaceChildren(h("div", { class: "problems" },
          h("strong", {}, p.problems.length ? "Would not publish" : "Would publish, with warnings"), h("ul", {}, notes)));
      }
      previewed.replaceChildren(h("div", { class: "card" },
        h("div", { class: "meta" }, "Preview: nothing is saved or sent"),
        h("div", { class: "rich" }, richOf(p.content_html))));
    } catch (e) {
      toast(e.message);
    }
  }

  async function publish() {
    problems.replaceChildren();
    previewed.replaceChildren();
    try {
      await save();
      const r = await api("POST", `/items/${id}/publish`, note.value ? { note: note.value, draft_rev: rev } : { draft_rev: rev });
      toast(`Published v${r.version.version}`);
      if (r.warnings && r.warnings.length) {
        problems.replaceChildren(h("div", { class: "problems" }, h("strong", {}, "Published, with warnings"),
          h("ul", {}, r.warnings.map((w) => h("li", {}, w)))));
      } else {
        location.hash = "#item/" + id;
        route();
      }
    } catch (e) {
      if (e.body.problems) {
        problems.replaceChildren(h("div", { class: "problems" }, h("strong", {}, "Not published"),
          h("ul", {}, e.body.problems.map((p) => h("li", {}, h("code", {}, p.at), " ", p.reason)))));
      } else toast(e.message);
    }
  }

  const latest = it.versions && it.versions.length ? it.versions[it.versions.length - 1] : null;
  const actions = h("div", { class: "row" },
    h("button", { onclick: () => attempt(save, "Saved") }, "Save draft"),
    h("button", { onclick: preview }, "Preview"),
    h("button", { class: "primary", onclick: publish }, it.version ? "Publish changes" : "Publish"));
  const more = id && it.version ? h("div", { class: "row" },
    it.url ? h("a", { href: it.url, target: "_blank", rel: "noopener" }, "Open public page ↗") : null,
    latest && latest.kind !== "withdrawn" && !latest.pinned_at
      ? h("button", { onclick: async () => {
          if (!confirm(`Pin v${latest.version}? A pin is permanent: that version stays public forever.`)) return;
          if (await attempt(() => api("POST", `/items/${id}/versions/${latest.version}/pin`), `Pinned v${latest.version}`)) route();
        } }, `Pin v${latest.version}`) : null,
    !it.withdrawn ? h("button", { class: "danger", onclick: async () => {
      if (!confirm("Withdraw this item? Its content leaves the public surface; pinned versions stay.")) return;
      if (await attempt(() => api("POST", `/items/${id}/withdraw`), "Withdrawn")) route();
    } }, "Withdraw") : null) : null;
  const del = id && !it.version ? h("div", { class: "row" }, h("button", { class: "danger", onclick: async () => {
    if (!confirm("Delete this draft?")) return;
    if (await attempt(() => api("DELETE", "/items/" + id), "Deleted")) location.hash = "#items";
  } }, "Delete draft")) : null;

  show(
    title,
    kindSeg,
    it.stub_of ? h("p", { class: "muted small" }, "A response to ", h("code", {}, it.stub_of.url || it.stub_of.id)) : null,
    h("label", { for: "md" }, "Markdown"), text, h("div", {}, count),
    h("label", { for: "note" }, "Note"), note,
    problems, actions, previewed, more, del,
    id ? h("p", { class: "muted small" }, "id ", h("code", {}, id)) : null,
  );
}

// ── reader ────────────────────────────────────────────────────────────────

let readerSub = 0;
let unreadOnly = true;
// Unread counts on the Reader's subscription chips, kept current as cards
// are marked: sid -> { n, el, label }.
let unreadChips = {};
// Filters (DESIGN.md "Filters"): showFiltered reads the feed with
// filters=off, so hidden and held posts can be seen and rated too.
let showFiltered = false;
let filtersOpen = false;
let filterByID = {}; // filter id → the filter, from the last list

function bumpUnread(sid, delta) {
  const c = unreadChips[sid];
  if (!c) return;
  c.n = Math.max(0, c.n + delta);
  c.el.textContent = c.n ? `${c.label} (${c.n})` : c.label;
}

async function viewReader() {
  const [subs, feed, waiting, failed, fl] = await Promise.all([
    attempt(() => api("GET", "/subscriptions")),
    attempt(() => api("GET", "/reader/feed?limit=50" + (unreadOnly ? "&unread=true" : "") + (readerSub ? "&subscription=" + readerSub : "") +
      (showFiltered ? "&filters=off" : ""))),
    // Only what is still to come or went wrong: a finished import can be a
    // thousand rows.
    attempt(() => api("GET", "/subscriptions/imports?status=pending")),
    attempt(() => api("GET", "/subscriptions/imports?status=failed")),
    attempt(() => api("GET", "/filters")),
  ]);
  if (!subs || !feed) return;
  const filters = fl ? fl.filters : [];
  filterByID = Object.fromEntries(filters.map((f) => [f.id, f]));
  const list = subs.subscriptions || [];
  for (const s of list) subTitles[s.id] = s.display_title || s.title || s.origin;
  unreadChips = {};
  const subChip = (s) => {
    const label = s.display_title || s.title || s.origin;
    const el = h("button", { class: readerSub === s.id ? "on" : "", onclick: () => { readerSub = s.id; route(); } });
    unreadChips[s.id] = { n: s.unread || 0, el, label };
    bumpUnread(s.id, 0);
    return el;
  };
  const url = h("input", { name: "url", type: "url", placeholder: "https://someone's blyg or feed", autocapitalize: "off", "aria-label": "Address to subscribe to" });
  show(
    h("h1", {}, "Reader"),
    h("form", { onsubmit: async (ev) => {
      ev.preventDefault();
      try {
        const r = await api("POST", "/subscriptions", { url: url.value.trim() });
        toast("Subscribed to " + (r.subscription.title || r.subscription.origin));
        route();
      } catch (e) {
        toast(e.body.trail ? `${e.message} (tried ${e.body.trail.length} URLs)` : e.message);
      }
    } }, h("div", { class: "row" }, url, h("button", { class: "primary" }, "Subscribe"))),
    importList(),
    importQueue(waiting ? waiting.imports : [], failed ? failed.imports : []),
    h("div", { id: "importing" }),
    h("div", { class: "chips" },
      h("button", { class: readerSub === 0 ? "on" : "", onclick: () => { readerSub = 0; route(); } }, "All"),
      list.map(subChip),
      h("button", { class: unreadOnly ? "on" : "", onclick: () => { unreadOnly = !unreadOnly; route(); } }, "Unread only"),
      // Marks what this feed could show, as of when it was read: anything
      // that arrived since stays unread.
      filters.length ? h("button", { class: showFiltered ? "on" : "", onclick: () => { showFiltered = !showFiltered; route(); } }, "Show filtered") : null,
      list.some((s) => s.unread && (!readerSub || s.id === readerSub))
        ? h("button", { onclick: async () => {
            const body = readerSub ? { as_of: feed.as_of, subscription_id: readerSub } : { as_of: feed.as_of };
            const r = await attempt(() => api("POST", "/reader/read", body));
            if (r) { toast(`Marked ${r.marked} read`); route(); }
          } }, "Mark all read") : null),
    readerSub ? subscriptionTools(list.find((s) => s.id === readerSub)) : null,
    fl ? filtersPanel(filters, fl.classifier) : null,
    leftOut(feed),
    feed.items && feed.items.length ? feed.items.map(readerCard) : h("p", { class: "muted" }, list.length ? "Nothing unread." : "Subscribe to someone to start reading."),
  );
  renderImporting();
}

// importList takes an OPML file (a blogroll, or another reader's export) or
// the address of one, or of a blyg with a blogroll. The entries resolve in
// the background; importQueue shows what is still to come.
function importList() {
  const queued = (r) => { toast(`Queued ${r.queued} of ${r.entries}`); route(); };
  const file = h("input", { type: "file", accept: ".opml,.xml,text/x-opml,application/xml", "aria-label": "OPML file", onchange: async () => {
    const f = file.files[0];
    if (!f) return;
    const r = await attempt(async () => api("POST", "/subscriptions/imports", { opml: await f.text() }));
    if (r) queued(r);
  } });
  const url = h("input", { name: "list", type: "url", placeholder: "https://… an OPML file, or a blyg with a blogroll", autocapitalize: "off", "aria-label": "Address of a list" });
  return h("details", {}, h("summary", {}, "Import a list"),
    h("p", { class: "muted small" }, "A blogroll or another reader's OPML export. Each entry is subscribed in the background; none joins your blogroll."),
    file,
    h("form", { onsubmit: async (ev) => {
      ev.preventDefault();
      const r = await attempt(() => api("POST", "/subscriptions/imports", { url: url.value.trim() }));
      if (r) queued(r);
    } }, h("div", { class: "row" }, url, h("button", {}, "Import"))));
}

function importQueue(waiting, failed) {
  if (!waiting.length && !failed.length) return null;
  return h("div", { class: "card" },
    h("div", {}, h("strong", {}, "Importing"), " ", [
      waiting.length ? `${waiting.length} waiting` : "",
      failed.length ? `${failed.length} failed` : ""].filter(Boolean).join(" · ")),
    failed.length ? h("ul", { class: "small" }, failed.map((e) => h("li", {}, e.title || e.url, h("span", { class: "muted" }, " — " + e.error)))) : null,
    failed.length ? h("div", { class: "row" }, h("button", { onclick: async () => {
      if (await attempt(() => api("DELETE", "/subscriptions/imports?status=failed"), "Cleared")) route();
    } }, "Clear failed")) : null);
}

// What this page of the feed left out, never silently.
function leftOut(feed) {
  const parts = [feed.hidden ? `${feed.hidden} hidden` : "", feed.held ? `${feed.held} held until checked` : ""].filter(Boolean);
  if (!parts.length) return null;
  return h("p", { class: "muted small" }, `${parts.join(" and ")} by your filters. `,
    h("a", { href: "#reader", onclick: (e) => { e.preventDefault(); showFiltered = true; route(); } }, "Show them"), ".");
}

const pct = (p) => (p == null ? "?" : Math.round(p * 100) + "%");
const matchWords = { mainly: "mainly about it", mentions: "that mention it" };

// The reader's filters: their topics, what each does, how ratings have
// tuned it, and a form for a new one. Ratings happen on the cards below.
function filtersPanel(filters, classifier) {
  const status = !classifier.enabled
    ? h("p", { class: "problems small" }, "Filters need a classifier, which this server's operator configures (BLYGD_SUBQ_KEY). Until then they are kept and do nothing.")
    : classifier.halted_until
      ? h("p", { class: "problems small" }, `The classifier is paused until ${new Date(classifier.halted_until).toLocaleTimeString()}: ${classifier.last_error}`)
      : null;
  const name = h("input", { type: "text", name: "filter-name", placeholder: "A topic: politics, AI, spoilers for Severance…", "aria-label": "Topic" });
  const desc = h("input", { type: "text", name: "filter-desc", placeholder: "What you mean by it (optional)", "aria-label": "What you mean by it" });
  const action = h("select", { name: "filter-action", "aria-label": "What to do" },
    h("option", { value: "fold" }, "Fold posts about it"), h("option", { value: "hide" }, "Hide posts about it"));
  const match = h("select", { name: "filter-match", "aria-label": "Which posts" },
    h("option", { value: "mainly" }, "mainly about it"), h("option", { value: "mentions" }, "that mention it at all"));
  const hold = h("input", { type: "checkbox", id: "filter-hold" });
  const details = h("details", { open: filtersOpen, ontoggle: (e) => (filtersOpen = e.target.open) },
    h("summary", {}, filters.length ? `Filters (${filters.length})` : "Filters"),
    status,
    h("p", { class: "muted small" }, "Name a topic, and posts about it fold or hide. Rate what you read, Yes or No, and each filter tunes itself to your ratings. Filters are yours alone: nobody you read is told."),
    filters.map(filterRow),
    h("form", { onsubmit: async (ev) => {
      ev.preventDefault();
      const body = { name: name.value.trim(), action: action.value, match: match.value, pending: hold.checked ? "hold" : "show" };
      if (desc.value.trim()) body.description = desc.value.trim();
      if (await attempt(() => api("POST", "/filters", body), `Filtering “${body.name}”: posts are checked in the background`)) {
        filtersOpen = true;
        route();
      }
    } },
      h("div", { class: "row" }, name), h("div", { class: "row" }, desc),
      h("div", { class: "row" }, action, match),
      h("div", { class: "row" }, h("label", { for: "filter-hold", class: "inline" }, hold, " Hold new posts until they are checked (for spoilers)")),
      h("div", { class: "row" }, h("button", { class: "primary" }, "Add filter"))));
  return details;
}

function filterRow(f) {
  const r = f.ratings;
  const tuning = f.auto_tune
    ? (f.tuned_at ? `tuned by ${r.rated} rating${r.rated === 1 ? "" : "s"}` : r.rated ? `${r.rated} rated, tunes at 2 each way` : "tunes as you rate")
    : "set by hand";
  const patch = async (body, msg) => { if (await attempt(() => api("PATCH", `/filters/${f.id}`, body), msg)) route(); };
  return h("div", { class: "card" + (f.enabled ? "" : " off") },
    h("div", {}, h("strong", {}, f.name), f.enabled ? null : h("span", { class: "badge" }, "paused")),
    f.description ? h("div", { class: "meta" }, f.description) : null,
    h("div", { class: "meta" }, `${f.action === "hide" ? "Hides" : "Folds"} posts ${matchWords[f.match]}, from ${pct(f.threshold)} sure · ${tuning}`,
      r.rated ? ` · ${r.disagree} of ${r.rated} ratings disagree` : "", f.pending === "hold" ? " · holds new posts" : ""),
    r.suggested_threshold != null ? h("div", { class: "row" }, h("span", { class: "small" }, `Your ratings suggest ${pct(r.suggested_threshold)}.`),
      h("button", { onclick: () => patch({ auto_tune: true }, "Tuning to your ratings") }, "Tune to my ratings")) : null,
    h("div", { class: "row" },
      h("button", { onclick: () => patch({ enabled: !f.enabled }, f.enabled ? "Paused" : "Resumed") }, f.enabled ? "Pause" : "Resume"),
      f.auto_tune ? h("button", { onclick: () => patch({ auto_tune: false }, "Threshold stays where it is") }, "Stop tuning") : null,
      h("button", { class: "danger", onclick: async () => {
        if (!confirm(`Delete the filter “${f.name}”, and your ratings under it?`)) return;
        if (await attempt(() => api("DELETE", `/filters/${f.id}`), "Deleted")) route();
      } }, "Delete")));
}

const foldWords = { fold: "Folded", hide: "Hidden", hold: "Held until checked" };

// Each of the reader's filters that covers a card, as a question they can
// answer in one tap: is this post about it? The model's guess is shown,
// and an answer of theirs wins and tunes the filter. Tapping their own
// answer again takes it back. Nothing moves under the thumb: what the
// answer does to the feed shows on the next refresh.
function ratingRow(it, l) {
  const f = filterByID[l.filter_id];
  const p = f && f.match === "mentions" ? l.p_mentions : l.p_mainly;
  const yes = h("button", { class: "mini" }, "Yes");
  const no = h("button", { class: "mini" }, "No");
  const guess = h("span", { class: "muted small" });
  const paint = () => {
    yes.classList.toggle("on", l.override === true);
    no.classList.toggle("on", l.override === false);
    yes.classList.toggle("guess", l.override == null && l.matched === true);
    no.classList.toggle("guess", l.override == null && l.matched === false);
    guess.textContent = l.matched == null && l.override == null ? " not checked yet" : ` model: ${pct(p)}`;
  };
  const rate = async (answer) => {
    const path = `/reader/items/${it.subscription_id}/${encodeURIComponent(it.item_id)}/filters/${l.filter_id}`;
    const undo = l.override === answer;
    try {
      const nf = undo ? await api("DELETE", path) : await api("PUT", path, { matched: answer });
      l.override = undo ? null : answer;
      paint();
      const before = filterByID[nf.id];
      filterByID[nf.id] = nf;
      if (before && nf.threshold !== before.threshold) toast(`“${nf.name}” now matches from ${pct(nf.threshold)} sure, tuned by ${nf.ratings.rated} ratings`);
      else toast(undo ? "Rating taken back" : l.matched == null ? "Rated (before the model answered, so it tunes nothing)" : `Rated: ${nf.ratings.rated} for “${nf.name}”`);
    } catch (e) {
      toast(e.message);
    }
  };
  yes.addEventListener("click", () => rate(true));
  no.addEventListener("click", () => rate(false));
  paint();
  return h("div", { class: "rating" }, h("span", { class: "small" }, `About “${l.filter}”?`), guess, yes, no);
}

function subscriptionTools(s) {
  if (!s) return null;
  return h("div", { class: "card" },
    h("div", {}, h("strong", {}, s.title || s.origin)),
    h("div", { class: "meta" }, s.origin),
    h("div", { class: "meta" }, `${s.items} items · polled ${when(s.last_polled_at) || "never"}`, s.last_error ? ` · ${s.last_error}` : ""),
    h("div", { class: "row" },
      h("button", { onclick: async () => { if (await attempt(() => api("POST", `/subscriptions/${s.id}/poll`), "Polled")) route(); } }, "Poll now"),
      h("button", { class: "danger", onclick: async () => {
        if (!confirm("Unsubscribe?")) return;
        if (await attempt(() => api("DELETE", "/subscriptions/" + s.id), "Unsubscribed")) { readerSub = 0; route(); }
      } }, "Unsubscribe")));
}

// Any tap on a card marks it read: its text, a link in it, or Open. The
// badge, the chip count and the toggle change at once and are put back if
// the server refuses. A card stays put until the next refresh, even under
// "Unread only", so nothing moves under the reader's thumb.
function readerCard(it) {
  const doc = it.doc || {};
  const body = it.l0 ? (doc.title ? doc.title + " — " : "") + (doc.summary_text || "") : textOf(doc.content_html);
  const author = doc.author && doc.author.name;
  const full = h("div", { class: "rich clipped" }, it.l0 ? h("p", {}, linkify(body)) : richOf(doc.content_html));
  overflowWatch.observe(full);
  const badge = h("span", { class: "badge unread" }, "new");
  const toggle = h("button", { onclick: () => setRead(!it.read) });
  const paint = () => {
    badge.hidden = it.read;
    toggle.textContent = it.read ? "Mark unread" : "Mark read";
  };
  const setRead = async (read) => {
    if (it.read === read) return;
    it.read = read;
    bumpUnread(it.subscription_id, read ? -1 : 1);
    paint();
    const path = `/reader/items/${it.subscription_id}/${encodeURIComponent(it.item_id)}/read`;
    try {
      await api(read ? "PUT" : "DELETE", path); // 204: no body, so not attempt()
    } catch (e) {
      toast(e.message);
      it.read = !read;
      bumpUnread(it.subscription_id, read ? 1 : -1);
      paint();
    }
  };
  paint();
  // A filtered post arrives folded, saying which filters folded it; Show
  // opens it without marking it read.
  const fd = it.filtered;
  const byNames = fd ? fd.filters.map((id) => (filterByID[id] || {}).name).filter(Boolean).join(", ") : "";
  const folded = fd ? h("div", { class: "folded" }, h("span", { class: "small" }, `${foldWords[fd.action] || "Filtered"}: ${byNames || "by a filter"}`),
    h("button", { class: "mini", onclick: (e) => { e.target.parentNode.remove(); textWrap.hidden = false; } }, "Show")) : null;
  const textWrap = h("div", { class: "tap", hidden: !!fd, onclick: (e) => {
    if (!e.target.closest("a")) full.classList.toggle("clipped");
    setRead(true);
  } }, full);
  const labels = (it.labels || []).filter((l) => filterByID[l.filter_id]);
  // With a few filters, ask about each. With more, ask only where it
  // matters: a match, a rating already given, or an answer near the
  // threshold. The rest are one tap away, for what the model missed.
  const worthAsking = (l) => {
    const f = filterByID[l.filter_id];
    const p = f.match === "mentions" ? l.p_mentions : l.p_mainly;
    return labels.length <= 3 || l.matched !== false || l.override != null || (p != null && Math.abs(p - f.threshold) < 0.2);
  };
  const asked = labels.filter(worthAsking), rest = labels.filter((l) => !worthAsking(l));
  const ratings = h("div", { class: "ratings" }, asked.map((l) => ratingRow(it, l)),
    rest.length ? h("button", { class: "mini", onclick: (e) => e.target.replaceWith(...rest.map((l) => ratingRow(it, l))) },
      `Rate ${rest.length} more filter${rest.length === 1 ? "" : "s"}`) : null);
  return h("div", { class: "card" },
    h("div", {}, badge,
      h("span", { class: "badge" }, it.l0 ? "rss" : it.kind), it.discrepancy ? h("span", { class: "badge" }, "changed") : null),
    folded,
    textWrap,
    labels.length ? ratings : null,
    h("div", { class: "meta" }, [author, it.source, when(it.sort_at)].filter(Boolean).join(" · ")),
    h("div", { class: "row" },
      it.page ? h("a", { href: it.page, target: "_blank", rel: "noopener noreferrer", onclick: () => setRead(true) }, "Open ↗") : null,
      !it.l0 && it.state === "current" ? h("button", { onclick: () => newDraft({ kind: "thread", content_md: `![[${it.item_id}]]\n\n` }) }, "Quote") : null,
      !it.l0 && it.state === "current" ? h("button", { onclick: () => newDraft({ stub_of: { id: it.item_id, origin: it.origin } }) }, "Respond") : null,
      it.l0 && it.page ? h("button", { onclick: () => newDraft({ stub_of: { url: it.page, cited: { source: it.source, excerpt: clip(doc.title || body, 200) } } }) }, "Respond") : null,
      toggle));
}

// ── mentions ──────────────────────────────────────────────────────────────

const verbs = { stub: "responded to", transclusion: "quoted", fork: "forked" };

async function viewMentions() {
  const r = await attempt(() => api("GET", "/mentions"));
  if (!r) return;
  show(
    h("h1", {}, "Mentions"),
    h("p", { class: "muted small" }, "Verified responses show on your pages only after you approve them."),
    r.mentions.length ? r.mentions.map(mentionCard) : h("p", { class: "muted" }, "No mentions yet."),
  );
}

function mentionCard(m) {
  const who = (m.source_author && m.source_author.name) || m.source_host;
  const link = m.source_page || m.source_url;
  return h("div", { class: "card" },
    h("div", {}, h("span", { class: "badge" }, m.status), m.approved_at ? h("span", { class: "badge unread" }, "shown") : null),
    h("div", {}, `${who} ${verbs[m.relation] || "mentioned"} `, h("code", {}, m.target_item_id.slice(0, 8))),
    h("div", { class: "meta" }, h("a", { href: link, target: "_blank", rel: "noopener noreferrer nofollow" }, clip(link, 80)), " · ", when(m.received_at)),
    h("div", { class: "row" },
      m.status === "verified" && !m.approved_at ? h("button", { class: "primary", onclick: async () => {
        if (await attempt(() => api("PUT", `/mentions/${m.id}/approval`), "Shown on your page")) route();
      } }, "Show") : null,
      m.approved_at ? h("button", { onclick: async () => {
        if (await attempt(() => api("DELETE", `/mentions/${m.id}/approval`), "Hidden")) route();
      } }, "Hide") : null,
      h("button", { class: "danger", onclick: async () => {
        if (!confirm(`Block ${m.source_host}? Nothing from it will reach you again.`)) return;
        if (await attempt(() => api("POST", "/mention-blocks", { kind: "host", value: m.source_host }), "Blocked")) route();
      } }, "Block")));
}

route();
