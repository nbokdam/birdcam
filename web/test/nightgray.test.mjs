import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
const NG = createRequire(import.meta.url)("../nightgray.js");

let seed = 1;
const rnd = () => ((seed = (seed * 1664525 + 1013904223) >>> 0) / 4294967296);
const clamp = (v) => Math.max(0, Math.min(255, Math.round(v)));
// Build a 64x48 frame; fn(i) -> [r,g,b].
function frame(fn) {
  const d = new Uint8ClampedArray(NG.SAMPLE_W * NG.SAMPLE_H * 4);
  for (let i = 0; i < d.length / 4; i++) {
    const [r, g, b] = fn(i);
    d.set([clamp(r), clamp(g), clamp(b), 255], i * 4);
  }
  return d;
}
// Tinted IR image: luma gradient+noise, channels scaled by a fixed tint.
const tinted = (tint, sat = 1) => frame((i) => {
  const l = 60 + 120 * ((i % 64) / 64) + (rnd() - 0.5) * 30;
  return tint.map((t) => l * (1 + (t - 1) * sat) + (rnd() - 0.5) * 6);
});

test("purple IR tint is ir", () => {
  const r = NG.classifyFrame(tinted([1.25, 0.8, 1.3]));
  assert.ok(r.ir && !r.day, JSON.stringify(r));
});
test("reddish IR tint is ir", () => {
  const r = NG.classifyFrame(tinted([1.4, 0.8, 0.8]));
  assert.ok(r.ir, JSON.stringify(r));
});
test("neutral grey scene (grey wall in daylight, or a camera already in B&W) is day", () => {
  const r = NG.classifyFrame(frame((i) => { const l = 40 + (i % 64) * 3; return [l, l, l]; }));
  assert.ok(r.day && !r.ir && r.spread < 1, JSON.stringify(r));
  // slightly noisy, faintly bluish grey like the real C1-lookalike daylight frame
  const w = NG.classifyFrame(frame((i) => { const l = 60 + (i % 9); return [l, l + 1, l + 2]; }));
  assert.ok(w.day && !w.ir, JSON.stringify(w));
});
test("faint purple IR tint is ir", () => {
  const r = NG.classifyFrame(frame((i) => { const l = (i % 11) - 5; return [130 + l, 115 + l, 135 + l]; }));
  assert.ok(r.ir && r.tint >= NG.ENTER_TINT, JSON.stringify(r));
});
test("colourful scene is day", () => {
  const regions = [[40, 160, 50], [150, 100, 60], [90, 140, 220], [200, 190, 70], [120, 120, 120]];
  const r = NG.classifyFrame(frame((i) => {
    const c = regions[Math.floor((i % 64) / 13) % regions.length];
    const k = 0.7 + rnd() * 0.5;
    return c.map((v) => v * k);
  }));
  assert.ok(r.day && !r.ir, JSON.stringify(r));
});
test("random hues is day", () => {
  const r = NG.classifyFrame(frame(() => [rnd() * 255, rnd() * 255, rnd() * 255]));
  assert.ok(r.day, JSON.stringify(r));
});
test("mostly black frame (dark nest box) counts as night", () => {
  const r = NG.classifyFrame(frame((i) => (i % 20 === 0 ? [120, 60, 130] : [3, 2, 4])));
  assert.ok(r.dark && r.ir && !r.day && !r.unknown, JSON.stringify(r));
  // dark reddish frame like the real C1-lookalike in the dark
  const red = NG.classifyFrame(frame((i) => (i % 7 === 0 ? [40, 6, 6] : [9, 2, 3])));
  assert.ok(red.dark && red.ir, JSON.stringify(red));
  assert.equal(NG.classifyFrame(new Uint8ClampedArray(0)).unknown, true);
});
test("transparent (undrawn) frame is unknown, not dark", () => {
  const r = NG.classifyFrame(new Uint8ClampedArray(64 * 48 * 4));
  assert.ok(r.unknown && !r.dark && !r.ir, JSON.stringify(r));
  const black = NG.classifyFrame(frame(() => [0, 0, 0]));
  assert.ok(black.unknown && !black.dark, JSON.stringify(black));
});
test("clipped white frame is unknown and keeps the state", () => {
  const r = NG.classifyFrame(frame(() => [250, 250, 250]));
  assert.ok(r.unknown && !r.ir && !r.day, JSON.stringify(r));
  const s = { gray: true, run: 2 };
  assert.deepEqual(NG.step(s, r), { gray: true, run: 0 });
});
test("step: needs N consecutive samples, both directions, ambiguous resets", () => {
  const ir = { ir: true, day: false, unknown: false }, day = { ir: false, day: true, unknown: false };
  const mid = { ir: false, day: false, unknown: false };
  let s = { gray: false, run: 0 };
  s = NG.step(s, ir); s = NG.step(s, ir); assert.equal(s.gray, false);
  s = NG.step(s, mid); s = NG.step(s, ir); s = NG.step(s, ir); assert.equal(s.gray, false);
  s = NG.step(s, ir); assert.equal(s.gray, true);
  s = NG.step(s, day); s = NG.step(s, day); s = NG.step(s, ir); assert.equal(s.gray, true);
  for (let i = 0; i < 3; i++) s = NG.step(s, day);
  assert.equal(s.gray, false);
});
