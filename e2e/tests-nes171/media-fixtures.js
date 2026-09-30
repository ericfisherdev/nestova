// Image fixtures for §11.
//
// Generated rather than committed: the checklist needs bytes that are awkward
// on purpose (truncated, oversized, EXIF-rotated, a renamed non-image), and
// NES-148's content-hash dedup means every fixture must ALSO be byte-distinct
// from every other, which a folder of checked-in samples makes easy to get
// wrong. ImageMagick is the one external tool; hasMagick lets a spec skip
// rather than fail where it is missing.
const { execFileSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { tool, hasTool } = require('../tests/tools');

const DIR = path.join(os.tmpdir(), `nestova-media-fixtures-${process.pid}`);

function hasMagick() {
  if (!hasTool('magick')) return false;
  try {
    execFileSync(tool('magick'), ['-version'], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

function dir() {
  fs.mkdirSync(DIR, { recursive: true });
  return DIR;
}

function magick(args) {
  execFileSync(tool('magick'), args, { stdio: 'pipe' });
}

// image writes a solid-colour image of the given type. The colour is derived
// from the file name so two fixtures never share content and trip the
// duplicate-content guard.
function image(fileName, { width = 64, height = 64, seed = fileName } = {}) {
  const out = path.join(dir(), fileName);
  if (fs.existsSync(out)) return out;
  const hue = Math.abs([...seed].reduce((a, c) => a + c.charCodeAt(0), 0)) % 360;
  magick(['-size', `${width}x${height}`, `xc:hsl(${hue},80%,50%)`, out]);
  return out;
}

// noise writes an image of random pixels, for cases that need real bulk (the
// size cap) or guaranteed-unique content.
function noise(fileName, { width = 256, height = 256 } = {}) {
  const out = path.join(dir(), fileName);
  if (fs.existsSync(out)) return out;
  magick(['-size', `${width}x${height}`, `xc:`, '+noise', 'Random', out]);
  return out;
}

// rotated writes a JPEG whose pixels are landscape but whose EXIF orientation
// says to display it rotated, which is the case T-11.2.8 is about.
function rotated(fileName = 'exif-rotated.jpg') {
  const out = path.join(dir(), fileName);
  if (fs.existsSync(out)) return out;
  const base = noise('rotation-source.png', { width: 200, height: 100 });
  magick([base, '-orient', 'RightTop', out]);
  return out;
}

// truncated writes the first n bytes of a valid JPEG: a real header with the
// image data cut off mid-stream.
function truncated(fileName = 'truncated.jpg', n = 400) {
  const out = path.join(dir(), fileName);
  if (fs.existsSync(out)) return out;
  const whole = fs.readFileSync(noise('truncation-source.jpg', { width: 300, height: 300 }));
  fs.writeFileSync(out, whole.subarray(0, n));
  return out;
}

// raw writes arbitrary bytes under an arbitrary name — a text file called
// .jpg, an SVG, an empty file.
function raw(fileName, contents) {
  const out = path.join(dir(), fileName);
  fs.writeFileSync(out, contents);
  return out;
}

// oversized writes an image comfortably past a byte cap. Noise is used because
// it does not compress, so the file really is the size it claims.
function oversized(fileName, bytes) {
  const out = path.join(dir(), fileName);
  if (fs.existsSync(out) && fs.statSync(out).size > bytes) return out;
  let side = 3000;
  for (let attempt = 0; attempt < 4; attempt++) {
    magick(['-size', `${side}x${side}`, 'xc:', '+noise', 'Random', out]);
    if (fs.statSync(out).size > bytes) return out;
    side = Math.round(side * 1.6);
  }
  return out;
}

function cleanup() {
  fs.rmSync(DIR, { recursive: true, force: true });
}

module.exports = { hasMagick, dir, image, noise, rotated, truncated, raw, oversized, cleanup };
