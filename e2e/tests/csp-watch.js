// Content-Security-Policy violation watcher shared by csp.spec.js and
// first-run-setup.spec.js (NES-212).
//
// A violation reaches the test through three independent channels, because a
// browser reports a blocked load differently depending on what was blocked:
//
//   1. the page's own `securitypolicyviolation` events (blocked scripts,
//      styles, images, fonts, fetches, form posts and inline handlers);
//   2. console errors mentioning "Content Security Policy" (what DevTools
//      shows, and the only signal for some blocks);
//   3. uncaught page errors mentioning it (a blocked eval/new Function throws
//      into the page instead of firing an event).
//
// Violations are forwarded to Node through an exposed binding rather than read
// back from `window` afterwards, so they survive full-page navigations: a
// violation from a page the test has already left is still reported.
const { expect } = require('@playwright/test');

const CSP_MESSAGE = /content security policy/i;

// REPORT_BINDING is the window function the init script calls for each event.
const REPORT_BINDING = '__nestovaReportCspViolation';

// watchCSP starts recording violations on page and returns a handle.
//
// Call it before the first navigation: the init script only runs on documents
// loaded after it is registered.
async function watchCSP(page) {
  const found = [];

  await page.exposeBinding(REPORT_BINDING, (_source, violation) => found.push(violation));
  await page.addInitScript((binding) => {
    window.__cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => {
      const violation = {
        source: 'securitypolicyviolation',
        directive: e.violatedDirective,
        blocked: e.blockedURI,
        page: e.documentURI,
        sample: e.sample,
        disposition: e.disposition,
      };
      window.__cspViolations.push(violation);
      if (typeof window[binding] === 'function') window[binding](violation);
    });
  }, REPORT_BINDING);

  page.on('console', (msg) => {
    if (msg.type() === 'error' && CSP_MESSAGE.test(msg.text())) {
      found.push({ source: 'console', message: msg.text(), page: page.url() });
    }
  });
  page.on('pageerror', (err) => {
    if (CSP_MESSAGE.test(err.message)) {
      found.push({ source: 'pageerror', message: err.message, page: page.url() });
    }
  });

  return {
    violations: () => [...found],
    // expectNone asserts nothing has been reported so far; label names the
    // step in the failure message.
    expectNone(label = 'page') {
      expect(found, `Content-Security-Policy violations on ${label}`).toEqual([]);
    },
  };
}

module.exports = { watchCSP };
