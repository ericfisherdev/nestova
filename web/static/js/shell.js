// Member-shell state for the narrow-screen navigation drawer (NES-211).
//
// Registers Alpine.data('shell') for the <body> in web/components/layout.templ.
// The drawer's open/close logic lives here, not in markup expressions, because
// the Alpine CSP build cannot evaluate arrow functions or statement blocks.
//
// Opening moves focus into the drawer (navPanel); closing returns it to the
// toggle button (navToggle) so keyboard users land back where they started.
document.addEventListener('alpine:init', () => {
  Alpine.data('shell', () => ({
    open: false,

    openNav() {
      this.open = true;
      this.$nextTick(() => this.$refs.navPanel.focus());
    },

    closeNav() {
      this.open = false;
      this.$refs.navToggle.focus();
    },

    // Escape only does something while the drawer is open, so it never steals
    // the key from other widgets on the page.
    onEscape() {
      if (this.open) {
        this.closeNav();
      }
    },
  }));
});
