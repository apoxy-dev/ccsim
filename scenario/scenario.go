// Package scenario defines the simulation configuration model: JSON
// (de)serialization, validation and the built-in preset scenarios.
package scenario

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ScenarioConfig is a complete description of one simulation run.
type ScenarioConfig struct {
	Seed   int64         `json:"seed"`
	Dur    float64       `json:"dur_s"`
	Link   LinkConfig    `json:"link"`
	Flows  []FlowConfig  `json:"flows"`
	Events []InjectEvent `json:"events,omitempty"`
	Sample SampleConfig  `json:"sample,omitempty"`
}

// LinkConfig describes the bottleneck link.
type LinkConfig struct {
	RateMbps float64     `json:"rate_mbps"`
	OwdMs    float64     `json:"owd_ms"`               // one-way delay, each direction
	RevOwdMs float64     `json:"rev_owd_ms,omitempty"` // reverse-direction delay override (0 = symmetric)
	JitterMs float64     `json:"jitter_ms,omitempty"`  // max extra delivery delay, uniform [0, jitter), FIFO-preserving
	Loss     float64     `json:"loss"`                 // bernoulli, per packet
	Queue    QueueConfig `json:"queue"`
}

// QueueConfig selects and parameterizes the queue discipline.
type QueueConfig struct {
	Kind       string  `json:"kind"` // "taildrop" | "red" | "codel" | "fqcodel"
	LimitPkts  int     `json:"limit_pkts,omitempty"`
	LimitBytes int     `json:"limit_bytes,omitempty"`
	ECN        bool    `json:"ecn,omitempty"`
	TargetMs   float64 `json:"target_ms,omitempty"`   // codel/fqcodel
	IntervalMs float64 `json:"interval_ms,omitempty"` // codel/fqcodel
	MinTh      float64 `json:"min_th,omitempty"`      // red, packets
	MaxTh      float64 `json:"max_th,omitempty"`      // red, packets
	MaxP       float64 `json:"max_p,omitempty"`       // red
}

// FlowConfig describes one TCP flow.
type FlowConfig struct {
	CC         string    `json:"cc"` // "cubic" | "bbr" | "reno" | "naive"
	StartAt    float64   `json:"start_at_s"`
	ExtraOwdMs float64   `json:"extra_owd_ms,omitempty"`
	Reverse    bool      `json:"reverse,omitempty"` // data flows receiver->sender (two-way traffic scenarios)
	App        AppConfig `json:"app"`
}

// AppConfig describes the application driving a flow.
type AppConfig struct {
	Kind        string  `json:"kind"`                   // "bulk" | "rr"
	ReqBytes    int     `json:"req_bytes,omitempty"`    // rr
	RespBytes   int     `json:"resp_bytes,omitempty"`   // rr
	PoissonRate float64 `json:"poisson_rate,omitempty"` // rr requests/s
}

// InjectEvent mutates a live-settable parameter at a given time.
type InjectEvent struct {
	At    float64         `json:"at_s"`
	Path  string          `json:"path"` // e.g. "link.rate_mbps"
	Value json.RawMessage `json:"value"`
}

// SampleConfig controls the metric sampling cadence.
type SampleConfig struct {
	IntervalMs   float64 `json:"interval_ms,omitempty"`   // default 1ms
	PacketEvents bool    `json:"packet_events,omitempty"` // per-packet event stream
	WireStats    bool    `json:"wire_stats,omitempty"`    // compact sampled link counters and arrival burstiness
}

// Interval returns the sampling interval with the default applied.
func (s SampleConfig) Interval() time.Duration {
	if s.IntervalMs <= 0 {
		return time.Millisecond
	}
	return time.Duration(s.IntervalMs * float64(time.Millisecond))
}

// Duration returns the run duration.
func (c *ScenarioConfig) Duration() time.Duration {
	return time.Duration(c.Dur * float64(time.Second))
}

// RateBps returns the link rate in bits/s.
func (l *LinkConfig) RateBps() int64 { return int64(l.RateMbps * 1e6) }

// Owd returns the one-way delay.
func (l *LinkConfig) Owd() time.Duration {
	return time.Duration(l.OwdMs * float64(time.Millisecond))
}

