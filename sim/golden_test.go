package sim

// Golden stream regression (test 28): the SHA-256 of every preset's sample
// stream is pinned in testdata/golden.json, keyed by the gVisor pin and BBR
// draft revision. Any behavioral change — a CC tweak, a netstack patch, a
// link-model fix — produces a reviewable diff here instead of sliding
// through silently.
//
// Regenerate with:
//
//	go test ./sim -run TestGoldenStreams -update -reason "why"
//
// The reason is mandatory and is appended to testdata/golden_changelog.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"ccsim/scenario"
	"ccsim/stream"
)

var updateReason = flag.String("reason", "", "justification recorded in golden_changelog.md when -update rewrites golden.json")

const (
	goldenPath    = "testdata/golden.json"
	changelogPath = "testdata/golden_changelog.md"
	bbrDraftRev   = "draft-ietf-ccwg-bbr-03"
)

type goldenEntry struct {
	SHA256  string   `json:"sha256"`
	Records int      `json:"records"`
	Seconds []string `json:"seconds"` // per-sim-second segment hashes (first 16 hex chars)
}

type goldenFile struct {
	Gvisor   string                 `json:"gvisor"`
	BBRDraft string                 `json:"bbr_draft"`
	Streams  map[string]goldenEntry `json:"streams"`
}

// gvisorPin extracts the vendored gVisor version from go.mod.
func gvisorPin(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "gvisor.dev/gvisor") {
			f := strings.Fields(line)
			return f[len(f)-1]
		}
	}
	t.Fatal("gvisor.dev/gvisor not found in go.mod")
	return ""
}

func hashStream(raw []byte) goldenEntry {
	full := sha256.Sum256(raw)
	e := goldenEntry{SHA256: hex.EncodeToString(full[:]), Records: len(raw) / stream.RecordSize}
	// Per-second segment hashes localize a divergence in sim time.
	recs, err := stream.Decode(raw)
	if err != nil {
		panic(err)
	}
	segStart := 0
	sec := 0
	flush := func(end int) {
		h := sha256.Sum256(raw[segStart*stream.RecordSize : end*stream.RecordSize])
		e.Seconds = append(e.Seconds, hex.EncodeToString(h[:8]))
		segStart = end
	}
	for i, r := range recs {
		for int(r.T) > sec {
			flush(i)
			sec++
		}
	}
	flush(len(recs))
	return e
}

func TestGoldenStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	presets := scenario.Presets()
	names := make([]string, 0, len(presets))
	for n := range presets {
		names = append(names, n)
	}
	sort.Strings(names)

	got := goldenFile{Gvisor: gvisorPin(t), BBRDraft: bbrDraftRev, Streams: map[string]goldenEntry{}}
	for _, name := range names {
		raw := rawRun(t, name, nil)
		got.Streams[name] = hashStream(raw)
	}

	if *updateDocs {
		if strings.TrimSpace(*updateReason) == "" {
			t.Fatal("-update for golden streams requires -reason (recorded in the changelog)")
		}
		writeGolden(t, got)
		return
	}

	data, err := os.ReadFile(goldenPath)
	if os.IsNotExist(err) {
		t.Fatalf("%s missing — generate it with: go test ./sim -run TestGoldenStreams -update -reason \"initial goldens\"", goldenPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	var want goldenFile
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if want.Gvisor != got.Gvisor || want.BBRDraft != got.BBRDraft {
		t.Errorf("golden key mismatch: goldens for (gvisor %s, %s), running (gvisor %s, %s) — regenerate with -update -reason",
			want.Gvisor, want.BBRDraft, got.Gvisor, got.BBRDraft)
	}
	for _, name := range names {
		w, ok := want.Streams[name]
		g := got.Streams[name]
		if !ok {
			t.Errorf("%s: no golden recorded (new preset?) — add it with -update -reason", name)
			continue
		}
		if w.SHA256 == g.SHA256 {
			continue
		}
		// Localize: first divergent sim-second.
		divSec := -1
		for i := 0; i < len(w.Seconds) && i < len(g.Seconds); i++ {
			if w.Seconds[i] != g.Seconds[i] {
				divSec = i
				break
			}
		}
		if divSec == -1 {
			divSec = min(len(w.Seconds), len(g.Seconds))
		}
		t.Errorf("%s: stream diverged (records %d -> %d): first divergent sim-second t=[%d,%d)s. "+
			"If intentional, regenerate: go test ./sim -run TestGoldenStreams -update -reason \"...\"",
			name, w.Records, g.Records, divSec, divSec+1)
	}
	for name := range want.Streams {
		if _, ok := got.Streams[name]; !ok {
			t.Errorf("%s: golden exists but preset is gone — prune with -update -reason", name)
		}
	}
}

func writeGolden(t *testing.T, g goldenFile) {
	t.Helper()
	if err := os.MkdirAll("testdata", 0755); err != nil {
		t.Fatal(err)
	}
	// Diff against the previous file (if any) for the changelog entry.
	changed := []string{}
	var prev goldenFile
	if data, err := os.ReadFile(goldenPath); err == nil && json.Unmarshal(data, &prev) == nil {
		for name, e := range g.Streams {
			if p, ok := prev.Streams[name]; !ok || p.SHA256 != e.SHA256 {
				changed = append(changed, name)
			}
		}
		for name := range prev.Streams {
			if _, ok := g.Streams[name]; !ok {
				changed = append(changed, name+" (removed)")
			}
		}
	} else {
		changed = append(changed, "all (initial)")
	}
	sort.Strings(changed)

	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goldenPath, append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
	entry := fmt.Sprintf("- %s — gvisor %s, %s — changed: %s — reason: %s\n",
		time.Now().UTC().Format("2006-01-02"), g.Gvisor, g.BBRDraft,
		strings.Join(changed, ", "), strings.TrimSpace(*updateReason))
	f, err := os.OpenFile(changelogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi != nil && fi.Size() == 0 {
		if _, err := f.WriteString("# Golden stream changelog\n\nEvery `-update` of testdata/golden.json appends an entry here.\n\n"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString(entry); err != nil {
		t.Fatal(err)
	}
	t.Logf("golden.json rewritten (%d presets; changed: %s)", len(g.Streams), strings.Join(changed, ", "))
}
