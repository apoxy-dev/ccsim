package stream

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden.bin")

var goldenRecords = []Record{
	{T: 0.001, Flow: 0, Kind: KindCwndPkts, Value: 10},
	{T: 0.002, Flow: 1, Kind: KindSRTTSec, Value: 0.0402},
	{T: 1.5, Flow: 0xFFFF, Kind: KindQDepthPkts, Value: 123},
	{T: 2.25, Flow: 2, Kind: KindFCTSec, Value: 0.181},
	{T: 2.5, Flow: 0, Kind: KindRTTSampleSec, Value: 0.052},
	{T: 30, Flow: 0, Kind: KindCEMark, Value: 1},
}

func encodeAll(rs []Record) []byte {
	var b []byte
	for _, r := range rs {
		b = AppendRecord(b, r)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := encodeAll(goldenRecords)
	if len(b) != len(goldenRecords)*RecordSize {
		t.Fatalf("encoded %d bytes", len(b))
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range got {
		if r != goldenRecords[i] {
			t.Fatalf("record %d: got %+v want %+v", i, r, goldenRecords[i])
		}
	}
	if _, err := Decode(b[:19]); err == nil {
		t.Fatal("Decode accepted truncated buffer")
	}
}

// TestGolden keeps testdata/golden.bin (shared with decoder_test.mjs) in
// sync with the encoder.
func TestGolden(t *testing.T) {
	b := encodeAll(goldenRecords)
	path := filepath.Join("testdata", "golden.bin")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./stream -update)", err)
	}
	if !bytes.Equal(got, b) {
		t.Fatal("golden.bin out of date (run go test ./stream -update)")
	}
}

func TestWriterFlush(t *testing.T) {
	var sink bytes.Buffer
	w := NewWriter(&sink, 40) // flush every 2 records
	for _, r := range goldenRecords {
		w.Write(r)
	}
	w.Flush()
	if sink.Len() != len(goldenRecords)*RecordSize {
		t.Fatalf("sink has %d bytes", sink.Len())
	}
	var taken [][]byte
	tw := NewTakeWriter(func(b []byte) { taken = append(taken, b) }, 40)
	for _, r := range goldenRecords {
		tw.Write(r)
	}
	tw.Flush()
	total := 0
	for _, b := range taken {
		total += len(b)
	}
	if total != len(goldenRecords)*RecordSize {
		t.Fatalf("take writer emitted %d bytes", total)
	}
}
