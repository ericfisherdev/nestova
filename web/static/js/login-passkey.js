// Passkey login and step-up (NES-137): drives the WebAuthn "get an
// assertion" ceremony from TWO pages that share this exact ceremony shape —
// the usernameless "Sign in with passkey" button (web/components/login.templ)
// and the "use your passkey" step-up option (web/components/login_mfa.templ).
// Registers Alpine.data("passkeyAssertion") for whichever element on the
// current page carries data-begin-url/data-finish-url/data-csrf-token/
// data-next; both templates instantiate the SAME component against their
// own endpoint pair (usernameless /login/passkey/... vs. targeted
// /login/mfa/passkey/...) — the ceremony itself does not care which.
//
// The ceremony:
//   1. GET beginUrl -> JSON assertion options (wrapped in a top-level
//      "publicKey" key, mirroring webauthn-register.js's own creation
//      options shape).
//   2. PublicKeyCredential.parseRequestOptionsFromJSON(options) converts
//      the JSON (base64url strings) into the ArrayBuffers
//      navigator.credentials.get() actually expects — the WebAuthn Level 3
//      counterpart to webauthn-register.js's parseCreationOptionsFromJSON,
//      so no hand-rolled base64url<->ArrayBuffer conversion is written here
//      either.
//   3. navigator.credentials.get({ publicKey: options }) prompts the
//      platform authenticator.
//   4. credential.toJSON() converts the resulting PublicKeyCredential back
//      to that same JSON wire format and is POSTed directly to finishUrl
//      (CSRF token in the X-CSRF-Token header — see webauthn-register.js's
//      own doc for why neither it nor any other value travels in the URL,
//      except `next`, which the SERVER re-sanitizes on the way back — see
//      LoginPasskeyHandlers.Finish's own doc for why the client's copy of
//      `next` is never trusted directly).
//   5. On success the response is {"redirect": "<server-sanitized path>"}
//      — this navigates there directly, never reusing whatever `next` this
//      page itself was loaded with.
//
// PublicKeyCredential.parseRequestOptionsFromJSON/credential.toJSON() are
// standard WebAuthn Level 3 browser methods; no polyfill or third-party
// client library is used, matching this codebase's no-Node-toolchain,
// vanilla-JS convention (see webauthn-register.js's own doc for the same
// reasoning).

// FETCH_TIMEOUT_MS bounds how long begin()/finish() wait for the server
// before giving up — a hung request would otherwise leave the ceremony
// stuck at busy=true indefinitely (the authenticator prompt itself has its
// own OS-level timeout, but a slow/unresponsive SERVER does not). Thirty
// seconds comfortably exceeds a real request's normal latency without
// forcing the member through an unreasonably long wait on a genuine outage.
const FETCH_TIMEOUT_MS = 30000;

document.addEventListener('alpine:init', () => {
  Alpine.data('passkeyAssertion', () => ({
    busy: false,
    error: '',
    beginUrl: '',
    finishUrl: '',
    csrfToken: '',
    next: '',

    init() {
      this.beginUrl = this.$el.dataset.beginUrl;
      this.finishUrl = this.$el.dataset.finishUrl;
      this.csrfToken = this.$el.dataset.csrfToken;
      this.next = this.$el.dataset.next || '';
    },

    // start runs the full ceremony (begin -> the authenticator prompt ->
    // finish) and navigates to the server-supplied redirect on success.
    async start() {
      if (this.busy) {
        return;
      }
      this.busy = true;
      this.error = '';
      try {
        const optionsJSON = await this.begin();
        const publicKey = PublicKeyCredential.parseRequestOptionsFromJSON(optionsJSON.publicKey);
        const credential = await navigator.credentials.get({ publicKey });
        const redirect = await this.finish(credential.toJSON());
        window.location.href = redirect;
      } catch (err) {
        this.error = describePasskeyLoginError(err);
        this.busy = false;
      }
    },

    async begin() {
      const res = await this.fetchWithTimeout(this.beginUrl, { method: 'GET' });
      this.assertOwnJSONResponse(res, 'begin');
      return res.json();
    },

    // finish returns the server-sanitized redirect target from a
    // successful response.
    async finish(credentialJSON) {
      const url = this.finishUrl + '?next=' + encodeURIComponent(this.next);
      const res = await this.fetchWithTimeout(url, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': this.csrfToken,
        },
        body: JSON.stringify(credentialJSON),
      });
      this.assertOwnJSONResponse(res, 'finish');
      const data = await res.json();
      return data.redirect || '/';
    },

    // fetchWithTimeout aborts the request after FETCH_TIMEOUT_MS, throwing
    // fetch()'s own AbortError (a DOMException) into the SAME catch block
    // start() already has — no separate error/reset path is needed:
    // describePasskeyLoginError distinguishes it from a member-cancelled
    // ceremony, and start()'s own finally-equivalent (the catch block)
    // already resets busy back to false.
    async fetchWithTimeout(url, options) {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
      try {
        return await fetch(url, { ...options, signal: controller.signal });
      } finally {
        clearTimeout(timeout);
      }
    },

    // assertOwnJSONResponse guards against a pending-state redirect (e.g.
    // LoginMFAHandlers.PasskeyBegin/PasskeyFinish redirecting to /login
    // when no pending member exists — a session race, not the common
    // case): fetch() follows a redirect transparently by default, so the
    // FINAL response's status can be a 200 (res.ok stays true) with an
    // HTML body, not JSON. res.redirected is the Fetch API's own
    // purpose-built signal for "a redirect was followed to reach this
    // response" — true only for an ACTUAL redirect, never for this
    // endpoint's own plain non-2xx error responses — so checking it
    // specifically (mirroring webauthn-register.js's identical guard)
    // cannot misfire on those.
    assertOwnJSONResponse(res, step) {
      if (res.redirected) {
        window.location.href = res.url;
        throw new Error(`passkey: ${step} redirected`);
      }
      if (!res.ok) {
        throw new Error(`passkey: ${step} failed`);
      }
    },
  }));
});

// describePasskeyLoginError maps a WebAuthn ceremony failure to a short,
// member-facing message — mirroring webauthn-register.js's own
// describeWebAuthnError, but worded for signing IN rather than registering
// a NEW device (InvalidStateError, specific to a duplicate registration
// attempt, cannot occur during a login/assertion ceremony at all).
// AbortError here specifically means fetchWithTimeout gave up on a hung
// server request — a different failure than NotAllowedError's "the
// AUTHENTICATOR prompt was cancelled or timed out" (fetch's own AbortError
// is also a DOMException with that same name, but NotAllowedError comes
// from navigator.credentials.get() itself, never from fetch()).
function describePasskeyLoginError(err) {
  if (err && err.name === 'NotAllowedError') {
    return 'Passkey sign-in was cancelled or timed out.';
  }
  if (err && err.name === 'AbortError') {
    return 'The server took too long to respond. Please try again.';
  }
  // Fallback-agnostic: this component also serves /login/mfa's step-up
  // option, where the alternative is a TOTP code or recovery code, not a
  // password.
  return 'Could not complete passkey verification. Please try again or use another verification method.';
}
