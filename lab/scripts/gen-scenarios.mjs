// Emits the scenario JSONs for every precomputed default stream (see
// fig1Precomp/fig2Precomp in src/lib/scenario.ts). Run via `make
// lab-precomp`, which then renders each JSON to a .bin stream with the
// native ccsim binary. Importing scenario.ts directly is the point: the
// generated scenarios cannot drift from what the app would send to wasm.

import { mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { HEAVY_CFG, LITE_CFG, bwStepScenario, scenarioFor } from '../src/lib/scenario.ts'

const outDir = process.argv[2]
if (!outDir) {
  console.error('usage: gen-scenarios.mjs <out-dir>')
  process.exit(1)
}
mkdirSync(outDir, { recursive: true })

const scenarios = {
  'fig1-cubic': scenarioFor('cubic', HEAVY_CFG),
  'fig1-bbr': scenarioFor('bbr', HEAVY_CFG),
  'fig1-cubic-lite': scenarioFor('cubic', LITE_CFG),
  'fig1-bbr-lite': scenarioFor('bbr', LITE_CFG),
  'fig2-cubic': bwStepScenario('cubic', 0),
  'fig2-bbr': bwStepScenario('bbr', 0),
}

for (const [name, scn] of Object.entries(scenarios)) {
  writeFileSync(join(outDir, name + '.json'), JSON.stringify(scn, null, 2) + '\n')
  console.log(name + '.json')
}
