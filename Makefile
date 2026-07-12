GO ?= go

.PHONY: build test slow fuzz perf wasm wasm-mem validate docs-update

build:
	$(GO) build ./... && $(GO) vet ./...

# Fast suite: smoke scenarios, conformance, AQM, property smoke, goldens.
test: build
	$(GO) test ./...

# Long-duration sweeps and the 200-iteration fuzz (nightly).
slow:
	$(GO) test -tags slow ./sim -run 'TestSlow|TestScenarioFuzz' -v -timeout 60m

perf:
	CCSIM_PERF=1 $(GO) test ./sim -run TestPerfBudget -v

wasm:
	GOOS=js GOARCH=wasm GOWASM=satconv,signext $(GO) build -o wasm/main.wasm ./wasm

wasm-mem: wasm
	node wasm/memtest.mjs wasm/main.wasm wasm/wasm_exec.js scenarios/bufferbloat.json 20

# Everything the credibility of the results rests on.
validate: test slow perf wasm-mem

# Regenerate the measured tables in docs/validation.md (slow: the tests ARE
# the data generators). Golden streams are deliberately excluded: those
# need an explicit -reason (see docs/validation.md).
docs-update:
	$(GO) test ./sim -run TestLateJoinerConvergence -update -v
	$(GO) test -tags slow ./sim -run 'TestSlowMathisSweep|TestSlowRTTFairnessExponent|TestSlowBBROperatingPointGrid|TestSlowCoexistenceSurface' -update -v -timeout 60m
