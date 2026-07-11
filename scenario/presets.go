package scenario

import (
	"encoding/json"
	"fmt"
	"sort"
)

// bulk returns a bulk-transfer flow.
func bulk(cc string, startAt float64) FlowConfig {
	return FlowConfig{CC: cc, StartAt: startAt, App: AppConfig{Kind: "bulk"}}
}

func rawNum(v float64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf("%g", v))
}

// Presets returns the built-in smoke scenarios, keyed by name.
//
// BDP notes (1500B packets):
//   - 100 Mbps x 20 ms = 250 kB  ~ 167 pkt
//   - 100 Mbps x 40 ms = 500 kB  ~ 333 pkt
//   - 100 Mbps x 30 ms = 375 kB  ~ 250 pkt
//   - 50 Mbps  x 30 ms = 187.5kB ~ 125 pkt
//   - 20 Mbps  x 40 ms = 100 kB  ~  67 pkt
func Presets() map[string]*ScenarioConfig {
	return map[string]*ScenarioConfig{
		"determinism": {
			Seed: 1, Dur: 10,
			Link: LinkConfig{RateMbps: 100, OwdMs: 10,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 167}},
			Flows: []FlowConfig{bulk("cubic", 0), bulk("bbr", 0)},
		},
		"cubic-single": {
			Seed: 2, Dur: 30,
			Link: LinkConfig{RateMbps: 100, OwdMs: 20,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 333}},
			Flows: []FlowConfig{bulk("cubic", 0)},
		},
		"bbr-single": {
			Seed: 3, Dur: 30,
			Link: LinkConfig{RateMbps: 100, OwdMs: 20,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 333}},
			Flows: []FlowConfig{bulk("bbr", 0)},
		},
		"bufferbloat": {
			Seed: 4, Dur: 30,
			Link: LinkConfig{RateMbps: 50, OwdMs: 15,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 6250}}, // 50 x BDP
			Flows: []FlowConfig{bulk("cubic", 0)},
		},
		"random-loss": {
			Seed: 5, Dur: 30,
			Link: LinkConfig{RateMbps: 100, OwdMs: 20, Loss: 0.01,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 3330}}, // deep: 10 x BDP
			Flows: []FlowConfig{bulk("cubic", 0)},
		},
		"fairness": {
			Seed: 6, Dur: 60,
			Link: LinkConfig{RateMbps: 100, OwdMs: 15,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 500}}, // 2 x BDP
			Flows: []FlowConfig{bulk("cubic", 0), bulk("bbr", 0)},
		},
		"rate-step": {
			Seed: 7, Dur: 45,
			Link: LinkConfig{RateMbps: 100, OwdMs: 10,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 334}}, // 2 x BDP
			Flows: []FlowConfig{bulk("bbr", 0)},
			Events: []InjectEvent{
				{At: 15, Path: "link.rate_mbps", Value: rawNum(25)},
				{At: 30, Path: "link.rate_mbps", Value: rawNum(100)},
			},
		},
		"ecn-codel": {
			Seed: 8, Dur: 30,
			Link: LinkConfig{RateMbps: 50, OwdMs: 10,
				Queue: QueueConfig{Kind: "fqcodel", ECN: true}},
			Flows: []FlowConfig{bulk("bbr", 0), bulk("cubic", 0)},
		},
		"rr-fct": {
			Seed: 9, Dur: 30,
			Link: LinkConfig{RateMbps: 20, OwdMs: 20,
				Queue: QueueConfig{Kind: "taildrop", LimitPkts: 1333}}, // 20 x BDP
			Flows: []FlowConfig{
				bulk("cubic", 0),
				{CC: "cubic", StartAt: 0.5, App: AppConfig{
					Kind: "rr", ReqBytes: 100, RespBytes: 16384, PoissonRate: 5}},
			},
		},
	}
}

// Preset returns a named preset or an error listing the valid names.
func Preset(name string) (*ScenarioConfig, error) {
	p := Presets()
	if c, ok := p[name]; ok {
		return c, nil
	}
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("scenario: unknown preset %q (available: %v)", name, names)
}
