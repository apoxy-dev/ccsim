GO ?= go

.PHONY: build test slow fuzz perf wasm wasm-mem validate docs-update lab-assets lab-precomp lab-dev lab-build

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

# CC Lab SPA (lab/): the wasm binary and worker glue are served from
# lab/public/sim/, refreshed from wasm/ on every lab target.
lab-assets: wasm
	mkdir -p lab/public/sim
	rm -f lab/public/sim/main.wasm lab/public/sim/wasm_exec.js lab/public/sim/worker.js
	cp wasm/main.wasm wasm/wasm_exec.js wasm/worker.js lab/public/sim/

# Precomputed default streams: the native binary renders the default
# scenarios once; determinism makes the bytes identical to a live wasm
# run, so the page loads instantly and only runs wasm off-defaults.
lab-precomp:
	$(GO) build -o ccsim ./cmd/ccsim
	node lab/scripts/gen-scenarios.mjs lab/public/sim/pre
	for s in fig1-cubic fig1-bbr fig1-naive fig1-cubic-lite fig1-bbr-lite fig2-cubic fig2-bbr; do \
		./ccsim -scenario lab/public/sim/pre/$$s.json -out lab/public/sim/pre/$$s.bin -summary=false || exit 1; \
	done

lab-dev: lab-assets lab-precomp
	cd lab && npm install && npm run dev

lab-build: lab-assets lab-precomp
	cd lab && npm install && npm run build

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
