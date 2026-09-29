// Chore seeding and chore-proof photo fixtures for §5.4 and §5.5.
//
// Instances are seeded with SQL because the background scheduler materialises
// them on a five-minute tick (see tests/db.js). Every seeded task gets a unique
// title so a row can be told apart from rows other tests left behind.
//
// Proof photos need a real EXIF DateTimeOriginal tag. ImageMagick cannot write
// one (`-set exif:DateTimeOriginal` is silently dropped on JPEG output, checked
// by reading the file back) and exiftool is not installed, so exifApp1 builds
// the EXIF APP1 segment by hand and spliceExif inserts it after the JPEG SOI.
// The layout is the minimal valid one: a big-endian TIFF header, IFD0 holding
// an optional DateTime and a pointer to the Exif sub-IFD, and the sub-IFD
// holding DateTimeOriginal.
const { execFileSync } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { psql } = require('../tests/db');

const DIR = path.join(os.tmpdir(), `nestova-chore-photos-${process.pid}`);

const TAG_DATE_TIME = 0x0132;
const TAG_EXIF_IFD_POINTER = 0x8769;
const TAG_DATE_TIME_ORIGINAL = 0x9003;
const TYPE_ASCII = 2;
const TYPE_LONG = 4;
const TIFF_HEADER_BYTES = 8;
const IFD_ENTRY_BYTES = 12;

// ---------------------------------------------------------------------------
// Seeding
// ---------------------------------------------------------------------------

function uniqueTitle(prefix) {
  return `${prefix} ${Date.now()}-${crypto.randomBytes(3).toString('hex')}`;
}

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

