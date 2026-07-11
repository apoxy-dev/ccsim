// Package stream defines the binary sample record format shared between the
// Go encoder, the native CLI output and the JS reference decoder
// (decoder.mjs).
//
// Record layout (little-endian, 20 bytes fixed):
//
//	offset 0  float64 t_s      virtual time in seconds
//	offset 8  uint16  flow_id  flow index, or a LinkFlowID pseudo-flow
//	offset 10 uint8   kind     see the Kind constants
//	offset 11 uint8   pad      always zero
//	offset 12 float64 value    kind-specific value
//
// Any change here must be mirrored in decoder.mjs.
package stream

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// RecordSize is the fixed encoded size of one record.
const RecordSize = 20

// Kind identifies what a record's value measures.
type Kind uint8

// Sample kinds. Per-flow kinds are periodic unless marked edge-triggered.
const (
	KindCwndPkts       Kind = 0  // congestion window, packets
	KindInflightBytes  Kind = 1  // bytes in flight
	KindPacingRateBps  Kind = 2  // pacing rate, bits/s (0 when CC does not pace)
	KindSRTTSec        Kind = 3  // smoothed RTT, seconds
	KindMinRTTSec      Kind = 4  // CC min-RTT estimate, seconds
	KindDeliveryBps    Kind = 5  // CC delivery rate estimate, bits/s
	KindBytesAckedCum  Kind = 6  // cumulative bytes acked
	KindRetransCum     Kind = 7  // cumulative retransmitted segments
	KindCCState        Kind = 8  // CC state code (BBR phase / cubic recovery state)
	KindQDepthPkts     Kind = 9  // link queue depth, packets (flow = LinkFwd/LinkRev)
	KindQDepthBytes    Kind = 10 // link queue depth, bytes
	KindDrop           Kind = 11 // edge: packet drop, value = DropReason code
	KindCEMark         Kind = 12 // edge: CE mark applied
	KindRTO            Kind = 13 // edge: retransmission timeout fired
	KindLossRecovery   Kind = 14 // edge: loss recovery entered (cwnd cut)
	KindUtilizationBps Kind = 15 // link delivered bits/s over the sample window
	KindFCTSec         Kind = 16 // edge: rr response flow completion time, seconds
	KindPktEnqueue     Kind = 17 // per-packet event stream (optional)
	KindPktDequeue     Kind = 18
	KindPktDrop        Kind = 19
)

// Pseudo flow ids for link-level records.
const (
	LinkFwd uint16 = 0xFFFF
	LinkRev uint16 = 0xFFFE
)

// Record is one decoded sample.
type Record struct {
	T     float64
	Flow  uint16
	Kind  Kind
	Value float64
}

// AppendRecord appends the 20-byte encoding of r to b.
func AppendRecord(b []byte, r Record) []byte {
	var tmp [RecordSize]byte
	binary.LittleEndian.PutUint64(tmp[0:], math.Float64bits(r.T))
	binary.LittleEndian.PutUint16(tmp[8:], r.Flow)
	tmp[10] = byte(r.Kind)
	tmp[11] = 0
	binary.LittleEndian.PutUint64(tmp[12:], math.Float64bits(r.Value))
	return append(b, tmp[:]...)
}

// Decode parses records from buf; len(buf) must be a multiple of RecordSize.
func Decode(buf []byte) ([]Record, error) {
	if len(buf)%RecordSize != 0 {
		return nil, fmt.Errorf("stream: buffer length %d not a multiple of %d", len(buf), RecordSize)
	}
	out := make([]Record, 0, len(buf)/RecordSize)
	for off := 0; off < len(buf); off += RecordSize {
		out = append(out, Record{
			T:     math.Float64frombits(binary.LittleEndian.Uint64(buf[off:])),
			Flow:  binary.LittleEndian.Uint16(buf[off+8:]),
			Kind:  Kind(buf[off+10]),
			Value: math.Float64frombits(binary.LittleEndian.Uint64(buf[off+12:])),
		})
	}
	return out, nil
}

// Writer encodes records into an internal buffer and flushes it to an
// io.Writer (native) or hands it to a TakeBuffer callback (wasm, so the
// buffer can be transferred to JS without copying).
type Writer struct {
	buf   []byte
	limit int
	sink  io.Writer
	take  func([]byte)
	err   error
}

// NewWriter creates a Writer flushing to w whenever the internal buffer
// exceeds limit bytes (default 64 KiB if limit <= 0).
func NewWriter(w io.Writer, limit int) *Writer {
	if limit <= 0 {
		limit = 64 << 10
	}
	return &Writer{sink: w, limit: limit}
}

// NewTakeWriter creates a Writer that hands full buffers to take; the
// callee owns the buffer (Writer allocates a fresh one).
func NewTakeWriter(take func([]byte), limit int) *Writer {
	if limit <= 0 {
		limit = 64 << 10
	}
	return &Writer{take: take, limit: limit}
}

// Write appends one record, flushing if the buffer is full.
func (w *Writer) Write(r Record) {
	w.buf = AppendRecord(w.buf, r)
	if len(w.buf) >= w.limit {
		w.Flush()
	}
}

// Flush emits any buffered records.
func (w *Writer) Flush() {
	if len(w.buf) == 0 {
		return
	}
	if w.take != nil {
		w.take(w.buf)
		w.buf = nil
		return
	}
	if w.sink != nil && w.err == nil {
		_, w.err = w.sink.Write(w.buf)
	}
	w.buf = w.buf[:0]
}

// Err returns the first sink write error, if any.
func (w *Writer) Err() error { return w.err }
