// First-run setup wizard state (NES-211). Registers Alpine.data('setupForm') for
// the form in web/components/setup.templ.
//
// Initial state arrives as JSON in the form's data-state attribute (built by
// alpineSetupState). It is parsed, never evaluated, so a hostile sticky value
// cannot become script. The Supabase rules previously lived in an x-effect
// statement block, which the Alpine CSP build cannot evaluate:
//   - Supabase requires TLS, so an insecure sslmode is upgraded to 'require'.
//   - The port follows the pooler mode, because buildConn requires them to
//     agree: 6543 for the transaction pooler, 5432 for session/direct.
const SECURE_SSL_MODES = new Set(['require', 'verify-ca', 'verify-full']);

document.addEventListener('alpine:init', () => {
  Alpine.data('setupForm', () => ({
    provider: 'postgres',
    sslmode: 'disable',
    poolMode: 'session',
    port: '5432',

    init() {
      Object.assign(this, JSON.parse(this.$el.dataset.state));
      this.applySupabaseRules();
      for (const field of ['provider', 'sslmode', 'poolMode']) {
        this.$watch(field, () => this.applySupabaseRules());
      }
    },

    applySupabaseRules() {
      if (this.provider !== 'supabase') {
        return;
      }
      if (!SECURE_SSL_MODES.has(this.sslmode)) {
        this.sslmode = 'require';
      }
      this.port = this.poolMode === 'transaction' ? '6543' : '5432';
    },
  }));
});
