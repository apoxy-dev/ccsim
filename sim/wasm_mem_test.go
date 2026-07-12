//go:build slow

package sim

// Test 30: wasm linear-memory stability across repeated load+run cycles of
// the bufferbloat preset (the long-lived-browser-tab scenario). The node
// harness (wasm/memtest.mjs) fails if memory keeps growing >5% per cycle
// after warmup — leaked timers or stream buffers would do exactly that.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSlowWasmMemoryStability(t *testing.T) {
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

	cmd := exec.Command(node,
		filepath.Join(root, "wasm", "memtest.mjs"),
		wasmBin,
		filepath.Join(root, "wasm", "wasm_exec.js"),
		filepath.Join(root, "scenarios", "bufferbloat.json"),
		"20",
	)
	out, err := cmd.CombinedOutput()
	t.Logf("memtest output:\n%s", out)
	if err != nil {
		t.Fatalf("wasm memory-growth harness failed: %v", err)
	}
}
