// Tells the member when an HTMX request fails because the network dropped
// (NES-202). htmx swallows a failed send silently — no swap, no error — so the
// form just looks as if the click was ignored. The service worker deliberately
// passes HTML and HX requests through to the network (web/static/sw.js), so
// nothing is queued and nothing was saved; the message says so.
//
// The alert lives in the member shell (web/components/layout.templ) as a
// hidden role="alert" element. Showing it is what makes assistive tech
// announce it. It clears when the browser reports the connection is back.
(function () {
  const alertEl = document.getElementById('network-alert');
  if (!alertEl) return;

  const show = () => { alertEl.hidden = false; };
  const hide = () => { alertEl.hidden = true; };

  // htmx:sendError fires when the request never reached the server (offline,
  // DNS, connection reset). htmx:responseError fires for 4xx/5xx, which are
  // real server answers, so only treat one as a network failure when the
  // browser also reports it is offline.
  document.body.addEventListener('htmx:sendError', show);
  document.body.addEventListener('htmx:responseError', () => {
    if (!navigator.onLine) show();
  });
  window.addEventListener('online', hide);
})();
