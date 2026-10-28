// Double-submit guard for forms (NES-211).
//
// Registers Alpine.data('submitOnce'). Placed on a <form> with
// x-data="submitOnce" x-on:submit="disableSubmitButtons()", it disables the
// form's submit buttons the moment the form submits, so a second click cannot
// post the form twice. This replaces an inline arrow-function expression the
// Alpine CSP build cannot evaluate.
document.addEventListener('alpine:init', () => {
  Alpine.data('submitOnce', () => ({
    disableSubmitButtons() {
      this.$el.querySelectorAll('button[type=submit]').forEach((button) => {
        button.disabled = true;
      });
    },
  }));
});
