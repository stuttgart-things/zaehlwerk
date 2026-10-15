// zaehlwerk-pong: the diagonal serve, as in doubles, for a game started with
// it. A file of its own because game.js is at the size ADR-0009 sets for it;
// this is geometry on the table game.js draws, and game.js asks it four
// things: where the server stands, how the serve flies, whether a bounce is
// in, and where a bot waits to serve.
//
// The rule (ITTF 2.6.3 for doubles): the ball bounces first in the server's
// right half court and then in the receiver's right half court, so it crosses
// the centre line. The table is drawn from above with player a's end on the
// left, so the left end's right half is the lower one and the right end's the
// upper one, and every legal serve goes corner to corner across the screen.
window.pongServe = function (T) {
  'use strict';
  const MID = T.y + T.h / 2;
  // right is whether y lies in the right half court of the end h.
  const right = (h, y) => (h === 0 ? y > MID : y < MID);
  // A bounce on the centre line is in for both halves, as a ball on the line
  // is in for the edge; only a clear miss is a fault.
  const LINE = 2;
  const inBox = (h, y) => right(h, y) || Math.abs(y - MID) <= LINE;
  // How far into the box a serve is aimed, from the centre line, as a share of
  // a half court's width, and how much a serve scatters around it.
  const AIM = 0.5, SCATTER = 0.3;
  // How much a paddle moving at the serve throws the ball off its line: a
  // serve played still lands where it is aimed, one played on the run may not.
  const SLIP = 0.12;
  // The share of serves that miss anyway, for bots too.
  const MISS = 0.03;

  return {
    // from says whether the server at end h may serve from y: in its right
    // half court.
    from: (h, y) => right(h, y),
    // stance is where a bot holds the ball before it serves: well inside its
    // right half court, so the first bounce stays on its side.
    stance: (h) => MID + (h === 0 ? 1 : -1) * 0.34 * T.h,
    // vy is the side speed that takes a serve from (x0, y0) over its first
    // bounce at x1 into the receiver's right half court at x2, flying at vx
    // with the given spin. How deep across it is aimed depends on how far
    // from the centre line the server stands: from the line itself the serve
    // has to stay close to it on the other side too, or its first bounce
    // would cross over before the net.
    vy: (h, x0, y0, x1, x2, vx, spin, paddleVy) => {
      const quarter = T.h / 4;
      const side = h === 0 ? -1 : 1; // the receiver's right half is up for a, down for b
      const f = (x1 - x0) / (x2 - x0);
      const room = 0.8 * (1 - f) / f * Math.abs(y0 - MID);
      const depth = Math.min(quarter * AIM * 2, room);
      let y = MID + side * depth * (1 + (Math.random() - 0.5) * 2 * SCATTER);
      y += paddleVy * SLIP;
      if (Math.random() < MISS) y -= side * quarter * (1 + Math.random());
      const t = Math.abs(x2 - x0) / Math.abs(vx);
      return (y - y0 - 0.5 * spin * t * t) / t;
    },
    // fair says whether the bounce number i of a serve from end h is in:
    // the first in the server's right half court, the second in the
    // receiver's.
    fair: (h, i, y) => inBox(i === 0 ? h : 1 - h, y),
  };
};
