// Service worker registration (NES-152), moved out of an inline <script> in
// layout.templ so the page needs no 'unsafe-inline' script source (NES-211).
//
// Guarded on feature detection because navigator.serviceWorker is undefined
// outside a secure context: the kiosk and any plain-HTTP LAN access skip this
// and keep working exactly as before. The catch matters for the same reason —
// the app is fully functional without a worker, so a failed registration must
// never break the page.
if ('serviceWorker' in navigator) {
  window.addEventListener('load', function () {
    navigator.serviceWorker.register('/sw.js').catch(function () {
      // No worker: the offline page and asset caching are unavailable;
      // everything else works normally.
    });
  });
}
