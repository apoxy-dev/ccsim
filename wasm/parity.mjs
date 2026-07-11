// Node parity runner: executes a scenario in the wasm build and writes the
// sample stream + summary so they can be compared byte-for-byte against the
// native CLI. Usage:
//   node wasm/parity.mjs <main.wasm> <wasm_exec.js> <scenario.json> <out.bin>
// Prints the summary JSON on stdout.
import { readFileSync, writeFileSync } from "node:fs";

const [wasmPath, execPath, scenarioPath, outPath] = process.argv.slice(2);
if (!outPath) {
  console.error("usage: node parity.mjs <main.wasm> <wasm_exec.js> <scenario.json> <out.bin>");
  process.exit(2);
}

// wasm_exec.js defines globalThis.Go.
const execSrc = readFileSync(execPath, "utf8");
(0, eval)(execSrc);

const go = new Go();
const chunks = [];

const ready = new Promise((resolve) => {
  globalThis.__ccsimReady = resolve;
});

const { instance } = await WebAssembly.instantiate(
  readFileSync(wasmPath),
  go.importObject,
);
go.run(instance); // resolves __ccsimReady, then blocks in select{}
await ready;

const scenario = readFileSync(scenarioPath, "utf8");
let r = globalThis.ccsim.load(scenario, (u8) => chunks.push(Buffer.from(u8)));
if (!r.ok) throw new Error(r.error);
const t0 = Date.now();
r = globalThis.ccsim.run();
if (!r.ok) throw new Error(r.error);
const wallMs = Date.now() - t0;

writeFileSync(outPath, Buffer.concat(chunks));
console.log(r.summary);
console.error(`wasm run wall time: ${wallMs} ms`);
process.exit(0);
