// Kiosk idle-timeout screensaver (NES-128). After idleTimeoutMs with no touch
// anywhere on the page, shows the full-screen photo-album overlay
// (web/components/kiosk.templ's kioskScreensaver, driven by 'x-show'). This
// never navigates: the underlying tab page is never replaced, so dismissing
// the screensaver by touching it is exactly "return to the last tab" — there
// is no separate state to restore.
document.addEventListener('alpine:init', () => {
  Alpine.data('kioskIdle', () => ({
    screensaverActive: false,
    idleTimer: null,
    idleTimeoutMs: 120000,
    boundActivity: null,

    init() {
      const configured = parseInt(this.$el.dataset.idleTimeoutMs || '', 10);
      this.idleTimeoutMs = Number.isFinite(configured) && configured > 0 ? configured : this.idleTimeoutMs;

      // Any touch, click, or keypress anywhere on the page counts as
      // activity — the whole kiosk is the "keep awake" surface, not just the
      // tab content, so a touch on the screensaver overlay itself (handled by
      // dismiss(), which also calls this) is not special-cased here.
      this.boundActivity = () => this.resetIdleTimer();
      document.addEventListener('touchstart', this.boundActivity, { passive: true });
      document.addEventListener('mousedown', this.boundActivity, { passive: true });
      document.addEventListener('keydown', this.boundActivity);

      this.resetIdleTimer();
    },

    // Alpine calls destroy() when this root component is torn down (never
    // expected for the <body> root in practice, but kept for symmetry with
    // this codebase's other Alpine.data components and to avoid a leaked
    // timer/listeners in a test harness that mounts/unmounts the component).
    destroy() {
      if (this.idleTimer) {
        clearTimeout(this.idleTimer);
        this.idleTimer = null;
      }
      if (this.boundActivity) {
        document.removeEventListener('touchstart', this.boundActivity);
        document.removeEventListener('mousedown', this.boundActivity);
        document.removeEventListener('keydown', this.boundActivity);
        this.boundActivity = null;
      }
    },

    resetIdleTimer() {
      if (this.idleTimer) {
        clearTimeout(this.idleTimer);
      }
      this.idleTimer = setTimeout(() => {
        this.screensaverActive = true;
      }, this.idleTimeoutMs);
    },

    // Called by the screensaver overlay's own click/touchstart handler.
    dismiss() {
      this.screensaverActive = false;
      this.resetIdleTimer();
    },
  }));
});
