// Runs the reference client's (blygger-studio) pure functions over cases
// read from stdin, for parity_test.go. Run from the reference checkout so
// its modules and tsx resolve:  node --import tsx <this file>
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const dir = process.env.BLYGD_REFERENCE_DIR!;
const mod = (p: string) => import(pathToFileURL(join(dir, p)).href);
const { selectionText } = await mod("src/markdown.ts");
const { selectionFromQuote, locateSelection } = await mod("src/transclusion.ts");
const { flattenFork } = await mod("src/fork-flatten.ts");

type Case = { op: string; html?: string; quote?: string; md?: string; origin?: string; generated?: unknown[] };
const cases: Case[] = JSON.parse(readFileSync(0, "utf8"));
const out: unknown[] = [];
for (const c of cases) {
  switch (c.op) {
    case "selectionText":
      out.push(selectionText(c.html!));
      break;
    case "quote":
      out.push(selectionFromQuote(c.quote!));
      break;
    case "locate":
      out.push(locateSelection(c.html!, selectionFromQuote(c.quote!)));
      break;
    case "flatten": {
      const r = await flattenFork(
        { contentMd: c.md!, contentHtml: c.html!, generated: (c.generated ?? []) as never },
        { origin: c.origin!, link: async (o: string, id: string) => `${o}items/${id}.json` },
      );
      out.push({ md: r.contentMd, exact: r.exact });
      break;
    }
    default:
      out.push({ error: "unknown op " + c.op });
  }
}
process.stdout.write(JSON.stringify(out));
