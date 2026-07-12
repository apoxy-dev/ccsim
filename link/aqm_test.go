package link

// AQM unit tests: the CoDel control law and the RED marking curve, driven
// synthetically (no netstack). End-to-end AQM behavior is covered in
// aqm_e2e_test.go.

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

// captureSink records qdisc drop/mark decisions.
type captureSink struct {
	drops []time.Duration
	kinds []DropReason
	marks int
	now   func() time.Duration
}

func (s *captureSink) qdiscDropped(p *Packet, r DropReason) {
	s.drops = append(s.drops, s.now())
	s.kinds = append(s.kinds, r)
}
func (s *captureSink) qdiscMarked(p *Packet) { s.marks++ }

func pkt1500() *Packet { return &Packet{Data: make([]byte, 1500), Flow: 0} }

// Test 16b: during a persistent overload the CoDel control law spaces
// drops at interval/sqrt(n): the first drop intervals must follow
// I/sqrt(1), I/sqrt(2), I/sqrt(3), ... within 20% (drop times are
// quantized to the dequeue cadence, which is far finer than the law).
func TestCoDelControlLawSqrtSpacing(t *testing.T) {
	var now time.Duration
	sink := &captureSink{now: func() time.Duration { return now }}
	p := DefaultCoDelParams()
	q := NewCoDel(100000, 0, p, sink)

	// Persistent overload: 2 packets arrive per dequeue opportunity.
	enq := func() { q.Enqueue(pkt1500(), now) }
	step := 500 * time.Microsecond
	for i := 0; i < 4000; i++ {
		now += step
		enq()
		enq()
		q.Dequeue(now)
	}
	var aqmDrops []time.Duration
	for i, ts := range sink.drops {
		if sink.kinds[i] == DropAQM {
			aqmDrops = append(aqmDrops, ts)
		}
	}
	if len(aqmDrops) < 6 {
		t.Fatalf("only %d AQM drops during persistent overload", len(aqmDrops))
	}
	// First interval is I/sqrt(1) after the entry drop; successive
	// intervals shrink as 1/sqrt(n).
	for n := 1; n <= 4; n++ {
		got := aqmDrops[n] - aqmDrops[n-1]
		want := time.Duration(float64(p.Interval) / math.Sqrt(float64(n)))
		ratio := float64(got) / float64(want)
		t.Logf("drop interval %d: measured %v, control law I/sqrt(%d) = %v (ratio %.3f)", n, got, n, want, ratio)
		if ratio < 0.8 || ratio > 1.2 {
			t.Errorf("drop interval %d = %v, want %v +/- 20%%", n, got, want)
		}
	}
}