// Jitter returns the maximum per-packet extra delivery delay.
func (l *LinkConfig) Jitter() time.Duration {
	return time.Duration(l.JitterMs * float64(time.Millisecond))
}

// Parse decodes and validates a scenario from JSON.
func Parse(data []byte) (*ScenarioConfig, error) {
	var c ScenarioConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("scenario: parsing JSON: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the configuration and returns an actionable error for the
// first problem found.
func (c *ScenarioConfig) Validate() error {
	if c.Dur <= 0 {
		return fmt.Errorf("scenario: dur_s must be > 0, got %v", c.Dur)
	}
	if c.Link.RateMbps <= 0 {
		return fmt.Errorf("scenario: link.rate_mbps must be > 0, got %v", c.Link.RateMbps)
	}
	if c.Link.OwdMs < 0 {
		return fmt.Errorf("scenario: link.owd_ms must be >= 0, got %v", c.Link.OwdMs)
	}
	if c.Link.RevOwdMs < 0 {
		return fmt.Errorf("scenario: link.rev_owd_ms must be >= 0, got %v", c.Link.RevOwdMs)
	}
	if c.Link.JitterMs < 0 {
		return fmt.Errorf("scenario: link.jitter_ms must be >= 0, got %v", c.Link.JitterMs)
	}
	if c.Link.Loss < 0 || c.Link.Loss >= 1 {
		return fmt.Errorf("scenario: link.loss must be in [0,1), got %v", c.Link.Loss)
	}
	switch c.Link.Queue.Kind {
	case "taildrop", "red":
		if c.Link.Queue.LimitPkts <= 0 && c.Link.Queue.LimitBytes <= 0 {
			return fmt.Errorf("scenario: queue kind %q needs limit_pkts or limit_bytes", c.Link.Queue.Kind)
		}
	case "codel", "fqcodel":
		// limits optional (RFC defaults apply)
	case "":
		return fmt.Errorf("scenario: link.queue.kind is required (taildrop|red|codel|fqcodel)")
	default:
		return fmt.Errorf("scenario: unknown queue kind %q (want taildrop|red|codel|fqcodel)", c.Link.Queue.Kind)
	}
	if len(c.Flows) == 0 {
		return fmt.Errorf("scenario: at least one flow is required")
	}
	for i, f := range c.Flows {
		switch f.CC {
		case "cubic", "reno", "bbr", "naive":
		default:
			return fmt.Errorf("scenario: flows[%d].cc %q unknown (want cubic|reno|bbr|naive)", i, f.CC)
		}
		if f.StartAt < 0 || f.StartAt >= c.Dur {
			return fmt.Errorf("scenario: flows[%d].start_at_s %v outside [0,%v)", i, f.StartAt, c.Dur)
		}
		switch f.App.Kind {
		case "bulk":
		case "rr":
			if f.App.RespBytes <= 0 {
				return fmt.Errorf("scenario: flows[%d] rr app needs resp_bytes > 0", i)
			}
			if f.App.PoissonRate <= 0 {
				return fmt.Errorf("scenario: flows[%d] rr app needs poisson_rate > 0", i)
			}
		default:
			return fmt.Errorf("scenario: flows[%d].app.kind %q unknown (want bulk|rr)", i, f.App.Kind)
		}
	}
	for i, e := range c.Events {
		if e.At < 0 || e.At > c.Dur {
			return fmt.Errorf("scenario: events[%d].at_s %v outside [0,%v]", i, e.At, c.Dur)
		}
		switch e.Path {
		case "link.rate_mbps", "link.loss", "link.owd_ms", "link.jitter_ms",
			"link.queue.limit_pkts", "link.queue.limit_bytes", "link.drop_next":
		default:
			return fmt.Errorf("scenario: events[%d].path %q is not live-settable "+
				"(want link.rate_mbps|link.loss|link.owd_ms|link.queue.limit_pkts|link.queue.limit_bytes|link.drop_next)", i, e.Path)
		}
		var v float64
		if err := json.Unmarshal(e.Value, &v); err != nil {
			return fmt.Errorf("scenario: events[%d].value must be a number: %w", i, err)
		}
	}
	return nil
}
