// Content-unique PNG fixtures for the e2e specs that upload photos.
//
// Shared by photos.spec.js and csp.spec.js so the dedup-avoiding encoder
// lives in one place.
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const zlib = require('zlib');

// CRC32 (IEEE 802.3 polynomial, as PNG requires), table-driven. Implemented
// here rather than via zlib.crc32(), which only exists on Node v20.12+ and
// this repo pins no Node version.
const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

/** Length-prefixed, typed, CRC32'd PNG chunk. */
function pngChunk(type, data) {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const typeBuf = Buffer.from(type, 'ascii');
  const crcBuf = Buffer.alloc(4);
  crcBuf.writeUInt32BE(crc32(Buffer.concat([typeBuf, data])));
  return Buffer.concat([len, typeBuf, data, crcBuf]);
}

/**
 * Encodes a fully valid 1x1 RGBA PNG whose pixel color is randomized per
 * call (NES-148). The server dedups uploads per household by content hash
 * (sha256 of the full stream, NES-123), so two tests uploading
 * byte-identical files collide: the second gets a duplicate response
 * carrying the FIRST test's caption. A randomized pixel makes every call's
 * bytes — and therefore hash — genuinely unique while remaining a real PNG
 * that survives Go's server-side image.Decode. (Appending junk after IEND
 * instead does NOT work: Go rejects the stream with an invalid checksum —
 * already disproven, don't retry it.)
 */
function randomOneByOnePng() {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(1, 0); // width
  ihdr.writeUInt32BE(1, 4); // height
  ihdr[8] = 8; // bit depth
  ihdr[9] = 6; // color type: RGBA
  // bytes 10-12: compression/filter/interlace, all zero.
  const [r, g, b] = crypto.randomBytes(3);
  const scanline = Buffer.from([0x00, r, g, b, 0xff]); // filter byte + RGBA pixel
  // The pixel alone is only 24 bits of entropy — enough calls would
  // eventually repeat one and reintroduce the very collision this helper
  // exists to prevent (the household is never reset, so uploads
  // accumulate across runs). A tEXt chunk carrying 128 random bits makes
  // repeats infeasible. tEXt is an ancillary chunk: Go's image/png skips
  // it, so the image still decodes as a plain 1x1 RGBA PNG.
  const nonce = crypto.randomBytes(16).toString('hex');
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', zlib.deflateSync(scanline)),
    pngChunk('tEXt', Buffer.from(`nonce\0${nonce}`, 'ascii')),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}

/**
 * Writes a freshly encoded, content-unique 1x1 PNG to a unique temp path and
 * returns that path. Both the filename AND the bytes differ per call — the
 * bytes matter because of the per-household content-hash dedup (see
 * randomOneByOnePng). The deliberate duplicate-detection test exercises real
 * dedup by reusing ONE call's result twice; only separate calls must differ.
 */
function writeTempPng() {
  const filePath = path.join(os.tmpdir(), `nestova-e2e-${Date.now()}-${Math.random().toString(36).slice(2)}.png`);
  fs.writeFileSync(filePath, randomOneByOnePng());
  return filePath;
}

module.exports = { writeTempPng };
