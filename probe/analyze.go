package probe

import "ccsim/stream"

// Series extracts (t, value) points of one kind for one flow within
// [from, to] seconds.
func Series(recs []stream.Record, flow uint16, kind stream.Kind, from, to float64) (ts, vs []float64) {
	for _, r := range recs {
		if r.Flow == flow && r.Kind == kind && r.T >= from && r.T <= to {
			ts = append(ts, r.T)
			vs = append(vs, r.Value)
		}
	}
	return
}

// MeanOf returns the mean of one kind's values in the window.
func MeanOf(recs []stream.Record, flow uint16, kind stream.Kind, from, to float64) float64 {
	_, vs := Series(recs, flow, kind, from, to)
	if len(vs) == 0 {
		return 0
	}
	return mean(vs)
}

// GoodputMbps computes flow goodput over [from, to] from the cumulative
// bytes-acked series.
func GoodputMbps(recs []stream.Record, flow uint16, from, to float64) float64 {
	ts, vs := Series(recs, flow, stream.KindBytesAckedCum, from, to)
	if len(vs) < 2 {
		return 0
	}
	dt := ts[len(ts)-1] - ts[0]
	if dt <= 0 {
		return 0
	}
	return (vs[len(vs)-1] - vs[0]) * 8 / dt / 1e6
}

// Count returns the number of records of a kind for a flow in the window.
func Count(recs []stream.Record, flow uint16, kind stream.Kind, from, to float64) int {
	_, vs := Series(recs, flow, kind, from, to)
	return len(vs)
}
