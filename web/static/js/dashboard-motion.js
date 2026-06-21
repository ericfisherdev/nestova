// Dashboard animation polish (NES-77). A small GSAP entrance pass for the
// A-Hearth shell plus a settle on HTMX swaps. Uses gsap.from so the content is
// at its natural (visible) state without JS, and gsap.matchMedia so the motion
// only runs when the user has not asked to reduce it (and is reverted otherwise).
(function () {
  if (typeof gsap === 'undefined') return;

  // Entrance: stagger the sidebar nav in from the left, then ease the main
  // content up. Only under prefers-reduced-motion: no-preference.
  gsap.matchMedia().add('(prefers-reduced-motion: no-preference)', function () {
    var tl = gsap.timeline({ defaults: { ease: 'power2.out' } });
    tl.from('#sidebar nav a', { autoAlpha: 0, x: -12, duration: 0.4, stagger: 0.05 });
    tl.from('#main-content', { autoAlpha: 0, y: 14, duration: 0.5 }, '-=0.2');
  });

  // Settle freshly swapped content (skipped under reduced motion).
  document.body.addEventListener('htmx:afterSwap', function (evt) {
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    var target = evt && evt.detail && evt.detail.target;
    if (target) {
      gsap.from(target, { autoAlpha: 0, y: 8, duration: 0.3, ease: 'power2.out' });
    }
  });
})();
