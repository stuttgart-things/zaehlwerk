// zaehlwerk-pong: the page at /live, one address for a screen that stays on.
//
// It is /play/{id} for the game playing now, and only watches. Once that game
// is over, however it ended, it leaves the result up for a moment and loads
// /live again: the next game, or the waiting page until there is one. The
// game is asked for rather than read from the board, so a game that is gone
// -- evicted, or the service restarted -- moves the page on as well.
(() => {
  'use strict';
  const id = document.body.dataset.live;
  if (!id) return;

  const POLL_MS = 3000;
  const RESULT_MS = 8000;
  let next = 0;
  const moveOn = (after) => {
    if (!next) next = setTimeout(() => { location.replace('/live'); }, after);
  };

  setInterval(async () => {
    try {
      const r = await fetch('/games/' + id, { cache: 'no-store' });
      if (r.status === 404) return moveOn(0);
      if (!r.ok) return;
      const v = await r.json();
      if (v.status !== 'playing') moveOn(RESULT_MS);
    } catch (e) { /* the service is away; ask again on the next tick */ }
  }, POLL_MS);
})();
