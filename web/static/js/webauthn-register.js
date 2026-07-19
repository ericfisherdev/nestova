// Passkey registration (NES-136): drives the WebAuthn "create a credential"
// ceremony from the "Your devices" settings section. Registers
// Alpine.data("webauthnRegister") for the #webauthn-devices element
// (web/components/webauthn_settings.templ), which carries the begin/finish
// endpoint URLs and the CSRF token as data attributes.
//
// The ceremony:
//   1. POST beginUrl (CSRF token in the X-CSRF-Token header — see finish()'s
//      own doc for why neither the CSRF token nor the nickname travel in
//      the URL) -> JSON credential creation options.
//   2. PublicKeyCredential.parseCreationOptionsFromJSON(options) converts
//      the JSON (base64url strings) into the ArrayBuffers
//      navigator.credentials.create() actually expects. This is the
//      WebAuthn Level 3 standard JSON serialization the go-webauthn server
//      library is built around (its own custom base64url JSON
//      (un)marshaling matches this exact wire format), so no hand-rolled
//      base64url<->ArrayBuffer conversion is written here on either side of
//      the ceremony.
//   3. navigator.credentials.create({ publicKey: options }) prompts the
//      platform authenticator (Face ID, fingerprint unlock, Windows Hello).
//   4. credential.toJSON() converts the resulting PublicKeyCredential back
//      to that same JSON wire format. Rather than persisting immediately,
//      an inline nickname field (in webauthn_settings.templ, not a native
//      window.prompt() dialog — unstyled, unvalidatable, and inconsistent
//      with every other form on this page) is revealed for the member to
//      name the device before it is saved.
//   5. Confirming that field POSTs finishUrl (the credential JSON plus the
//      nickname, both in the body) -> {"ok":true} on success.
//   6. On success, reload the page — the simplest way to show the new
//      device with its server-rendered created/last-used labels, with no
//      client-side row templating to keep in sync with the server's own.
//
// PublicKeyCredential.parseCreationOptionsFromJSON/credential.toJSON() are
// standard WebAuthn Level 3 browser methods; no polyfill or third-party
// client library is used, matching this codebase's no-Node-toolchain,
// vanilla-JS convention (see web/static/js/upload-queue.js's own doc for the
// same reasoning applied to XHR instead of a fetch wrapper library).
document.addEventListener('alpine:init', () => {
  Alpine.data('webauthnRegister', () => ({
    busy: false,
    error: '',
    // awaitingNickname is true between a successful
    // navigator.credentials.create() and the member confirming (or
    // cancelling) the inline nickname field — pendingCredentialJSON holds
    // the already-created credential until then.
    awaitingNickname: false,
    nickname: '',
    pendingCredentialJSON: null,
    beginUrl: '',
    finishUrl: '',
    csrfToken: '',

    init() {
      this.beginUrl = this.$el.dataset.beginUrl;
      this.finishUrl = this.$el.dataset.finishUrl;
      this.csrfToken = this.$el.dataset.csrfToken;
    },

    // startRegistration runs the browser-side ceremony (begin -> the
    // authenticator prompt) and then reveals the nickname field; it does
    // NOT itself persist anything server-side yet — see confirmNickname.
    async startRegistration() {
      if (this.busy) {
        return;
      }
      this.busy = true;
      this.error = '';
      try {
        const creationOptionsJSON = await this.begin();
        const publicKey = PublicKeyCredential.parseCreationOptionsFromJSON(creationOptionsJSON.publicKey);
        const credential = await navigator.credentials.create({ publicKey });
        this.pendingCredentialJSON = credential.toJSON();
        this.awaitingNickname = true;
        this.$nextTick(() => this.$refs.webauthnNickname && this.$refs.webauthnNickname.focus());
      } catch (err) {
        this.error = describeWebAuthnError(err);
      } finally {
        this.busy = false;
      }
    },

    // confirmNickname persists the already-created credential (from
    // startRegistration) under the member-entered nickname.
    async confirmNickname() {
      if (this.busy || !this.pendingCredentialJSON) {
        return;
      }
      this.busy = true;
      this.error = '';
      try {
        await this.finish(this.pendingCredentialJSON, this.nickname);
        // Reloading is the simplest way to show the new device in the list
        // with the server's own formatted created-at label — there is no
        // client-side device-row template to keep in sync with
        // web/components/webauthn_settings.templ's own rendering.
        window.location.reload();
      } catch (err) {
        // The pending credential's challenge is single-use and is cleared
        // server-side on EVERY finish attempt, win or lose (see
        // WebAuthnWebHandlers.RegisterFinish's own doc) — so a failed
        // finish() can never be retried with the same pendingCredentialJSON.
        // Reset to the pre-ceremony state so "Add a passkey" reappears and
        // startRegistration() can run a fresh ceremony, instead of leaving
        // the member stuck on a nickname field whose "Save" button would
        // just fail again until they reload the page themselves.
        this.error = describeWebAuthnError(err);
        this.awaitingNickname = false;
        this.pendingCredentialJSON = null;
        this.busy = false;
      }
    },

    async begin() {
      const res = await fetch(this.beginUrl, {
        method: 'POST',
        headers: { 'X-CSRF-Token': this.csrfToken },
      });
      this.assertOwnJSONResponse(res, 'begin');
      return res.json();
    },

    async finish(credentialJSON, nickname) {
      const res = await fetch(this.finishUrl, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': this.csrfToken,
        },
        // The nickname travels in the JSON body (not the URL, and not a
        // second header) alongside the credential's own fields — see
        // WebAuthnWebHandlers.RegisterFinish's own doc for why: a header
        // value cannot safely carry arbitrary member-supplied text (a
        // nickname may contain non-ASCII characters), and
        // protocol.ParseCredentialCreationResponseBody (the server's
        // credential parser) silently ignores JSON keys it does not
        // recognize, so adding "nickname" alongside credentialJSON's own
        // keys does not interfere with it.
        body: JSON.stringify({ ...credentialJSON, nickname }),
      });
      this.assertOwnJSONResponse(res, 'finish');
    },

    // assertOwnJSONResponse guards against RequireStepUp's own redirect
    // (NES-135): a stale/never-verified session sends begin/finish to a
    // 303 -> /login/mfa, which fetch() follows transparently by default —
    // the FINAL response's status is the login MFA page's 200 (res.ok
    // stays true), but its body is HTML, not JSON, so res.json() would
    // throw a confusing parse error instead of this handler's own real
    // outcome. res.redirected is the Fetch API's own purpose-built signal
    // for "a redirect was followed to reach this response" (true only for
    // an ACTUAL redirect, unlike a plain non-JSON error body — this
    // endpoint's own error responses are never redirects, just non-2xx
    // status codes, so checking res.redirected specifically — rather than
    // e.g. the response's content-type — cannot misfire on those): when
    // true, this navigates the page to the step-up prompt directly instead
    // of attempting to parse a login page as this endpoint's JSON.
    assertOwnJSONResponse(res, step) {
      if (res.redirected) {
        window.location.href = res.url;
        throw new Error(`webauthn: ${step} redirected to step-up`);
      }
      if (!res.ok) {
        throw new Error(`webauthn: ${step} failed`);
      }
    },
  }));
});

// describeWebAuthnError maps a WebAuthn ceremony failure to a short,
// member-facing message. NotAllowedError covers both an explicit
// member-initiated cancel and a ceremony timeout — the browser does not
// distinguish the two itself, so neither does this.
function describeWebAuthnError(err) {
  if (err && err.name === 'NotAllowedError') {
    return 'Passkey setup was cancelled or timed out.';
  }
  if (err && err.name === 'InvalidStateError') {
    return 'This device is already registered as a passkey.';
  }
  return 'Could not register this passkey. Please try again.';
}
