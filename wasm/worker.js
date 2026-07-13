// Worker glue: adapts the ccsim wasm global API to a postMessage mailbox.
// Sample buffers flow out as Transferables; a `summary` message is posted on
// run completion.
//
// A load may carry a `gen` (run generation id); it is echoed on every
// subsequent outbound message so the page can drop stale messages that were
// in flight when a new load was posted.
//
// Messages in:  {op:"load", scenario:<json string|object>, gen:<number>}
//               {op:"run"}                    batch: flat out to the end
//               {op:"stream", batch_ms:250}   flat out, yielding between
//                                             batches so samples flush
//                                             promptly and set/pause land
//                                             mid-run
//               {op:"pace", ratio:1.0}        paced: hold sim/real ratio
//               {op:"pause"}
//               {op:"step", dt_ms:10}
//               {op:"set", path:"link.rate_mbps", value:25}
//               {op:"presets"}
// Messages out: {type:"ready"} {type:"loaded"} {type:"samples", buf}
//               {type:"summary", summary} {type:"error", error}
//               {type:"progress", t_s} {type:"presets", scenarios}

importScripts("wasm_exec.js");

const go = new Go();
let paceTimer = null;
let streamSeq = 0; // bumped by stopPacing() to cancel an in-flight stream loop
let gen = 0; // current run generation, echoed on outbound messages

// Unclamped yield between stream batches. setTimeout(0) is subject to timer
// clamping — iOS Safari under low-power throttling stretches it to hundreds
// of milliseconds, turning the flat-out stream loop into a real-time
// slideshow. A MessageChannel ping is an ordinary task with no clamp, and
// still lets the mailbox drain so set/pause/load land mid-run.
const yieldChan = new MessageChannel();
let yieldCb = null;
yieldChan.port1.onmessage = () => {
  const cb = yieldCb;
  yieldCb = null;
  if (cb) cb();
};
function yieldNext(cb) {
  yieldCb = cb;
  yieldChan.port2.postMessage(0);
}

self.__ccsimReady = () => {
  postMessage({ type: "ready" });
};

WebAssembly.instantiateStreaming(fetch("main.wasm"), go.importObject)
  .then((result) => go.run(result.instance))
  .catch((err) => {
    // Without this the page waits for a "ready" that never comes (e.g.
    // main.wasm missing or served with the wrong MIME type).
    postMessage({ type: "error", error: `wasm bootstrap failed: ${err}`, gen });
  });

function onSamples(u8) {
  // u8 is a fresh Uint8Array owned by us; transfer its buffer.
  postMessage({ type: "samples", buf: u8.buffer, gen }, [u8.buffer]);
}

function fail(r) {
  postMessage({ type: "error", error: r.error, gen });
}

self.onmessage = (e) => {
  const m = e.data;
  switch (m.op) {
    case "load": {
      stopPacing();
      if (m.gen !== undefined) gen = m.gen;
      const json =
        typeof m.scenario === "string" ? m.scenario : JSON.stringify(m.scenario);
      const r = ccsim.load(json, onSamples);
      r.ok ? postMessage({ type: "loaded", gen }) : fail(r);
      break;
    }
    case "run": {
      stopPacing();
      const r = ccsim.run();
      r.ok ? postMessage({ type: "summary", summary: JSON.parse(r.summary), gen }) : fail(r);
      break;
    }
    case "stream": {
      // Progressive mode: advance flat out, but in batches on a chained
      // zero-delay timeout. Between batches the mailbox drains (so set /
      // pause / load are honored mid-run) and the sample buffer is flushed
      // (so the chart's leading edge lags sim time by at most one batch).
      stopPacing();
      const myGen = ++streamSeq;
      const batchMs = m.batch_ms || 250;
      const loop = () => {
        if (myGen !== streamSeq) return;
        const r = ccsim.step(batchMs);
        if (!r.ok) { fail(r); return; }
        ccsim.flush();
        postMessage({ type: "progress", t_s: r.t_s, done: r.done, gen });
        if (r.done) {
          const f = ccsim.finish();
          f.ok ? postMessage({ type: "summary", summary: JSON.parse(f.summary), gen }) : fail(f);
          return;
        }
        yieldNext(loop);
      };
      loop();
      break;
    }
    case "pace": {
      // Paced mode: advance sliceMs of sim time every sliceMs/ratio of wall
      // time (same loop as batch, just timer-driven between slices).
      stopPacing();
      const sliceMs = 10;
      const ratio = m.ratio || 1.0;
      paceTimer = setInterval(() => {
        const r = ccsim.step(sliceMs);
        if (!r.ok) { stopPacing(); fail(r); return; }
        postMessage({ type: "progress", t_s: r.t_s, gen });
        if (r.done) {
          stopPacing();
          const f = ccsim.finish();
          f.ok ? postMessage({ type: "summary", summary: JSON.parse(f.summary), gen }) : fail(f);
        }
      }, sliceMs / ratio);
      break;
    }
    case "pause":
      stopPacing();
      break;
    case "step": {
      const r = ccsim.step(m.dt_ms || 10);
      r.ok ? postMessage({ type: "progress", t_s: r.t_s, done: r.done, gen }) : fail(r);
      break;
    }
    case "set": {
      const r = ccsim.set(m.path, m.value);
      if (!r.ok) fail(r);
      break;
    }
    case "presets": {
      const r = ccsim.presets();
      r.ok ? postMessage({ type: "presets", scenarios: r.scenarios }) : fail(r);
      break;
    }
    default:
      postMessage({ type: "error", error: `unknown op ${m.op}` });
  }
};

function stopPacing() {
  streamSeq++; // invalidates any in-flight stream loop
  yieldCb = null;
  if (paceTimer !== null) {
    clearInterval(paceTimer);
    paceTimer = null;
  }
}
