// Types for the repo's reference JS decoder (stream/decoder.mjs), which is
// imported directly so the lab never carries a drifting copy of the record
// format.
declare module '*/stream/decoder.mjs' {
  export const RECORD_SIZE: number
  export const LINK_FWD: number
  export const LINK_REV: number
  export const Kind: Readonly<{
    CwndPkts: number
    InflightBytes: number
    PacingRateBps: number
    SRTTSec: number
    MinRTTSec: number
    DeliveryBps: number
    BytesAckedCum: number
    RetransCum: number
    CCState: number
    QDepthPkts: number
    QDepthBytes: number
    Drop: number
    CEMark: number
    RTO: number
    LossRecovery: number
    UtilizationBps: number
    FCTSec: number
    PktEnqueue: number
    PktDequeue: number
    PktDrop: number
    WireBurstCV: number
    LinkEnqueueBytesCum: number
    LinkDequeueBytesCum: number
    LinkEnqueuePktsCum: number
    LinkArrivalBytesCum: number
  }>
  export function decode(
    buf: ArrayBuffer | Uint8Array,
  ): { t: number; flow: number; kind: number; value: number }[]
}
