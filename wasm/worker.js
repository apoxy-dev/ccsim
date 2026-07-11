// Worker glue: adapts the ccsim wasm global API to a postMessage mailbox.
// Sample buffers flow out as Transferables; a `summary` message is posted on
// run completion.
//
// Messages in:  {op:"load", scenario:<json string|object>}
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

self.__ccsimReady = () => {
  postMessage({ type: "ready" });
};

WebAssembly.instantiateStreaming(fetch("main.wasm"), go.importObject).then(
  (result) => go.run(result.instance),
);

function onSamples(u8) {
  // u8 is a fresh Uint8Array owned by us; transfer its buffer.
  postMessage({ type: "samples", buf: u8.buffer }, [u8.buffer]);
}

function fail(r) {
  postMessage({ type: "error", error: r.error });
}

self.onmessage = (e) => {
  const m = e.data;
  switch (m.op) {
    case "load": {
      stopPacing();
      const json =
        typeof m.scenario === "string" ? m.scenario : JSON.stringify(m.scenario);
      const r = ccsim.load(json, onSamples);
      r.ok ? postMessage({ type: "loaded" }) : fail(r);
      break;
    }
    case "run": {
      stopPacing();
      const r = ccsim.run();
      r.ok ? postMessage({ type: "summary", summary: JSON.parse(r.summary) }) : fail(r);
      break;
    }
    case "stream": {
      // Progressive mode: advance flat out, but in batches on a chained
      // zero-delay timeout. Between batches the mailbox drains (so set /
      // pause / load are honored mid-run) and the sample buffer is flushed
      // (so the chart's leading edge lags sim time by at most one batch).
      stopPacing();
      const batchMs = m.batch_ms || 250;
      const loop = () => {
        paceTimer = null;
        const r = ccsim.step(batchMs);
        if (!r.ok) { fail(r); return; }
        ccsim.flush();
        postMessage({ type: "progress", t_s: r.t_s, done: r.done });
        if (r.done) {
          const f = ccsim.finish();
          f.ok ? postMessage({ type: "summary", summary: JSON.parse(f.summary) }) : fail(f);
          return;
        }
        paceTimer = setTimeout(loop, 0);
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
        postMessage({ type: "progress", t_s: r.t_s });
        if (r.done) {
          stopPacing();
          const f = ccsim.finish();
          f.ok ? postMessage({ type: "summary", summary: JSON.parse(f.summary) }) : fail(f);
        }
      }, sliceMs / ratio);
      break;
    }
    case "pause":
      stopPacing();
      break;
    case "step": {
      const r = ccsim.step(m.dt_ms || 10);
      r.ok ? postMessage({ type: "progress", t_s: r.t_s, done: r.done }) : fail(r);
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
  if (paceTimer !== null) {
    // paceTimer may hold an interval (pace) or a timeout (stream); the two
    // clear functions are interchangeable per the HTML spec, but be explicit.
    clearInterval(paceTimer);
    clearTimeout(paceTimer);
    paceTimer = null;
  }
}
