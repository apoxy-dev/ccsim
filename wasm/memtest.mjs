// wasm memory-growth harness: run N consecutive load+run cycles of one
// scenario in a single wasm instance and watch the linear memory. Go's
// wasm memory never shrinks, so a leak shows up as memory that keeps
// growing cycle over cycle after the allocator has warmed up; a healthy
// build plateaus within the first few cycles.
//
// Usage:
//   node wasm/memtest.mjs <main.wasm> <wasm_exec.js> <scenario.json> [cycles]
//
// Exits 1 if any cycle after the third grows linear memory by more than 5%.
import { readFileSync } from "node:fs";

const [wasmPath, execPath, scenarioPath, cyclesArg] = process.argv.slice(2);
if (!scenarioPath) {
  console.error("usage: node memtest.mjs <main.wasm> <wasm_exec.js> <scenario.json> [cycles]");
  process.exit(2);
}
const cycles = Number(cyclesArg ?? 20);

const execSrc = readFileSync(execPath, "utf8");
(0, eval)(execSrc);

const go = new Go();
const ready = new Promise((resolve) => {
  globalThis.__ccsimReady = resolve;
});
const { instance } = await WebAssembly.instantiate(
  readFileSync(wasmPath),
  go.importObject,
);
go.run(instance);
await ready;

const scenario = readFileSync(scenarioPath, "utf8");
const memBytes = () => instance.exports.mem.buffer.byteLength;

let prev = 0;
let violations = 0;
for (let i = 1; i <= cycles; i++) {
  let sink = 0;
  let r = globalThis.ccsim.load(scenario, (u8) => {
    sink += u8.length; // consume and discard the stream
  });
  if (!r.ok) throw new Error(r.error);
  r = globalThis.ccsim.run();
  if (!r.ok) throw new Error(r.error);
  const mem = memBytes();
  const growth = prev ? (mem - prev) / prev : 0;
  console.log(
    `cycle ${String(i).padStart(2)}: mem ${(mem / 1048576).toFixed(1)} MB` +
      (prev ? ` (${growth >= 0 ? "+" : ""}${(growth * 100).toFixed(2)}% vs prev)` : "") +
      ` stream ${(sink / 1048576).toFixed(1)} MB`,
  );
  if (i > 3 && growth > 0.05) {
    violations++;
    console.error(`cycle ${i}: linear memory grew ${(growth * 100).toFixed(1)}% after warmup`);
  }
  prev = mem;
}

if (violations > 0) {
  console.error(`FAIL: ${violations} post-warmup growth violations`);
  process.exit(1);
}
console.log(`OK: memory stable after warmup over ${cycles} cycles`);
process.exit(0);
