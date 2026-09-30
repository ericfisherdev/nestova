// Absolute paths for the external commands the e2e helpers run.
//
// Commands are never looked up through PATH: a writable directory early in
// PATH could substitute a different binary for docker, go or magick. Each one
// resolves instead to the first match in a fixed list of system directories.
// A machine that installs a command elsewhere names it explicitly with
// NESTOVA_E2E_<NAME>_BIN, which must be an absolute path to an executable.
const fs = require('node:fs');
const path = require('node:path');

const TRUSTED_DIRS = ['/usr/local/bin', '/usr/bin', '/bin', '/usr/local/go/bin', '/usr/sbin', '/sbin'];

const resolved = new Map();

function isExecutable(file) {
  try {
    fs.accessSync(file, fs.constants.X_OK);
    return fs.statSync(file).isFile();
  } catch {
    return false;
  }
}

function overrideFor(name) {
  const key = `NESTOVA_E2E_${name.toUpperCase()}_BIN`;
  const value = process.env[key];
  if (!value) return null;
  if (!path.isAbsolute(value) || !isExecutable(value)) {
    throw new Error(`${key}=${value} must be an absolute path to an executable`);
  }
  return value;
}

// tool returns the absolute path of the named command. It throws when the
// command is in none of the trusted directories, so a missing dependency
// fails at the call site instead of as a confusing ENOENT later.
function tool(name) {
  if (resolved.has(name)) return resolved.get(name);
  const found = overrideFor(name)
    ?? TRUSTED_DIRS.map((dir) => path.join(dir, name)).find(isExecutable);
  if (!found) {
    throw new Error(`${name} not found in ${TRUSTED_DIRS.join(', ')}; set NESTOVA_E2E_${name.toUpperCase()}_BIN`);
  }
  resolved.set(name, found);
  return found;
}

// hasTool reports whether the named command resolves, for specs that skip
// when an optional dependency is missing.
function hasTool(name) {
  try {
    tool(name);
    return true;
  } catch {
    return false;
  }
}

module.exports = { tool, hasTool };
