// Command ccsim runs one simulation scenario in batch mode, writes the
// binary sample stream and prints a run summary.
//
// Usage:
//
//	ccsim -scenario scenarios/bufferbloat.json -out run.bin -summary
//	ccsim -preset cubic-single -summary -json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"ccsim/probe"
	"ccsim/scenario"
	"ccsim/sim"
	"ccsim/stream"
)

func main() {
	var (
		scenarioPath = flag.String("scenario", "", "path to a scenario JSON file")
		presetName   = flag.String("preset", "", "name of a built-in preset scenario")
		outPath      = flag.String("out", "", "write the binary sample stream to this file")
		summary      = flag.Bool("summary", true, "print the run summary table")
		asJSON       = flag.Bool("json", false, "print the run summary as JSON")
	)
	flag.Parse()

	cfg, err := loadConfig(*scenarioPath, *presetName)
	if err != nil {
		fatal(err)
	}

	var w *stream.Writer
	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			fatal(err)
		}
		defer outFile.Close()
		w = stream.NewWriter(outFile, 0)
	}

	s, err := sim.New(cfg, w)
	if err != nil {
		fatal(err)
	}
	start := time.Now()
	sum := s.Run(nil)
	wall := time.Since(start)
	if w != nil && w.Err() != nil {
		fatal(fmt.Errorf("writing %s: %w", *outPath, w.Err()))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sum); err != nil {
			fatal(err)
		}
	}
	if *summary && !*asJSON {
		printSummary(os.Stdout, cfg, sum)
	}
	fmt.Fprintf(os.Stderr, "simulated %.0fs in %v wall (%.0fx real time)\n",
		sum.DurS, wall.Round(time.Millisecond), sum.DurS/wall.Seconds())
}

func loadConfig(path, preset string) (*scenario.ScenarioConfig, error) {
	switch {
	case path != "" && preset != "":
		return nil, fmt.Errorf("use either -scenario or -preset, not both")
	case path != "":
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return scenario.Parse(data)
	case preset != "":
		return scenario.Preset(preset)
	}
	return nil, fmt.Errorf("one of -scenario or -preset is required")
}

func printSummary(out io.Writer, cfg *scenario.ScenarioConfig, sum probe.RunSummary) {
	tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "flow\tcc\tgoodput\tsrtt mean\tsrtt p95\tsrtt max\tretrans\trtos\tcwnd cuts\tfct p50/p95/p99\n")
	for _, f := range sum.Flows {
		fct := "-"
		if f.FCTCount > 0 {
			fct = fmt.Sprintf("%.0f/%.0f/%.0f ms (n=%d)", f.FCTP50Ms, f.FCTP95Ms, f.FCTP99Ms, f.FCTCount)
		}
		fmt.Fprintf(tw, "%d\t%s\t%.1f Mbps\t%.1f ms\t%.1f ms\t%.1f ms\t%d\t%d\t%d\t%s\n",
			f.ID, f.CC, f.GoodputMbps, f.SRTTMeanMs, f.SRTTP95Ms, f.SRTTMaxMs,
			f.Retransmits, f.RTOs, f.CwndCuts, fct)
	}
	tw.Flush()
	fmt.Fprintf(out, "\nlink: %.0f Mbps, rtt %.0f ms | drops %d | ce marks %d | queue mean %.1f pkts, max %d pkts\n",
		cfg.Link.RateMbps, 2*cfg.Link.OwdMs, sum.Drops, sum.CEMarks, sum.QDepthMeanPkt, sum.QDepthMaxPkt)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ccsim:", err)
	os.Exit(1)
}
