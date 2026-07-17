// Required environment variables for the e2e suite; fail fast with a clear
// message instead of falling back to committed credentials.
function requireEnv(name) {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} must be set to run the e2e suite`);
  }
  return value;
}

module.exports = { requireEnv };
