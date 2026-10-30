// zaehlwerk-pong: the faults of a game while it plays, for the tab that plays
// it. game.js says which tab that is, once tab.js has settled it, and hands
// over the token; every other tab never sees the form. A change is sent as a
// whole, as POST /games names the faults, and the service checks it again.
(() => {
  'use strict';
  const root = document.getElementById('pong');
  const tune = document.getElementById('tune');
  const form = document.getElementById('tune-form');
  const note = document.getElementById('tune-note');
  if (!root || !tune || !form) return;
  const id = root.dataset.game;
  const told = note.textContent;
  let token = '';

  root.addEventListener('pong:host', (e) => {
    token = e.detail;
    tune.hidden = false;
  });

  form.addEventListener('change', async () => {
    if (!token) return;
    const faults = { correct: form.elements.correct.checked };
    for (const input of form.querySelectorAll('input[type=number]')) {
      faults[input.name] = Number(input.value || 0);
    }
    let answer;
    try {
      answer = await fetch('/games/' + id + '/faults', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Pong-Token': token },
        body: JSON.stringify(faults),
      });
    } catch (err) {
      note.textContent = 'Nicht übernommen: der Dienst ist nicht erreichbar.';
      return;
    }
    if (answer.ok) {
      note.textContent = 'Übernommen. ' + told;
      return;
    }
    // The service's reason is for the log; the page says what it means.
    note.textContent = {
      400: 'Nicht übernommen: jeder Anteil muss zwischen 0 und 1 liegen.',
      403: 'Nicht übernommen: dieses Spiel spielt ein anderer Tab.',
      409: 'Nicht übernommen: das Spiel ist vorbei.',
    }[answer.status] || 'Nicht übernommen (' + answer.status + ').';
  });
})();
