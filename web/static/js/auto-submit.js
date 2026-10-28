// Submits a form as soon as one of its controls changes (NES-211).
//
// Any element carrying data-autosubmit submits its enclosing form on change.
// The proof-photo capture input (web/components/tasks.templ's proofPhotoSlot)
// uses it so that taking the photo IS the upload action. This replaces an
// inline onchange handler, which a strict Content-Security-Policy blocks.
//
// One delegated listener on document, rather than one per input, so inputs
// swapped in by htmx need no re-binding.
document.addEventListener('change', (event) => {
  const target = event.target;
  if (target instanceof Element && target.matches('[data-autosubmit]') && target.form) {
    target.form.requestSubmit();
  }
});
