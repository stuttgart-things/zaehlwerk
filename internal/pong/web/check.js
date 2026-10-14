// zaehlwerk-pong: a button check's page.
//
// Everything on this page is htmx and SSE except this: the check's token comes
// in the fragment of the lobby's redirect and goes back in a header, which htmx
// cannot read from there. The token ends the check and does nothing else; a
// check presses nothing (ADR-0006). The copy button is here because it is next
// to it, and a clipboard is not something htmx does.
(() => {
  'use strict';
  const root = document.getElementById('check');
  if (!root) return;
  const id = root.dataset.game;
  const key = 'pong-token-' + id;
  let token = '';
  try {
    if (location.hash.length > 1) {
      sessionStorage.setItem(key, location.hash.slice(1));
      history.replaceState(null, '', location.pathname);
    }
    token = sessionStorage.getItem(key) || '';
  } catch (e) { /* no storage: this tab watches */ }

  const end = document.getElementById('check-end');
  const copy = document.getElementById('check-copy');
  // Shown by what the page holds: end while it runs and this tab has the
  // token, copy once there is a summary to copy.
  const show = () => {
    const over = !!document.getElementById('summary');
    end.hidden = over || token === '';
    copy.hidden = !over;
  };
  end.addEventListener('click', async () => {
    end.disabled = true;
    await fetch('/games/' + id + '/end', { method: 'POST', headers: { 'X-Pong-Token': token } }).catch(() => {});
    end.disabled = false;
  });
  copy.addEventListener('click', async () => {
    const text = document.getElementById('summary').textContent;
    try {
      await navigator.clipboard.writeText(text);
      copy.textContent = 'Kopiert';
    } catch (e) {
      // No clipboard outside a secure context: select it for Ctrl+C instead.
      window.getSelection().selectAllChildren(document.getElementById('summary'));
    }
  });
  // Watched rather than hooked into htmx: the card is swapped by the SSE
  // extension, and what matters is only whether a summary is in it.
  new MutationObserver(show).observe(root, { childList: true, subtree: true });
  show();
})();
