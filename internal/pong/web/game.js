// zaehlwerk-pong: the game in the browser.
//
// Why JavaScript here: a game loop draws sixty frames a second, and htmx
// answers requests, one per swap. ADR-0006 makes this file the exception to
// invariant 8, in this binary only; zaehlwerk's own pages stay htmx, and so
// does everything on this page around the canvas.
//
// What this file decides: who wins a rally, and when a side presses ready.
// What it does not: phase, ends, waiting and serving. They come from zaehlwerk,
// through the service, on /games/{id}/events and in the answer to every press,
// and the canvas draws what they say. Every rally is reported with its number;
// the service keeps the truth and injects the faults (#86) on the way on.
(() => {
  'use strict';
  const root = document.getElementById('pong');
  if (!root) return;
  const id = root.dataset.game;
  const mode = root.dataset.mode; // pvp, pvc or cvc
  const bots = JSON.parse(root.dataset.bots || 'null') || [null, null]; // player a, b
  const canvas = root.querySelector('canvas');
  const g = canvas.getContext('2d');
  const W = canvas.width, H = canvas.height;
  const HALVES = ['left', 'right'];

  // The token comes once, in the fragment of the lobby's redirect, and stays in
  // this tab. A second tab has none and watches: one reporter per game.
  let token = '';
  try {
    const key = 'pong-token-' + id;
    if (location.hash.length > 1) {
      sessionStorage.setItem(key, location.hash.slice(1));
      history.replaceState(null, '', location.pathname);
    }
    token = sessionStorage.getItem(key) || '';
  } catch (e) { /* no storage: this tab watches */ }
  let reporter = token !== '';

  // The table from above: its length across the screen, the net down the
  // middle, a player behind each end.
  const T = { x: 120, y: 80, w: W - 240, h: H - 160 };
  const NET = T.x + T.w / 2, HALF = T.w / 2;
  const PADDLE_X = [T.x - 36, T.x + T.w + 36];
  const PADDLE_HALF = 34, BALL_R = 6;
  const SPIN = 0.25;         // the one spin parameter: paddle speed at contact, as curve
  const START_SPEED = 520;   // px/s along the table
  const MAX_SPEED = 1050;
  const SPEEDUP = 1.06;      // per hit, so a rally gets faster
  const LET = 0.05;          // a serve that touches the net and lands in
  const HUMAN_SPEED = 560;

  let view = null, st = null;
  let shownEnds = ['a', 'b']; // who is drawn left and right; lags zaehlwerk through a change of ends
  let press = 1;              // the next press number
  let queue = Promise.resolve();
  let awaiting = false;       // a rally is reported and the next serve waits for zaehlwerk's answer
  let readySent = {};
  let phaseSince = 0, restingSince = 0, warmServer = 0, serveRallyServer = null;
  let ball = null, resting = null; // resting: the half whose server holds the ball
  let message = '', messageUntil = 0;
  const paddles = [0, 1].map(() => ({ y: T.y + T.h / 2, vy: 0, target: null, since: 0, aim: null }));

  const idx = (p) => (p === 'b' ? 1 : 0);
  const halfOf = (p, ends) => (ends[0] === p ? 0 : 1);
  const botAt = (h) => bots[idx(shownEnds[h])];
  const human = () => (mode === 'pvc' ? 'a' : null);
  const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
  const say = (text, ms = 1400) => { message = text; messageUntil = performance.now() + ms; };
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

  // Sound: a short tock on paddle and table. Off is remembered per browser.
  let audio = null;
  let muted = false;
  try { muted = localStorage.getItem('pong-muted') === '1'; } catch (e) { /* default on */ }
  const muteButton = document.getElementById('mute');
  const endButton = document.getElementById('end');
  const showMute = () => { if (muteButton) muteButton.textContent = muted ? 'Ton an' : 'Ton aus'; };
  const toggleMute = () => {
    muted = !muted;
    try { localStorage.setItem('pong-muted', muted ? '1' : '0'); } catch (e) { /* not kept */ }
    showMute();
  };
  if (muteButton) muteButton.addEventListener('click', toggleMute);
  showMute();
  function tock(freq) {
    if (muted) return;
    try {
      audio = audio || new AudioContext();
      const o = audio.createOscillator(), v = audio.createGain(), t = audio.currentTime;
      o.type = 'triangle';
      o.frequency.value = freq;
      v.gain.setValueAtTime(0.22, t);
      v.gain.exponentialRampToValueAtTime(0.001, t + 0.07);
      o.connect(v).connect(audio.destination);
      o.start(t);
      o.stop(t + 0.08);
    } catch (e) { /* no audio here */ }
  }

  // What zaehlwerk says, from the stream or from an answer.
  function apply(v) {
    if (!v || !v.state) return;
    const before = st ? st.phase : null;
    view = v;
    st = v.state;
    if (v.last_press && press <= v.last_press) press = v.last_press + 1;
    if (st.phase !== 'change_ends') shownEnds = st.ends.slice();
    if (before !== st.phase) {
      ball = null;
      resting = null;
      readySent = {};
      serveRallyServer = null;
      phaseSince = performance.now();
    }
    if (v.status !== 'playing') {
      ball = null;
      resting = null;
      reporter = false;
      messageUntil = 0;
      if (endButton) endButton.hidden = true;
    }
    hint();
  }

  // Presses go out one at a time, in order. A press whose answer is lost is sent
  // again with the same number, and the service takes it once.
  function send(kind, half) {
    if (!reporter) return Promise.resolve();
    queue = queue.then(() => post(kind, half));
    return queue;
  }
  async function post(kind, half) {
    for (let attempt = 1; reporter; attempt++) {
      const n = press;
      let res;
      try {
        res = await fetch('/games/' + id + '/presses', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-Pong-Token': token },
          body: JSON.stringify({ rally: n, kind: kind, half: half }),
        });
      } catch (e) {
        await sleep(Math.min(500 * attempt, 3000));
        continue;
      }
      let body = null;
      try { body = await res.json(); } catch (e) { /* no body */ }
      if (res.ok) {
        press = n + 1;
        apply(body);
        return;
      }
      if (res.status === 403) {
        reporter = false;
        say('Gemeldet wird nur aus dem Tab, der das Spiel gestartet hat.', 4000);
        return;
      }
      if (res.status === 409) {
        // Behind zaehlwerk: take its state and go on from there.
        apply(body);
        return;
      }
      await sleep(1000);
    }
  }

  // A shot from half h: one bounce on the other half, or one on each for a
  // serve. Where it bounces comes from where it met the paddle and how fast
  // the rally has become; whether it clears the net is drawn, more often wrong
  // off the edge of the paddle. That is where rallies end, for people and
  // bots alike.
  function shot(h, y, speed, off, paddleVy, serve) {
    const dir = h === 0 ? 1 : -1;
    const pace = speed / START_SPEED;
    const depth = 0.55 + (Math.random() - 0.5) * 0.3 * pace + Math.abs(off) * (Math.random() < 0.5 ? 0.42 : -0.3);
    const bounces = [];
    if (serve) bounces.push(NET - dir * 0.55 * HALF);
    bounces.push(NET + dir * depth * HALF);
    let net = null;
    const r = Math.random();
    if (serve) net = r < 0.02 ? 'fault' : r < 0.02 + LET ? 'let' : null;
    else if (r < 0.02 + 0.1 * off * off) net = 'fault';
    ball = {
      x: PADDLE_X[h] + dir * 8, y: y, z: 14, x0: PADDLE_X[h], xEnd: PADDLE_X[1 - h] + dir * 60,
      vx: dir * speed, vy: serve ? (Math.random() - 0.5) * 60 : off * 150 + (Math.random() - 0.5) * 30,
      spin: -paddleVy * SPIN, bounces: bounces, next: 0, hitter: h, net: net, touched: false, passed: false,
    };
  }

  function serve(h) {
    if (resting !== h || awaiting || ball) return;
    resting = null;
    tock(800);
    shot(h, paddles[h].y, START_SPEED, 0, paddles[h].vy, true);
  }

  // A rally is over: report its winner's half, except while warming up.
  function end(winner, label) {
    ball = null;
    if (label) say(label);
    if (!st || (st.phase !== 'serve_rally' && st.phase !== 'playing')) return;
    awaiting = true;
    send('rally', HALVES[winner]).finally(() => { awaiting = false; });
  }

  function step(dt) {
    const b = ball;
    const px = b.x;
    b.x += b.vx * dt;
    b.y += b.vy * dt;
    b.vy += b.spin * dt;
    if ((px - NET) * (b.x - NET) <= 0) {
      if (b.net === 'fault') { b.x = NET; return end(1 - b.hitter, 'NETZ'); }
      if (b.net === 'let') b.touched = true;
    }
    while (b.next < b.bounces.length && (b.x - b.bounces[b.next]) * Math.sign(b.vx) >= 0) {
      const bx = b.bounces[b.next++];
      tock(1100);
      if (b.y < T.y || b.y > T.y + T.h || bx < T.x || bx > T.x + T.w) return end(1 - b.hitter, 'AUS');
      if (b.next === b.bounces.length && b.touched) {
        ball = null;
        resting = b.hitter;
        restingSince = performance.now();
        return say('NETZ – Aufschlag wiederholen');
      }
    }
    b.z = height(b);
    const r = 1 - b.hitter;
    if (!b.passed && b.next === b.bounces.length && (b.x - PADDLE_X[r]) * Math.sign(b.vx) >= 0) {
      const p = paddles[r];
      if (Math.abs(b.y - p.y) <= PADDLE_HALF + BALL_R) {
        tock(700);
        const off = clamp((b.y - p.y) / PADDLE_HALF, -1, 1);
        return shot(r, b.y, Math.min(MAX_SPEED, Math.abs(b.vx) * SPEEDUP), off, p.vy, false);
      }
      b.passed = true;
    }
    if (b.passed && Math.abs(b.x - PADDLE_X[r]) > 60) end(b.hitter, '');
  }

  // Height along the flight: a parabola from the paddle to each bounce and on
  // past the far paddle. Only drawn, as shadow and size.
  function height(b) {
    const xs = [b.x0].concat(b.bounces, [b.xEnd]);
    for (let i = 0; i < xs.length - 1; i++) {
      const u = (b.x - xs[i]) / (xs[i + 1] - xs[i]);
      if (u >= 0 && u <= 1) {
        const za = i === 0 ? 14 : 0, zb = i === xs.length - 2 ? 18 : 0;
        return za * (1 - u) + zb * u + 34 * 4 * u * (1 - u);
      }
    }
    return 0;
  }

  // The bot: it starts moving its reaction time after the ball turns its way,
  // no faster than its speed, to where the ball will cross plus its aim error.
  function bot(h, params, dt, now) {
    const p = paddles[h];
    let target = T.y + T.h / 2;
    if (ball && ball.hitter !== h) {
      if (!p.since) p.since = now;
      // A faster ball is harder to meet in the middle.
      if (p.aim === null) p.aim = (Math.random() * 2 - 1) * params.aim * PADDLE_HALF * Math.abs(ball.vx) / START_SPEED;
      if (now - p.since < params.reaction_ms) target = p.y;
      else {
        const t = Math.max(0, (PADDLE_X[h] - ball.x) / ball.vx);
        target = ball.y + ball.vy * t + 0.5 * ball.spin * t * t + p.aim;
      }
    } else {
      p.since = 0;
      p.aim = null;
    }
    move(p, target, params.speed * T.h, dt);
  }

  function move(p, target, max, dt) {
    const v = clamp((target - p.y) / Math.max(dt, 0.001), -max, max);
    p.vy = v;
    p.y = clamp(p.y + v * dt, T.y - 60, T.y + T.h + 60);
  }

  // Keys and touch. Two people share one keyboard: the left half moves with
  // W/S and serves or confirms with D, the right with ↑/↓ and ←. Against the
  // computer either set works.
  const held = new Set();
  const MOVE = [{ w: -1, W: -1, s: 1, S: 1 }, { ArrowUp: -1, ArrowDown: 1 }];
  const ACTION = [{ d: 1, D: 1 }, { ArrowLeft: 1 }];
  function controlled(h) { return mode === 'pvp' || (mode === 'pvc' && shownEnds[h] === human()); }
  addEventListener('keydown', (e) => {
    if (e.target.closest && e.target.closest('input, select, textarea')) return;
    if (['ArrowUp', 'ArrowDown', 'ArrowLeft', ' '].includes(e.key)) e.preventDefault();
    if (e.repeat) return;
    held.add(e.key);
    if (e.key === 'm' || e.key === 'M') toggleMute();
    for (const h of [0, 1]) {
      const any = mode === 'pvc' && (ACTION[0][e.key] || ACTION[1][e.key] || e.key === ' ' || e.key === 'Enter');
      if ((mode === 'pvp' && ACTION[h][e.key]) || (any && controlled(h))) action(h);
    }
  });
  addEventListener('keyup', (e) => held.delete(e.key));

  const touches = new Map();
  const toCanvas = (e) => {
    const r = canvas.getBoundingClientRect();
    return { x: (e.clientX - r.left) * W / r.width, y: (e.clientY - r.top) * H / r.height };
  };
  const touchHalf = (x) => (mode === 'pvc' ? (controlled(0) ? 0 : 1) : x < W / 2 ? 0 : 1);
  canvas.addEventListener('pointerdown', (e) => {
    if (mode === 'cvc') return;
    const c = toCanvas(e);
    touches.set(e.pointerId, { h: touchHalf(c.x), x: c.x, y: c.y, t: performance.now() });
    canvas.setPointerCapture(e.pointerId);
  });
  canvas.addEventListener('pointermove', (e) => {
    const t = touches.get(e.pointerId);
    if (t) paddles[t.h].target = toCanvas(e).y;
  });
  canvas.addEventListener('pointerup', (e) => {
    const t = touches.get(e.pointerId);
    touches.delete(e.pointerId);
    if (!t) return;
    const c = toCanvas(e);
    // A tap, not a drag, is the key: serve or ready.
    if (performance.now() - t.t < 250 && Math.hypot(c.x - t.x, c.y - t.y) < 14) action(t.h);
  });

  // The key for half h: ready in the warm-up and a change of ends, otherwise
  // the serve when that side holds the ball.
  function action(h) {
    if (!st || !reporter) return;
    if (st.phase === 'warmup' || st.phase === 'change_ends') {
      // Ready is pressed at the half the player is walking to, which is where
      // zaehlwerk already has them.
      const half = mode === 'pvc' ? halfOf(human(), st.ends) : h;
      return ready(half);
    }
    if (resting === h && !botAt(h)) serve(h);
  }
  function ready(half) {
    if (readySent[half] || !(st.waiting || []).includes(HALVES[half])) return;
    readySent[half] = true;
    send('ready', HALVES[half]);
  }

  function update(dt, now) {
    for (const h of [0, 1]) {
      const p = paddles[h];
      const params = botAt(h);
      if (params && reporter) bot(h, params, dt, now);
      else if (controlled(h)) {
        let dir = 0;
        for (const set of mode === 'pvc' ? MOVE : [MOVE[h]]) for (const k in set) if (held.has(k)) dir += set[k];
        if (dir) { p.target = null; move(p, p.y + Math.sign(dir) * 1000, HUMAN_SPEED, dt); }
        else if (p.target !== null) move(p, p.target, HUMAN_SPEED * 1.6, dt);
        else p.vy = 0;
      }
    }
    if (!st || !reporter || view.status !== 'playing') return;

    const phase = st.phase;
    if (phase === 'warmup' || phase === 'change_ends') {
      // The bots confirm a moment after the phase starts, at their new half.
      for (const p of ['a', 'b']) {
        if (bots[idx(p)] && now - phaseSince > 1200) ready(halfOf(p, st.ends));
      }
    }
    if (phase === 'warmup') {
      // Free play: nothing scores, the ball comes back from alternate sides.
      if (!ball && resting === null) { resting = warmServer; warmServer ^= 1; restingSince = now; }
      if (resting !== null && now - restingSince > 900) serve(resting);
    } else if (phase === 'serve_rally' || phase === 'playing') {
      if (!ball && resting === null && !awaiting) {
        if (phase === 'playing') resting = halfOf(st.serving, shownEnds);
        else resting = serveRallyServer === null ? (serveRallyServer = Math.random() < 0.5 ? 0 : 1) : serveRallyServer;
        restingSince = now;
      }
      if (resting !== null && botAt(resting) && now - restingSince > 700) serve(resting);
    }
    if (ball) step(dt);
  }

  function draw(now) {
    g.fillStyle = '#0b1220';
    g.fillRect(0, 0, W, H);
    g.fillStyle = '#1d6b74';
    g.fillRect(T.x, T.y, T.w, T.h);
    g.strokeStyle = '#f4f4f8';
    g.lineWidth = 4;
    g.strokeRect(T.x, T.y, T.w, T.h);
    g.lineWidth = 1.5;
    g.beginPath(); g.moveTo(T.x, T.y + T.h / 2); g.lineTo(T.x + T.w, T.y + T.h / 2); g.stroke();
    g.strokeStyle = '#dcdce8';
    g.lineWidth = 5;
    g.beginPath(); g.moveTo(NET, T.y - 16); g.lineTo(NET, T.y + T.h + 16); g.stroke();
    g.fillStyle = '#9a9ab8';
    g.fillRect(NET - 4, T.y - 22, 8, 8);
    g.fillRect(NET - 4, T.y + T.h + 14, 8, 8);

    g.font = '600 20px "Space Grotesk", system-ui, sans-serif';
    g.textAlign = 'center';
    for (const h of [0, 1]) {
      const p = paddles[h];
      g.fillStyle = h === 0 ? '#d4334a' : '#1b1b22';
      g.strokeStyle = '#d4334a';
      g.lineWidth = 2;
      g.fillRect(PADDLE_X[h] - 6, p.y - PADDLE_HALF, 12, PADDLE_HALF * 2);
      g.strokeRect(PADDLE_X[h] - 6, p.y - PADDLE_HALF, 12, PADDLE_HALF * 2);
      if (st) {
        const cx = h === 0 ? T.x + HALF / 2 : NET + HALF / 2;
        g.fillStyle = '#ececff';
        const serving = st.phase === 'playing' && st.serving === shownEnds[h] ? '● ' : '';
        g.fillText(serving + st.players[idx(shownEnds[h])], cx, T.y - 32);
        if ((st.waiting || []).includes(HALVES[h])) {
          g.fillStyle = '#ffb547';
          g.font = '14px "Space Grotesk", system-ui, sans-serif';
          g.fillText(st.phase === 'warmup' ? 'Bereit? Taste drücken' : 'Hier ankommen und Taste drücken', cx, T.y + T.h + 44);
          g.font = '600 20px "Space Grotesk", system-ui, sans-serif';
        }
      }
    }

    let bx = null, by = null, bz = 0;
    if (ball) { bx = ball.x; by = ball.y; bz = ball.z; }
    else if (resting !== null) { bx = PADDLE_X[resting] + (resting === 0 ? 16 : -16); by = paddles[resting].y; bz = 10; }
    if (bx !== null) {
      g.fillStyle = 'rgba(0, 0, 0, 0.35)';
      g.beginPath(); g.ellipse(bx + bz * 0.3, by + bz * 0.5, BALL_R, BALL_R * 0.7, 0, 0, Math.PI * 2); g.fill();
      g.fillStyle = '#ffe2b0';
      g.beginPath(); g.arc(bx, by - bz * 0.8, BALL_R * (1 + bz / 70), 0, Math.PI * 2); g.fill();
    }

    g.fillStyle = '#ececff';
    g.font = '700 30px "Space Grotesk", system-ui, sans-serif';
    let centre = '';
    if (view && view.status !== 'playing') centre = view.status === 'won' && st.winner ? 'SIEG ' + st.players[idx(st.winner)] : 'SPIEL BEENDET';
    else if (now < messageUntil) centre = message;
    else if (!reporter && view) centre = 'Zuschauen';
    else if (st && st.phase === 'change_ends') centre = 'SEITENWECHSEL';
    if (centre) g.fillText(centre, W / 2, H / 2 + 10);
  }

  function hint() {
    const el = document.getElementById('hint');
    if (!el) return;
    if (!reporter) {
      el.textContent = view && view.status === 'playing'
        ? 'Zuschauen: gespielt und gemeldet wird im Tab, der das Spiel gestartet hat.' : '';
      return;
    }
    el.textContent = {
      pvp: 'Links: W/S bewegen, D Aufschlag/Bereit · Rechts: ↑/↓ bewegen, ← Aufschlag/Bereit · Touch: ziehen bewegt, tippen schlägt auf · M Ton',
      pvc: 'W/S oder ↑/↓ bewegen, Leertaste Aufschlag/Bereit · Touch: ziehen bewegt, tippen schlägt auf · M Ton',
      cvc: 'Die Computer spielen. M Ton',
    }[mode] || '';
  }

  if (endButton && reporter) {
    endButton.hidden = false;
    endButton.addEventListener('click', async () => {
      await fetch('/games/' + id + '/end', { method: 'POST', headers: { 'X-Pong-Token': token } }).catch(() => {});
      location.href = '/';
    });
  }

  const events = new EventSource('/games/' + id + '/events');
  events.onmessage = (e) => apply(JSON.parse(e.data));

  let last = performance.now();
  function frame(now) {
    const dt = Math.min(0.033, (now - last) / 1000);
    last = now;
    update(dt, now);
    draw(now);
    requestAnimationFrame(frame);
  }
  hint();
  requestAnimationFrame(frame);
})();
