package scenario

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite scenarios/*.json from Presets()")

func TestValidatePresets(t *testing.T) {
	for name, c := range Presets() {
		if err := c.Validate(); err != nil {
			t.Errorf("preset %s invalid: %v", name, err)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*ScenarioConfig)
	}{
		{"zero duration", func(c *ScenarioConfig) { c.Dur = 0 }},
		{"zero rate", func(c *ScenarioConfig) { c.Link.RateMbps = 0 }},
		{"bad loss", func(c *ScenarioConfig) { c.Link.Loss = 1.5 }},
		{"bad queue kind", func(c *ScenarioConfig) { c.Link.Queue.Kind = "fifo9000" }},
		{"no queue limit", func(c *ScenarioConfig) { c.Link.Queue.LimitPkts = 0 }},
		{"low latency ECN without ECN", func(c *ScenarioConfig) { c.Link.Queue.ECNLowLatency = true }},
		{"no flows", func(c *ScenarioConfig) { c.Flows = nil }},
		{"bad cc", func(c *ScenarioConfig) { c.Flows[0].CC = "vegas" }},
		{"bad app", func(c *ScenarioConfig) { c.Flows[0].App.Kind = "torrent" }},
		{"bad event path", func(c *ScenarioConfig) {
			c.Events = []InjectEvent{{At: 1, Path: "flows[0].cc", Value: rawNum(1)}}
		}},
	}
	for _, tc := range cases {
		c, err := Preset("cubic-single")
		if err != nil {
			t.Fatal(err)
		}
		tc.mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted invalid config", tc.name)
		}
	}
}

func TestParseRoundTrip(t *testing.T) {
	orig, _ := Preset("rate-step")
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Link.RateMbps != orig.Link.RateMbps || len(back.Events) != 2 {
		t.Fatalf("round trip mismatch: %+v", back)
	}
}

// TestScenarioFiles keeps scenarios/*.json in sync with Presets().
// Run `go test ./scenario -update` after changing presets.
func TestScenarioFiles(t *testing.T) {
	dir := filepath.Join("..", "scenarios")
	for name, c := range Presets() {
		path := filepath.Join(dir, name+".json")
		want, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, '\n')
		if *update {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s missing (run go test ./scenario -update): %v", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s out of date (run go test ./scenario -update)", path)
		}
	}
}
