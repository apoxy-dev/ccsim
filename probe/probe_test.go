package probe

import (
	"bytes"
	"testing"
	"time"

	"ccsim/link"
	"ccsim/stream"
)

func TestWireStatsRecordActualLinkCounters(t *testing.T) {
	var buf bytes.Buffer
	w := stream.NewWriter(&buf, 0)
	r := NewRecorder(w, 1, []string{"cubic"})
	r.WireStats = true
	h := r.LinkHooks()

	h.OnArrival(link.Event{T: time.Millisecond, Dir: link.Fwd, Flow: 0, Size: 1500})
	h.OnEnqueue(link.Event{T: time.Millisecond, Dir: link.Fwd, Flow: 0, Size: 1500})
	h.OnArrival(link.Event{T: 2 * time.Millisecond, Dir: link.Fwd, Flow: 0, Size: 500})
	h.OnEnqueue(link.Event{T: 2 * time.Millisecond, Dir: link.Fwd, Flow: 0, Size: 500})
	// A third arrival is rejected by the queue: it contributes to arrival
	// bytes but not accepted enqueue bytes.
	h.OnArrival(link.Event{T: 2500 * time.Microsecond, Dir: link.Fwd, Flow: 0, Size: 1000})
	h.OnDequeue(link.Event{T: 3 * time.Millisecond, Dir: link.Fwd, Flow: 0, Size: 1500})
	h.OnArrival(link.Event{T: 4 * time.Millisecond, Dir: link.Rev, Flow: 0, Size: 60})
	h.OnEnqueue(link.Event{T: 4 * time.Millisecond, Dir: link.Rev, Flow: 0, Size: 60})
	h.OnDequeue(link.Event{T: 5 * time.Millisecond, Dir: link.Rev, Flow: 0, Size: 60})
	r.OnFlowSample(20*time.Millisecond, 0, FlowMetrics{RTTSample: 47 * time.Millisecond})
	r.OnLinkSample(20*time.Millisecond, 1, 500, 20*time.Millisecond)
	w.Flush()

	recs, err := stream.Decode(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := map[[2]uint16]float64{
		{stream.LinkFwd, uint16(stream.KindLinkEnqueueBytesCum)}: 2000,
		{stream.LinkFwd, uint16(stream.KindLinkDequeueBytesCum)}: 1500,
		{stream.LinkFwd, uint16(stream.KindLinkEnqueuePktsCum)}:  2,
		{stream.LinkFwd, uint16(stream.KindLinkArrivalBytesCum)}: 3000,
		{stream.LinkRev, uint16(stream.KindLinkEnqueueBytesCum)}: 60,
		{stream.LinkRev, uint16(stream.KindLinkDequeueBytesCum)}: 60,
		{stream.LinkRev, uint16(stream.KindLinkEnqueuePktsCum)}:  1,
		{stream.LinkRev, uint16(stream.KindLinkArrivalBytesCum)}: 60,
		{0, uint16(stream.KindRTTSampleSec)}:                     0.047,
	}
	for _, rec := range recs {
		key := [2]uint16{rec.Flow, uint16(rec.Kind)}
		if v, ok := want[key]; ok {
			if rec.Value != v {
				t.Errorf("record %v = %v, want %v", key, rec.Value, v)
			}
			delete(want, key)
		}
	}
	for key, v := range want {
		t.Errorf("missing record %v = %v", key, v)
	}
}
