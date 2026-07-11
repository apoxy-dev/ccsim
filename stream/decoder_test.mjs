// Node test for the reference decoder: decodes the Go-generated golden file
// and checks the expected records. Run: node stream/decoder_test.mjs
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import assert from "node:assert/strict";
import { decode, Kind, RECORD_SIZE } from "./decoder.mjs";

const dir = dirname(fileURLToPath(import.meta.url));
const buf = readFileSync(join(dir, "testdata", "golden.bin"));

const want = [
  { t: 0.001, flow: 0, kind: Kind.CwndPkts, value: 10 },
  { t: 0.002, flow: 1, kind: Kind.SRTTSec, value: 0.0402 },
  { t: 1.5, flow: 0xffff, kind: Kind.QDepthPkts, value: 123 },
  { t: 2.25, flow: 2, kind: Kind.FCTSec, value: 0.181 },
  { t: 30, flow: 0, kind: Kind.CEMark, value: 1 },
];

assert.equal(buf.length, want.length * RECORD_SIZE);
const got = decode(buf);
assert.deepEqual(got, want);
assert.throws(() => decode(buf.subarray(0, 19)));
console.log("decoder_test.mjs: OK");
