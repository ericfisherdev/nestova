// Live countdown badge for a claimed chore whose claim carries an expiry
// risk (NES-118). Ticks down every second from the server-supplied deadline
// using the browser's own clock (no polling), and — once the deadline
// passes — dispatches a "claim-expired" event on the row element so HTMX
// (via the row's own hx-trigger="claim-expired") refreshes the enclosing
// #task-groups container to its post-sweep state. The background scheduler
// sweep still owns the actual revert; this only asks the server to re-render
// once the client believes the claim has lapsed.
document.addEventListener('alpine:init', () => {
  Alpine.data('claimCountdown', (expiresAtISO) => ({
    label: '',
    tickTimer: null,
    retryTimer: null,
    expiresAtMs: new Date(expiresAtISO).getTime(),
    // retryDelayMs bounds the wait before re-dispatching claim-expired when
    // the countdown is ALREADY past its deadline the moment this component
    // mounts. That happens when a group refresh completes before the
    // background sweep has actually reverted the claim: the freshly
    // rendered row still carries the same, already-expired deadline.
    // Dispatching again immediately would tight-loop (refresh -> still
    // expired -> refresh -> ...) with no progress until the sweep catches
    // up. Waiting gives the scheduler's poll cadence room to run the sweep
    // before the next attempt; a live countdown reaching zero during this
    // session still dispatches immediately (see tick()).
    retryDelayMs: 30000,

    init() {
      if (this.isExpired()) {
        this.label = 'Claim expiring…';
        this.retryTimer = setTimeout(() => this.dispatchExpired(), this.retryDelayMs);
        return;
      }
      this.tick();
      this.tickTimer = setInterval(() => this.tick(), 1000);
    },

    // Alpine calls destroy() when the row is removed or swapped out (e.g. by
    // an unrelated HTMX update elsewhere on the page); stop any pending
    // timer so it never fires against a detached element.
    destroy() {
      this.clearTimers();
    },

    clearTimers() {
      if (this.tickTimer) {
        clearInterval(this.tickTimer);
        this.tickTimer = null;
      }
      if (this.retryTimer) {
        clearTimeout(this.retryTimer);
        this.retryTimer = null;
      }
    },

    isExpired() {
      return this.expiresAtMs - Date.now() <= 0;
    },

    tick() {
      if (this.isExpired()) {
        // A live countdown reaching zero during this session — the common,
        // expected case — dispatches immediately.
        this.label = 'Claim expiring…';
        this.clearTimers();
        this.dispatchExpired();
        return;
      }
      this.label = `expires in ${formatRemaining(this.expiresAtMs - Date.now())}`;
    },

    // Dispatched on $el (this row's own div), where the row's own
    // hx-trigger="claim-expired" listener is registered.
    dispatchExpired() {
      this.$dispatch('claim-expired');
    },
  }));
});

// formatRemaining renders a millisecond duration as a compact "Xh Ym" label
// (or "Ym" once under an hour) for the countdown badge.
function formatRemaining(ms) {
  const totalMinutes = Math.max(1, Math.round(ms / 60000));
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  return hours > 0 ? `${hours}h ${minutes}m` : `${minutes}m`;
}
