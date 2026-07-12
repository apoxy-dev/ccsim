//go:build slow

package sim

// The nightly fuzz budget: 200 randomized scenarios (the fast suite runs a
// 15-iteration smoke of the same generator and invariants).
func init() { fuzzIterations = 200 }
