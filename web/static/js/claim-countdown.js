// Live countdown badge for a claimed chore whose claim carries an expiry
// risk (NES-118). Ticks down every second from the server-supplied deadline
// using the browser's own clock (no polling), and — once the deadline
// passes — dispatches a "claim-expired" event on the row element so HTMX
// (via the row's own hx-trigger="claim-expired") refreshes just that row to
// its post-sweep, claimable state. The background scheduler sweep still owns
// the actual revert; this only asks the server to re-render once the client
// believes the claim has lapsed, so a client clock running slightly ahead of
// the server just means one harmless extra fetch before the sweep catches up.
document.addEventListener('alpine:init', () => {
  Alpine.data('claimCountdown', (expiresAtISO) => ({
    label: '',
    timer: null,
    expiresAtMs: new Date(expiresAtISO).getTime(),

    init() {
      this.tick();
      this.timer = setInterval(() => this.tick(), 1000);
    },

    // Alpine calls destroy() when the row is removed or swapped out (e.g. by
    // an unrelated HTMX update elsewhere on the page); stop the timer so it
    // never keeps ticking against a detached element.
    destroy() {
      if (this.timer) {
        clearInterval(this.timer);
        this.timer = null;
      }
    },

    tick() {
      const remainingMs = this.expiresAtMs - Date.now();
      if (remainingMs <= 0) {
        this.label = 'Claim expiring…';
        if (this.timer) {
          clearInterval(this.timer);
          this.timer = null;
        }
        // Dispatched on $el (this row's own div), where the row's own
        // hx-trigger="claim-expired" listener is registered.
        this.$dispatch('claim-expired');
        return;
      }
      this.label = `expires in ${formatRemaining(remainingMs)}`;
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
