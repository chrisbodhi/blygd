// blygd API reference: /api/openapi.json, rendered for people to read. Like
// the studio beside it, it builds DOM nodes with text in them and never
// parses a string as markup (studio_test.go enforces it), and it loads
// nothing from anywhere but this host. It reads the description only: it
// never sends a token.
"use strict";

const main = document.getElementById("main");
const nav = document.getElementById("tags");
const METHODS = ["get", "put", "post", "patch", "delete"];

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// prose renders a description: blank lines part paragraphs, and `code`
// spans become <code>. Nothing else in it is markup.
function prose(text, cls) {
  if (!text) return null;
  return text.split(/\n{2,}/).map((para) =>
    h("p", { class: cls }, para.split("`").map((part, i) => (i % 2 ? h("code", {}, part) : part))));
}

// nodes flattens kids as h does, for the DOM calls that do not.
const nodes = (...kids) => kids.flat(Infinity).filter((k) => k != null && k !== false);

const refName = (ref) => ref.slice(ref.lastIndexOf("/") + 1);
const schemaAnchor = (name) => "schema-" + name;

function resolve(doc, obj) {
  while (obj && obj.$ref) {
    let node = doc;
    for (const part of obj.$ref.replace(/^#\//, "").split("/")) node = node && node[part];
    obj = node;
  }
  return obj || {};
}

// typeOf is a schema's type in one line, with a link for a named schema.
function typeOf(s) {
  if (!s) return "any";
  if (s.$ref) return h("a", { href: "#" + schemaAnchor(refName(s.$ref)) }, refName(s.$ref));
  if (s.oneOf || s.anyOf) {
    const alts = (s.oneOf || s.anyOf).map(typeOf);
    return h("span", {}, "one of ", alts.flatMap((a, i) => (i ? [" | ", a] : [a])));
  }
  const types = Array.isArray(s.type) ? s.type : [s.type || (s.properties ? "object" : "any")];
  return h("span", {}, types.flatMap((t, i) => {
    const sep = i ? [" | "] : [];
    if (t === "array") return [...sep, "array of ", typeOf(s.items)];
    return [...sep, t];
  }));
}

// facts are a schema's constraints, each as a short label.
function facts(s) {
  const out = [];
  if (s.format) out.push(s.format);
  if (s.enum) out.push("one of: " + s.enum.map((v) => JSON.stringify(v)).join(", "));
  if (s.const !== undefined) out.push("always " + JSON.stringify(s.const));
  if (s.pattern) out.push("pattern " + s.pattern);
  if (s.minimum !== undefined) out.push("≥ " + s.minimum);
  if (s.maximum !== undefined) out.push("≤ " + s.maximum);
  if (s.minLength !== undefined) out.push("at least " + s.minLength + " characters");
  if (s.maxLength !== undefined) out.push("at most " + s.maxLength + " characters");
  if (s.minItems !== undefined) out.push("at least " + s.minItems + " items");
  if (s.maxItems !== undefined) out.push("at most " + s.maxItems + " items");
  if (s.default !== undefined) out.push("default " + JSON.stringify(s.default));
  return out;
}

// fields lists an object schema's members; an inline object nests, a named
// one links to its own section, so a recursive schema stays finite.
function fields(s, depth) {
  const props = Object.entries(s.properties || {});
  const required = new Set(s.required || []);
  const rows = props.map(([name, p]) => {
    const inner = p.items && !p.items.$ref && p.items.properties ? p.items : p;
    return h("li", {},
      h("div", { class: "field" },
        h("code", { class: "name" }, name),
        h("span", { class: "type" }, typeOf(p)),
        required.has(name) ? h("span", { class: "req" }, "required") : null,
        facts(p).map((f) => h("span", { class: "fact" }, f))),
      prose(p.description, "desc"),
      !inner.$ref && inner.properties && depth < 4 ? fields(inner, depth + 1) : null);
  });
  if (s.additionalProperties && typeof s.additionalProperties === "object") {
    rows.push(h("li", {}, h("div", { class: "field" },
      h("code", { class: "name" }, "{any name}"), h("span", { class: "type" }, typeOf(s.additionalProperties)))));
  }
  return rows.length ? h("ul", { class: "fields" }, rows) : null;
}

function schemaBlock(doc, s) {
  if (!s) return null;
  const shown = s.$ref ? resolve(doc, s) : s;
  return h("div", { class: "schema" },
    h("div", { class: "field" }, h("span", { class: "type" }, typeOf(s)), facts(shown).map((f) => h("span", { class: "fact" }, f))),
    s.$ref ? null : prose(s.description, "desc"),
    s.$ref ? null : fields(s.items && !s.items.$ref ? s.items : s, 0));
}

function bodyOf(doc, content) {
  return Object.entries(content || {}).map(([type, media]) =>
    h("div", {}, h("div", { class: "small muted" }, type), schemaBlock(doc, media.schema)));
}

function operation(doc, path, method, op, shared) {
  const scopes = (op.security || doc.security || []).flatMap((req) => Object.values(req).flat());
  const open = op.security && op.security.length === 0;
  const params = [...shared, ...(op.parameters || [])].map((p) => resolve(doc, p));
  const body = op.requestBody && resolve(doc, op.requestBody);
  const id = op.operationId || method + path;
  return h("details", { class: "op card", id },
    h("summary", {},
      h("span", { class: "method " + method }, method.toUpperCase()),
      h("code", { class: "path" }, path),
      h("span", { class: "summary" }, op.summary || "")),
    h("div", { class: "facts" },
      h("span", { class: "fact" }, open ? "no token needed" : scopes.length ? "scope " + scopes.join(", ") : "any token"),
      op["x-blygd-cost"] ? h("span", { class: "fact" }, "cost " + op["x-blygd-cost"]) : null,
      h("a", { class: "fact", href: "#" + id }, "link")),
    prose(op.description),
    params.length ? [h("h3", {}, "Parameters"), h("ul", { class: "fields" }, params.map((p) =>
      h("li", {},
        h("div", { class: "field" },
          h("code", { class: "name" }, p.name),
          h("span", { class: "fact" }, p.in),
          h("span", { class: "type" }, typeOf(p.schema)),
          p.required ? h("span", { class: "req" }, "required") : null,
          facts(p.schema || {}).map((f) => h("span", { class: "fact" }, f))),
        prose(p.description || (p.schema && p.schema.description), "desc"))))] : null,
    body ? [h("h3", {}, "Request body", body.required ? "" : " (optional)"), prose(body.description), bodyOf(doc, body.content)] : null,
    h("h3", {}, "Responses"),
    Object.entries(op.responses || {}).sort(([a], [b]) => a.localeCompare(b)).map(([code, r]) => {
      const res = resolve(doc, r);
      return h("div", { class: "response" },
        h("div", { class: "field" }, h("span", { class: "code c" + code[0] }, code), h("span", {}, res.description || "")),
        bodyOf(doc, res.content));
    }));
}

function render(doc) {
  const byTag = new Map((doc.tags || []).map((t) => [t.name, { tag: t, ops: [] }]));
  for (const [path, item] of Object.entries(doc.paths || {})) {
    for (const method of METHODS) {
      const op = item[method];
      if (!op) continue;
      const name = (op.tags && op.tags[0]) || "other";
      if (!byTag.has(name)) byTag.set(name, { tag: { name }, ops: [] });
      byTag.get(name).ops.push(operation(doc, path, method, op, item.parameters || []));
    }
  }
  const schemas = Object.entries((doc.components && doc.components.schemas) || {});
  const auth = Object.values((doc.components && doc.components.securitySchemes) || {});

  nav.replaceChildren(...nodes(
    ...[...byTag.keys()].map((name) => h("a", { href: "#tag-" + name }, name)),
    schemas.length ? h("a", { href: "#schemas" }, "schemas") : null));
  main.replaceChildren(...nodes(
    h("h1", {}, doc.info.title, " ", h("span", { class: "muted small" }, doc.info.version)),
    h("p", { class: "small" }, h("a", { href: "/api/openapi.json" }, "openapi.json"), " · ", h("a", { href: "/studio/" }, "studio")),
    prose(doc.info.description),
    auth.map((a) => [h("h2", {}, "Authentication"), prose(a.description)]),
    [...byTag.values()].map(({ tag, ops }) =>
      h("section", { id: "tag-" + tag.name }, h("h2", {}, tag.name), prose(tag.description), ops)),
    schemas.length ? h("section", { id: "schemas" }, h("h2", {}, "Schemas"),
      schemas.map(([name, s]) => h("div", { class: "card", id: schemaAnchor(name) },
        h("h3", {}, name), schemaBlock(doc, s)))) : null));

  // A link to an operation opens it; a link to a schema lands on it.
  const follow = () => {
    const target = location.hash && document.getElementById(decodeURIComponent(location.hash.slice(1)));
    if (!target) return;
    if (target.tagName === "DETAILS") target.open = true;
    target.scrollIntoView();
  };
  window.addEventListener("hashchange", follow);
  follow();
}

fetch("/api/openapi.json")
  .then((res) => (res.ok ? res.json() : Promise.reject(new Error("HTTP " + res.status))))
  .then(render)
  .catch((err) => main.replaceChildren(h("p", {}, "Could not load the API description: " + err.message)));
