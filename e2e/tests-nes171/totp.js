// Minimal TOTP generator for the §2.2 tests.
//
// The app uses pquerna/otp with its defaults (SHA-1, 6 digits, 30-second
// period), so this reproduces RFC 6238 with those parameters. Duplicating the
// algorithm is the price of driving enrolment end to end: the secret is only
// ever revealed once, in the enrolment fragment, and no authenticator app is
// available to a headless test.
const crypto = require('crypto');

function base32Decode(input) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  // Strip whitespace, then trailing '=' padding with a linear scan. A regex
  // like /=+$/ backtracks quadratically on a long run of '=' that is not at the
  // end of the string.
  const compact = input.replace(/\s+/g, '').toUpperCase();
  let end = compact.length;
  while (end > 0 && compact[end - 1] === '=') end--;
  const clean = compact.slice(0, end);
  let bits = 0;
  let value = 0;
  const out = [];
  for (const char of clean) {
    const idx = alphabet.indexOf(char);
    if (idx === -1) throw new Error(`invalid base32 character: ${char}`);
    value = (value << 5) | idx;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

// code returns the 6-digit TOTP for secret at the given time (default now).
// stepOffset shifts by whole 30-second periods, for testing an expired code.
function code(secret, { at = Date.now(), stepOffset = 0 } = {}) {
  const counter = Math.floor(at / 1000 / 30) + stepOffset;
  const buf = Buffer.alloc(8);
  buf.writeBigUInt64BE(BigInt(counter));
  const hmac = crypto.createHmac('sha1', base32Decode(secret)).update(buf).digest();
  const offset = hmac[hmac.length - 1] & 0x0f;
  const binary = ((hmac[offset] & 0x7f) << 24)
    | ((hmac[offset + 1] & 0xff) << 16)
    | ((hmac[offset + 2] & 0xff) << 8)
    | (hmac[offset + 3] & 0xff);
  return String(binary % 1_000_000).padStart(6, '0');
}

module.exports = { code, base32Decode };
