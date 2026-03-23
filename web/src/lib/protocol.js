/**
 * Conduit binary framing protocol — JavaScript implementation.
 *
 * Wire layout (big-endian throughout):
 *   [ magic      : 4 bytes ] = 0xC0DE1337
 *   [ type       : 1 byte  ]
 *   [ sequence   : 2 bytes ]
 *   [ sessionID  : 4 bytes ]
 *   [ payloadLen : 2 bytes ]
 *   [ payload    : payloadLen bytes ]
 *   [ crc32      : 4 bytes ] ← CRC32-IEEE over all preceding bytes
 *
 * Must stay in sync with internal/protocol/frame.go.
 */

// ---- Constants --------------------------------------------------------------

export const MAGIC = 0xC0DE1337;

export const HEADER_SIZE   = 13; // magic(4) + type(1) + seq(2) + sessionID(4) + payloadLen(2)
export const CHECKSUM_SIZE = 4;
export const MIN_FRAME     = HEADER_SIZE + CHECKSUM_SIZE; // 17 bytes

export const TYPE = Object.freeze({
  KEYSTROKE:      0x01,
  OUTPUT:         0x02,
  SYNC:           0x03,
  ACK:            0x04,
  NACK:           0x05,
  HEARTBEAT:      0x06,
  REPLAY_REQUEST: 0x07,
  REPLAY_FRAME:   0x08,
});

// ---- CRC32-IEEE -------------------------------------------------------------
// Must match Go's crc32.MakeTable(crc32.IEEE) + crc32.Checksum().
// Go's Checksum(data, table) calls Update(0, table, data) which internally
// initialises with ^0 = 0xFFFFFFFF and finalises with another ^crc.

const _crcTable = new Uint32Array(256);
(function buildTable() {
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let j = 0; j < 8; j++) {
      c = (c & 1) ? (0xEDB88320 ^ (c >>> 1)) : (c >>> 1);
    }
    _crcTable[i] = c;
  }
})();

/**
 * Compute CRC32-IEEE over buf[0..length).
 * Returns an unsigned 32-bit integer.
 */
function crc32(buf, length) {
  let crc = 0xFFFFFFFF;
  for (let i = 0; i < length; i++) {
    crc = _crcTable[(crc ^ buf[i]) & 0xFF] ^ (crc >>> 8);
  }
  return (crc ^ 0xFFFFFFFF) >>> 0;
}

// ---- Marshal / Unmarshal ----------------------------------------------------

const _enc = new TextEncoder();
const _dec = new TextDecoder();

/**
 * Serialize a frame to a Uint8Array ready to send over WebSocket.
 *
 * @param {{ type: number, sequence?: number, sessionID?: number, payload?: Uint8Array|string }} frame
 * @returns {Uint8Array}
 */
export function marshal({ type, sequence = 0, sessionID = 0, payload = new Uint8Array(0) }) {
  const pl = typeof payload === 'string' ? _enc.encode(payload) : payload;
  const total = HEADER_SIZE + pl.length + CHECKSUM_SIZE;
  const buf  = new Uint8Array(total);
  const view = new DataView(buf.buffer);

  view.setUint32(0,  MAGIC,     false); // big-endian
  buf[4] = type;
  view.setUint16(5,  sequence,  false);
  view.setUint32(7,  sessionID, false);
  view.setUint16(11, pl.length, false);
  buf.set(pl, HEADER_SIZE);

  const headerAndPayload = HEADER_SIZE + pl.length;
  view.setUint32(headerAndPayload, crc32(buf, headerAndPayload), false);

  return buf;
}

/**
 * Deserialize a frame from an ArrayBuffer (WebSocket binary message).
 *
 * @param {ArrayBuffer} data
 * @returns {{ type: number, sequence: number, sessionID: number, payload: Uint8Array }}
 * @throws {Error} on any validation failure
 */
export function unmarshal(data) {
  const buf  = new Uint8Array(data);
  const view = new DataView(data);

  if (buf.length < MIN_FRAME) {
    throw new Error(`frame too short: ${buf.length} bytes`);
  }

  const magic = view.getUint32(0, false);
  if (magic !== MAGIC) {
    throw new Error(`invalid magic: 0x${magic.toString(16).toUpperCase()}`);
  }

  const type       = buf[4];
  const sequence   = view.getUint16(5,  false);
  const sessionID  = view.getUint32(7,  false);
  const payloadLen = view.getUint16(11, false);
  const needed     = HEADER_SIZE + payloadLen + CHECKSUM_SIZE;

  if (buf.length < needed) {
    throw new Error(`frame truncated: need ${needed}, got ${buf.length}`);
  }

  const headerAndPayload = HEADER_SIZE + payloadLen;
  const storedCRC   = view.getUint32(headerAndPayload, false);
  const computedCRC = crc32(buf, headerAndPayload);

  if (storedCRC !== computedCRC) {
    throw new Error(`checksum mismatch: stored 0x${storedCRC.toString(16)}, computed 0x${computedCRC.toString(16)}`);
  }

  return {
    type,
    sequence,
    sessionID,
    payload: buf.slice(HEADER_SIZE, HEADER_SIZE + payloadLen),
  };
}

/** Decode a frame payload as a UTF-8 string. */
export function payloadText(payload) {
  return _dec.decode(payload);
}
