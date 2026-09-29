// Shared persona fixtures for the NES-171 checklist run.
//
// The checklist's tests are written against five personas (owner, adult,
// child, an owner of a SECOND household for tenant-isolation tests, and a
// kiosk device). They are defined here so every spec agrees on the
// credentials rather than each one inventing its own.
const PASSWORD = 'testtest1234';

const PERSONAS = {
  owner: { email: 'owner@test.local', password: PASSWORD, displayName: 'Owner A', role: 'owner' },
  adult: { email: 'adult@test.local', password: PASSWORD, displayName: 'Adult A', role: 'adult' },
  child: { email: 'child@test.local', password: PASSWORD, displayName: 'Child A', role: 'child' },
  otherOwner: { email: 'owner@other.local', password: PASSWORD, displayName: 'Owner B', role: 'owner' },
};

const HOUSEHOLD_A = 'Household A';
const HOUSEHOLD_B = 'Household B';

module.exports = { PASSWORD, PERSONAS, HOUSEHOLD_A, HOUSEHOLD_B };
