// Isolated-process test driver. Production stdin cannot select these hooks/roots.
import { guestAction } from "../scripts/guest-source.ts";
import { json, parseJson } from "../scripts/client-io.ts";
const { root, payload, crash } = parseJson(await Bun.stdin.text());
try {
  console.log(json(await guestAction(payload, { root, checkpoint: phase => { if (phase === crash) process.exit(91); } })));
} catch (e) { console.error(e instanceof Error ? e.message : String(e)); process.exitCode = 1; }
