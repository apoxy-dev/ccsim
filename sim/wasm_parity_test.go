package sim

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/stream"
)

// Scenario 10: wasm-parity — the wasm build must produce a byte-identical
// sample stream and a matching summary for the cubic-single preset.
func TestScenarioWasmParity(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found in PATH")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	wasmBin := filepath.Join(tmp, "main.wasm")
	build := exec.Command("go", "build", "-o", wasmBin, "./wasm")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "GOWASM=satconv,signext")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("wasm build failed: %v\n%s", err, out)
	}

	// Native run (in-process).
	cfg, err := scenario.Preset("cubic-single")
	if err != nil {
		t.Fatal(err)
	}
	var nativeBuf bytes.Buffer
	s, err := New(cfg, stream.NewWriter(&nativeBuf, 0))
	if err != nil {
		t.Fatal(err)
	}
	nativeSum := s.Run(nil)

	// Wasm run under node.
	wasmOut := filepath.Join(tmp, "wasm.bin")
	execJS := filepath.Join(root, "wasm", "wasm_exec.js")
	cmd := exec.Command(node,
		filepath.Join(root, "wasm", "parity.mjs"),
		wasmBin, execJS,
		filepath.Join(root, "scenarios", "cubic-single.json"),
		wasmOut)
	sumJSON, err := cmd.Output()
	if err != nil {
		t.Fatalf("node parity run failed: %v", err)
	}

	wasmBytes, err := os.ReadFile(wasmOut)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(nativeBuf.Bytes(), wasmBytes) {
		t.Errorf("sample streams differ: native %d bytes, wasm %d bytes (GOARCH=%s)",
			nativeBuf.Len(), len(wasmBytes), runtime.GOARCH)
	}

	var wasmSum probe.RunSummary
	if err := json.Unmarshal(sumJSON, &wasmSum); err != nil {
		t.Fatalf("parsing wasm summary: %v\n%s", err, sumJSON)
	}
	if len(wasmSum.Flows) != len(nativeSum.Flows) {
		t.Fatalf("flow count: wasm %d native %d", len(wasmSum.Flows), len(nativeSum.Flows))
	}
	tol := func(a, b float64) bool {
		return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	}
	nf, wf := nativeSum.Flows[0], wasmSum.Flows[0]
	if !tol(nf.GoodputMbps, wf.GoodputMbps) || !tol(nf.SRTTMeanMs, wf.SRTTMeanMs) ||
		nf.Retransmits != wf.Retransmits || nf.CwndCuts != wf.CwndCuts {
		t.Errorf("summaries differ:\nnative %+v\nwasm   %+v", nf, wf)
	}
}
