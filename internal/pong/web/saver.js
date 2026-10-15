// zaehlwerk-pong: the screensaver's game page (ADR-0013).
//
// Only the tab that holds the game's token acts here. Another tab watching the
// same demo, from the lobby say, neither ends it nor starts the next one.
(() => {
  'use strict';
  const root = document.getElementById('pong');
  if (!root) return;
  const id = root.dataset.game;
  const token = () => {
    try {
      const t = sessionStorage.getItem('pong-token-' + id) || '';
      return t.startsWith('guest:') ? '' : t;
    } catch (e) { return ''; }
  };

  // Over, however it ended: a short look at the result, then the next demo.
  // A table taken in between is the screensaver's to find out, and it waits.
  const AGAIN_MS = 10000;
  let again = 0;
  document.body.addEventListener('htmx:afterSwap', (e) => {
    if (again || !token() || !e.target.querySelector || !e.target.querySelector('.banner.over')) return;
    again = setTimeout(() => { location.href = '/screensaver'; }, AGAIN_MS);
  });

  // The screen moving on ends the demo: zaehlwerk's watch screen drops this
  // page the moment somebody checks in. Left to itself, the demo would hold
  // the panel until PONG_BROWSER_IDLE, minutes in which the players see bots
  // on the matrix instead of their names. keepalive lets the request outlive
  // the page that sends it.
  addEventListener('pagehide', () => {
    const t = token();
    if (!t) return;
    fetch('/games/' + id + '/end', { method: 'POST', keepalive: true, headers: { 'X-Pong-Token': t } })
      .catch(() => {});
  });
})();
