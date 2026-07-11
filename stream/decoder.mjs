// Reference decoder for the ccsim binary sample stream.
// Must mirror stream.go exactly: 20-byte little-endian records:
//   [f64 t_s][u16 flow_id][u8 kind][u8 pad][f64 value]

export const RECORD_SIZE = 20;

export const Kind = Object.freeze({
  CwndPkts: 0,
  InflightBytes: 1,
  PacingRateBps: 2,
  SRTTSec: 3,
  MinRTTSec: 4,
  DeliveryBps: 5,
  BytesAckedCum: 6,
  RetransCum: 7,
  CCState: 8,
  QDepthPkts: 9,
  QDepthBytes: 10,
  Drop: 11,
  CEMark: 12,
  RTO: 13,
  LossRecovery: 14,
  UtilizationBps: 15,
  FCTSec: 16,
  PktEnqueue: 17,
  PktDequeue: 18,
  PktDrop: 19,
});

export const LINK_FWD = 0xffff;
export const LINK_REV = 0xfffe;

/**
 * Decode an ArrayBuffer (or Uint8Array) of sample records.
 * @returns {{t: number, flow: number, kind: number, value: number}[]}
 */
export function decode(buf) {
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (bytes.byteLength % RECORD_SIZE !== 0) {
    throw new Error(
      `stream: buffer length ${bytes.byteLength} not a multiple of ${RECORD_SIZE}`,
    );
  }
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const out = new Array(bytes.byteLength / RECORD_SIZE);
  for (let i = 0, off = 0; off < bytes.byteLength; i++, off += RECORD_SIZE) {
    out[i] = {
      t: view.getFloat64(off, true),
      flow: view.getUint16(off + 8, true),
      kind: view.getUint8(off + 10),
      value: view.getFloat64(off + 12, true),
    };
  }
  return out;
}