// seedTask inserts an active daily chore and returns its id. photoPolicy is one
// of none / after_only / before_after (the recurring_task CHECK values).
function seedTask({ title, policy = 'fixed', points = 5, photoPolicy = 'none', householdId = householdA() }) {
  return psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days,
       active, photo_policy, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdId}', '${title}', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            '${policy}', ${points}, 0, true, '${photoPolicy}', now(), now())
    RETURNING id;`).trim();
}

// seedInstance inserts a pending scheduled instance of a fresh task, due
// dueInDays from today (UTC, as the app's /tasks windows are), and returns
// { taskId, instanceId, title }.
function seedInstance({ assignee = null, titlePrefix = 'Chore', policy, points, photoPolicy, householdId, dueInDays = 0 } = {}) {
  const title = uniqueTitle(titlePrefix);
  const hh = householdId || householdA();
  const taskId = seedTask({ title, policy, points, photoPolicy, householdId: hh });
  const who = assignee ? `'${assignee}'` : 'NULL';
  const instanceId = psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${hh}', ${who}, current_date + ${Number(dueInDays)}, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
  return { taskId, instanceId, title };
}

function instanceRow(instanceId) {
  const [status, assignee, completedBy] = psql(
    `SELECT status, coalesce(assignee_id::text, ''), coalesce(completed_by::text, '')
       FROM nestova.task_instance WHERE id = '${instanceId}';`,
  ).trim().split('|');
  return { status, assignee, completedBy };
}

// tasksToken loads /tasks and returns the page's CSRF token.
async function tasksToken(page) {
  await page.goto('/tasks');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// ---------------------------------------------------------------------------
// EXIF fixtures
// ---------------------------------------------------------------------------

function dir() {
  fs.mkdirSync(DIR, { recursive: true });
  return DIR;
}

// exifDate renders a Date as an EXIF "YYYY:MM:DD HH:MM:SS" string in LOCAL
// time. The server reads an offset-less EXIF timestamp in its own local zone
// (ExifReader.dateTimeOriginal uses time.Local), and the test runner shares
// this machine's zone, so local on both ends is what lines them up.
function exifDate(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}:${p(d.getMonth() + 1)}:${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function minutesFromNow(minutes) {
  return new Date(Date.now() + minutes * 60_000);
}

function asciiValue(text) {
  return Buffer.from(`${text}\0`, 'ascii');
}

// writeIfd writes one IFD at offset: entry count, entries, a zero next-IFD
// link. Each entry is { tag, type, count, value } where value is a 32-bit
// number stored inline (a LONG, or an offset to out-of-line data).
function writeIfd(buf, offset, entries) {
  buf.writeUInt16BE(entries.length, offset);
  entries.forEach((e, i) => {
    const at = offset + 2 + i * IFD_ENTRY_BYTES;
    buf.writeUInt16BE(e.tag, at);
    buf.writeUInt16BE(e.type, at + 2);
    buf.writeUInt32BE(e.count, at + 4);
    buf.writeUInt32BE(e.value, at + 8);
  });
  buf.writeUInt32BE(0, offset + 2 + entries.length * IFD_ENTRY_BYTES);
}

function ifdBytes(entryCount) {
  return 2 + entryCount * IFD_ENTRY_BYTES + 4;
}

// exifTiff builds the TIFF body of an EXIF segment carrying DateTimeOriginal
// and/or DateTime. Either may be omitted, which is how the "no usable capture
// timestamp" fixtures are made.
function exifTiff({ dateTimeOriginal, dateTime }) {
  const dateTimeData = dateTime ? asciiValue(dateTime) : null;
  const originalData = dateTimeOriginal ? asciiValue(dateTimeOriginal) : null;

  const ifd0Count = (dateTimeData ? 1 : 0) + (originalData ? 1 : 0);
  const ifd0Offset = TIFF_HEADER_BYTES;
  const dateTimeOffset = ifd0Offset + ifdBytes(ifd0Count);
  const exifIfdOffset = dateTimeOffset + (dateTimeData ? dateTimeData.length : 0);
  const originalOffset = exifIfdOffset + ifdBytes(1);
  const total = originalData ? originalOffset + originalData.length : exifIfdOffset;

  const buf = Buffer.alloc(total);
  buf.write('MM', 0, 'ascii');
  buf.writeUInt16BE(42, 2);
  buf.writeUInt32BE(ifd0Offset, 4);

  // IFD entries must be sorted by tag: DateTime (0x0132) before the Exif
  // sub-IFD pointer (0x8769).
  const ifd0 = [];
  if (dateTimeData) ifd0.push({ tag: TAG_DATE_TIME, type: TYPE_ASCII, count: dateTimeData.length, value: dateTimeOffset });
  if (originalData) ifd0.push({ tag: TAG_EXIF_IFD_POINTER, type: TYPE_LONG, count: 1, value: exifIfdOffset });
  writeIfd(buf, ifd0Offset, ifd0);
  if (dateTimeData) dateTimeData.copy(buf, dateTimeOffset);

  if (originalData) {
    writeIfd(buf, exifIfdOffset, [
      { tag: TAG_DATE_TIME_ORIGINAL, type: TYPE_ASCII, count: originalData.length, value: originalOffset },
    ]);
    originalData.copy(buf, originalOffset);
  }
  return buf;
}

function exifApp1(fields) {
  const tiff = exifTiff(fields);
  const header = Buffer.from('Exif\0\0', 'binary');
  const length = 2 + header.length + tiff.length;
  const marker = Buffer.from([0xff, 0xe1, length >> 8, length & 0xff]);
  return Buffer.concat([marker, header, tiff]);
}

// spliceExif inserts an EXIF APP1 segment straight after the JPEG SOI marker.
function spliceExif(jpeg, fields) {
  return Buffer.concat([jpeg.subarray(0, 2), exifApp1(fields), jpeg.subarray(2)]);
}

// noiseJpeg renders a small random-noise JPEG. Random pixels make every
// fixture byte-distinct, so no two uploads share a content hash.
function noiseJpeg(side = 96) {
  const out = path.join(dir(), `noise-${crypto.randomBytes(6).toString('hex')}.jpg`);
  execFileSync('magick', ['-size', `${side}x${side}`, 'xc:', '+noise', 'Random', out], { stdio: 'pipe' });
  const bytes = fs.readFileSync(out);
  fs.rmSync(out, { force: true });
  return bytes;
}

// proofJpeg returns JPEG bytes whose DateTimeOriginal is takenAt.
function proofJpeg(takenAt) {
  return spliceExif(noiseJpeg(), { dateTimeOriginal: exifDate(takenAt) });
}

// readBackDateTimeOriginal reads the tag back with ImageMagick, independently
// of the hand-built writer, so a malformed segment fails loudly in setup.
function readBackDateTimeOriginal(bytes) {
  const file = path.join(dir(), `readback-${crypto.randomBytes(6).toString('hex')}.jpg`);
  fs.writeFileSync(file, bytes);
  try {
    return execFileSync('magick', ['identify', '-format', '%[EXIF:DateTimeOriginal]', file]).toString().trim();
  } finally {
    fs.rmSync(file, { force: true });
  }
}

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

// uploadProof posts a chore-proof photo exactly as the row's capture form does
// (multipart: csrf_token, kind, photo) and returns { status, body }. Redirects
// are not followed, so a 303 means the upload was accepted.
async function uploadProof(page, { instanceId, kind, bytes, csrfToken, fileName = 'proof.jpg', mimeType = 'image/jpeg' }) {
  const res = await page.request.post(`/tasks/${instanceId}/photos`, {
    multipart: {
      csrf_token: csrfToken,
      kind,
      photo: { name: fileName, mimeType, buffer: bytes },
    },
    maxRedirects: 0,
  });
  return { status: res.status(), body: await res.text() };
}

function proofPhotoCount(instanceId, kind) {
  const filter = kind ? ` AND kind = '${kind}'` : '';
  return Number(psql(
    `SELECT count(*) FROM nestova.task_instance_photo WHERE task_instance_id = '${instanceId}'${filter};`,
  ).trim());
}

function cleanup() {
  fs.rmSync(DIR, { recursive: true, force: true });
}

module.exports = {
  householdA,
  memberId,
  seedTask,
  seedInstance,
  instanceRow,
  tasksToken,
  exifDate,
  minutesFromNow,
  spliceExif,
  noiseJpeg,
  proofJpeg,
  readBackDateTimeOriginal,
  uploadProof,
  proofPhotoCount,
  dir,
  cleanup,
};
