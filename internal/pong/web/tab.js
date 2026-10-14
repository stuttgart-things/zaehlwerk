// zaehlwerk-pong: one tab per token (#97).
//
// A page's token lives in sessionStorage, and "duplicate tab" copies
// sessionStorage with everything in it. A copy therefore cannot tell from its
// storage that it is one: whatever a tab writes there to mark itself, the copy
// has too. What it can do is ask the other tabs of this browser, which share a
// BroadcastChannel. A tab holding the token answers; a tab that is answered is
// the copy, drops the token and watches. A reload of the original asks as
// well, finds nobody else holding it, and keeps its part.
//
// The token itself is not sent on the channel. A game has at most one host
// token, one guest token and one check token, so the part names it.
(() => {
  'use strict';
  // A holder answers within a task or two; this is ample for a busy phone and
  // short enough that nobody presses before it is over.
  const ASK_MS = 300;

  window.pongTab = {
    // claim resolves to true if this tab may keep the token for part, and
    // answers later askers for as long as holds() says it still has it.
    claim(id, part, holds) {
      // Without BroadcastChannel (only very old browsers) a tab cannot ask and
      // keeps its token, as before #97: a copy plays into nothing, and the
      // service still counts every press once.
      if (typeof BroadcastChannel !== 'function') return Promise.resolve(true);
      const channel = new BroadcastChannel('pong-' + id);
      let asking = true, answered = false;
      channel.onmessage = (e) => {
        const m = e.data || {};
        if (asking && m.held === part) answered = true;
        // A tab still asking does not answer, or an original and its copy
        // loading together would each find the other and both watch.
        if (!asking && m.ask === part && holds()) channel.postMessage({ held: part });
      };
      channel.postMessage({ ask: part });
      return new Promise((resolve) => setTimeout(() => {
        asking = false;
        if (answered) channel.close();
        resolve(!answered);
      }, ASK_MS));
    },
  };
})();
