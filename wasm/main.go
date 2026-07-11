//go:build js && wasm

// Command wasm is the thin browser/node entry for ccsim. It exposes a
// `ccsim` global with load/step/run/set/summary functions; the worker glue
// (worker.js) adapts that surface to a postMessage mailbox and transfers
// sample buffers to the main thread.
//
// No syscall/js is used inside simulation slices: the sim runs pure Go and
// JS is touched only at load, at slice boundaries (step/run return) and at
// sample-buffer flush points.
package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"
	"time"

	"ccsim/scenario"
	"ccsim/sim"
	"ccsim/stream"
)

type session struct {
	sim       *sim.Sim
	w         *stream.Writer
	onSamples js.Value // JS callback receiving Uint8Array buffers
}

var cur *session

// flushTake hands one full sample buffer to JS (ownership transfers).
func (s *session) flushTake(buf []byte) {
	if s.onSamples.IsUndefined() || s.onSamples.IsNull() {
		return
	}
	u8 := js.Global().Get("Uint8Array").New(len(buf))
	js.CopyBytesToJS(u8, buf)
	s.onSamples.Invoke(u8)
}

func errResult(err error) js.Value {
	return js.ValueOf(map[string]any{"error": err.Error()})
}

func okResult(kv map[string]any) js.Value {
	if kv == nil {
		kv = map[string]any{}
	}
	kv["ok"] = true
	return js.ValueOf(kv)
}

// load(scenarioJSON string, onSamples func(Uint8Array)) -> {ok} | {error}
func load(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return errResult(fmt.Errorf("load: scenario JSON required"))
	}
	cfg, err := scenario.Parse([]byte(args[0].String()))
	if err != nil {
		return errResult(err)
	}
	s := &session{}
	if len(args) > 1 {
		s.onSamples = args[1]
	} else {
		s.onSamples = js.Undefined()
	}
	s.w = stream.NewTakeWriter(s.flushTake, 64<<10)
	s.sim, err = sim.New(cfg, s.w)
	if err != nil {
		// Fail closed: the previous session must not stay silently live, or
		// subsequent set/step ops would act on a sim the caller believes was
		// replaced.
		cur = nil
		return errResult(err)
	}
	cur = s
	return okResult(nil)
}

// step(dtMs float64) -> {ok, done, t_s} — advances one slice.
func step(this js.Value, args []js.Value) any {
	if cur == nil {
		return errResult(fmt.Errorf("step: no scenario loaded"))
	}
	dt := sim.DefaultSlice
	if len(args) > 0 && args[0].Type() == js.TypeNumber {
		dt = time.Duration(args[0].Float() * float64(time.Millisecond))
	}
	cur.sim.Step(dt)
	return okResult(map[string]any{
		"done": cur.sim.Done(),
		"t_s":  cur.sim.Elapsed().Seconds(),
	})
}

// run() -> {ok, summary} — batch mode: advance flat out to the end, flush
// the stream, return the summary JSON.
func run(this js.Value, args []js.Value) any {
	if cur == nil {
		return errResult(fmt.Errorf("run: no scenario loaded"))
	}
	sum := cur.sim.Run(nil)
	data, err := json.Marshal(sum)
	if err != nil {
		return errResult(err)
	}
	return okResult(map[string]any{"summary": string(data)})
}

// finish() -> {ok, summary} — flush + summary without advancing (used after
// stepping to the end externally, e.g. paced mode).
func finish(this js.Value, args []js.Value) any {
	if cur == nil {
		return errResult(fmt.Errorf("finish: no scenario loaded"))
	}
	sum := cur.sim.Finish()
	data, err := json.Marshal(sum)
	if err != nil {
		return errResult(err)
	}
	return okResult(map[string]any{"summary": string(data)})
}

// flush() -> {ok} — hand any buffered samples to the onSamples callback now.
// Used by the worker's streaming mode to bound chart latency; without it,
// samples only surface at 64KB buffer boundaries.
func flush(this js.Value, args []js.Value) any {
	if cur == nil {
		return errResult(fmt.Errorf("flush: no scenario loaded"))
	}
	cur.w.Flush()
	return okResult(nil)
}

// set(path string, value float64) -> {ok} | {error} — live parameter change.
func set(this js.Value, args []js.Value) any {
	if cur == nil {
		return errResult(fmt.Errorf("set: no scenario loaded"))
	}
	if len(args) != 2 {
		return errResult(fmt.Errorf("set: want (path, value)"))
	}
	if err := cur.sim.Set(args[0].String(), args[1].Float()); err != nil {
		return errResult(err)
	}
	return okResult(nil)
}

// presets() -> {ok, names: []string, scenarios: {name: json}}
func presets(this js.Value, args []js.Value) any {
	out := map[string]any{}
	for name, cfg := range scenario.Presets() {
		data, _ := json.Marshal(cfg)
		out[name] = string(data)
	}
	return okResult(map[string]any{"scenarios": out})
}

func main() {
	api := map[string]any{
		"load":    js.FuncOf(load),
		"step":    js.FuncOf(step),
		"run":     js.FuncOf(run),
		"finish":  js.FuncOf(finish),
		"flush":   js.FuncOf(flush),
		"set":     js.FuncOf(set),
		"presets": js.FuncOf(presets),
	}
	js.Global().Set("ccsim", js.ValueOf(api))
	// Signal readiness for async loaders.
	if cb := js.Global().Get("__ccsimReady"); cb.Type() == js.TypeFunction {
		cb.Invoke()
	}
	select {} // keep the Go runtime alive
}