// Test 18: the RED drop probability follows the configured linear ramp.
// With the count correction active, inter-drop gaps are uniform on
// [1, 1/pb], so the mean gap must be (1 + 1/pb)/2; a chi-square test
// checks uniformity at one mid-ramp level. Wq=0 pins the average queue
// estimate so each level can be tested in isolation.
func TestREDMarkingCurve(t *testing.T) {
	params := REDParams{MinTh: 100, MaxTh: 300, MaxP: 0.1, Wq: 0}
	pbAt := func(avg float64) float64 {
		switch {
		case avg < params.MinTh:
			return 0
		case avg >= 2*params.MaxTh:
			return 1
		case avg >= params.MaxTh: // gentle region
			return params.MaxP + (avg-params.MaxTh)/params.MaxTh*(1-params.MaxP)
		default:
			return params.MaxP * (avg - params.MinTh) / (params.MaxTh - params.MinTh)
		}
	}

	// gapPMF derives the exact inter-drop gap distribution implied by the
	// linear-ramp pb and the count correction pa = pb/(1 - count*pb)
	// (Floyd's uniformization; count includes the current packet here, and
	// falls back to pb once count*pb >= 1). Testing against this pmf
	// checks the configured ramp itself, with no approximation error.
	gapPMF := func(pb float64) []float64 {
		var pmf []float64
		surv := 1.0
		for k := 1; k <= 4000 && surv > 1e-9; k++ {
			pa := pb
			if float64(k)*pb < 1 {
				pa = pb / (1 - float64(k)*pb)
			}
			if pa > 1 {
				pa = 1
			}
			pmf = append(pmf, surv*pa)
			surv *= 1 - pa
		}
		return pmf
	}
	pmfMean := func(pmf []float64) float64 {
		m, tot := 0.0, 0.0
		for i, p := range pmf {
			m += float64(i+1) * p
			tot += p
		}
		return m / tot
	}

	for _, avg := range []float64{50, 150, 250, 450} {
		pb := pbAt(avg)
		sink := &captureSink{now: func() time.Duration { return 0 }}
		rng := rand.New(rand.NewPCG(0xAE0, uint64(avg)))
		q := NewRED(1_000_000, params, rng.Float64, sink)
		q.avg = avg

		const n = 200_000
		var gaps []int
		last := -1
		for i := 0; i < n; i++ {
			preDrops := len(sink.drops)
			dropped := q.Enqueue(pkt1500(), 0)
			q.avg = avg // re-pin (Wq=0 keeps it, but be explicit)
			if !dropped {
				q.Dequeue(0)
			}
			if len(sink.drops) > preDrops {
				if last >= 0 {
					gaps = append(gaps, i-last)
				}
				last = i
			}
		}

		if pb == 0 {
			if len(sink.drops) != 0 {
				t.Errorf("avg=%v below MinTh: %d drops, want 0", avg, len(sink.drops))
			}
			t.Logf("avg=%v: pb=0, drops=0 OK", avg)
			continue
		}
		if len(gaps) < 100 {
			t.Fatalf("avg=%v: only %d inter-drop gaps", avg, len(gaps))
		}
		var sum float64
		for _, g := range gaps {
			sum += float64(g)
		}
		meanGap := sum / float64(len(gaps))
		wantGap := pmfMean(gapPMF(pb))
		ratio := meanGap / wantGap
		t.Logf("avg=%v: pb=%.4f mean gap %.2f, ramp-pmf prediction %.2f (ratio %.3f, %d drops)",
			avg, pb, meanGap, wantGap, ratio, len(sink.drops))
		if ratio < 0.95 || ratio > 1.05 {
			t.Errorf("avg=%v: mean inter-drop gap %.2f, want %.2f +/- 5%%", avg, meanGap, wantGap)
		}
	}

	// Chi-square uniformity of gaps at avg=250 (pb=0.075, gaps uniform on
	// [1, 13.3]): 10 bins, dof 9, 99th percentile threshold 21.67.
	{
		pb := pbAt(250)
		sink := &captureSink{now: func() time.Duration { return 0 }}
		rng := rand.New(rand.NewPCG(0xAE1, 250))
		q := NewRED(1_000_000, params, rng.Float64, sink)
		q.avg = 250
		var gaps []int
		last := -1
		for i := 0; i < 400_000; i++ {
			preDrops := len(sink.drops)
			if !q.Enqueue(pkt1500(), 0) {
				q.Dequeue(0)
			}
			q.avg = 250
			if len(sink.drops) > preDrops {
				if last >= 0 {
					gaps = append(gaps, i-last)
				}
				last = i
			}
		}
		// Chi-square against the exact ramp-implied pmf, one cell per
		// discrete gap value (tail cells with expected < 5 merged).
		pmf := gapPMF(pb)
		n := len(gaps)
		obs := map[int]int{}
		for _, g := range gaps {
			obs[g]++
		}
		chi2, cells := 0.0, 0
		tailObs, tailExp := 0.0, 0.0
		for k := 1; k <= len(pmf); k++ {
			exp := pmf[k-1] * float64(n)
			o := float64(obs[k])
			if exp < 5 {
				tailObs += o
				tailExp += exp
				continue
			}
			chi2 += (o - exp) * (o - exp) / exp
			cells++
		}
		for g, c := range obs {
			if g > len(pmf) {
				tailObs += float64(c)
			}
		}
		if tailExp >= 5 {
			chi2 += (tailObs - tailExp) * (tailObs - tailExp) / tailExp
			cells++
		}
		// 99th percentile of chi-square at dof = cells-1 (~13): ~27.7.
		const thresh = 27.7
		t.Logf("chi-square vs ramp pmf (avg=250): chi2=%.2f over %d cells (n=%d), 99th pct threshold %.1f",
			chi2, cells, n, thresh)
		if chi2 > thresh {
			t.Errorf("inter-drop gap distribution deviates from the configured ramp: chi2 %.2f > %.1f", chi2, thresh)
		}
	}
}
