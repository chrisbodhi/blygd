// The reference client (blygger-studio) under Miniflare, for the live
// interop check. run.sh copies this file into the reference checkout and
// runs it from there, so the checkout's own esbuild, miniflare and
// migrations are used. Private fetches are allowed (both studios are on
// localhost) and outbound network is real. A control port fires a cron:
// GET /?cron=*/15 * * * *  runs the reference's poll and outbound drain.
import { build } from "esbuild";
import { Miniflare } from "miniflare";
import { readD1Migrations } from "@cloudflare/vitest-pool-workers";
import { createServer } from "node:http";

const port = Number(process.env.REF_PORT ?? 18787);
const control = Number(process.env.REF_CONTROL_PORT ?? 18788);
const output = await build({
  entryPoints: ["src/index.ts"], bundle: true, platform: "neutral", conditions: ["workerd"],
  external: ["cloudflare:*", "node:*"], mainFields: ["module", "main"], format: "esm", target: "es2022",
  loader: { ".txt": "text" }, write: false,
});
const mf = new Miniflare({
  modules: [{ type: "ESModule", path: "worker.mjs", contents: output.outputFiles[0].text }],
  compatibilityDate: "2026-07-01", compatibilityFlags: ["nodejs_compat"], host: "127.0.0.1", port,
  bindings: {
    OWNER_PASSWORD: process.env.REF_PASSWORD ?? "interop-password", COOKIE_SECRET: "interop-cookie-secret",
    MOUNT: "", ALLOW_PRIVATE_FETCH: "true", API_READ_LIMIT: "10000", API_WRITE_LIMIT: "10000",
  },
  d1Databases: ["DB"], r2Buckets: ["MEDIA"],
});
const db = await mf.getD1Database("DB");
for (const m of await readD1Migrations("./migrations")) await db.batch(m.queries.map((sql) => db.prepare(sql)));
await mf.ready;
createServer(async (req, res) => {
  const cron = new URL(req.url!, "http://x").searchParams.get("cron") ?? "*/15 * * * *";
  try {
    const worker = await mf.getWorker();
    res.end(JSON.stringify(await worker.scheduled({ cron, scheduledTime: new Date() })));
  } catch (e) {
    res.statusCode = 500;
    res.end(String(e));
  }
}).listen(control, "127.0.0.1");
console.log(`reference on http://127.0.0.1:${port}/, cron control on :${control}`);
