"use strict";
// Night grayscale detection (display-only). Some USB cameras with IR LEDs stay
// in colour mode at night, so the image has one uniform tint (purple/magenta,
// sometimes red) or is already B&W. The viewer then shows the video in
// grayscale. Pure functions only; app.js does the canvas sampling and the CSS.
// Unit tests: web/test/nightgray.test.mjs (node --test web/test).
//
// All thresholds below are initial guesses to be tuned with real nest-box
// footage: open the page with ?debugNight=1 to see the live numbers.
const NightGray = (() => {
  // Frame sampled for classification (drawImage scales the video down to this).
  const SAMPLE_W = 64, SAMPLE_H = 48;
  const SAMPLE_INTERVAL_MS = 2000;

  // Pixels with BT.601 luma outside [LUMA_MIN, LUMA_MAX] carry no reliable
  // chroma (sensor noise in the black, clipping in the white) and are ignored.
  const LUMA_MIN = 16, LUMA_MAX = 232;
  // A frame whose mean luma (all pixels) is below DARK_LUMA is "dark" and
  // counts as night (a dark nest box; grayscale costs nothing there). Else,
  // if fewer than MIN_VALID_FRACTION of the pixels are valid, the frame is
  // "unknown" (e.g. clipped white) and does not change the state.
  const MIN_VALID_FRACTION = 0.10;
  const DARK_LUMA = 20;

  // spread = sqrt(var(cb) + var(cr)) over valid pixels, where cb/cr are the
  // YCbCr chroma divided by luma, in percent (so a fixed tint scales out). A
  // single tint of any direction plus sensor noise stays low (synthetic ~2, real maybe 3-8;
  // ~0-3 for B&W); a daylight scene with green foliage, brown wood, blue sky
  // etc. is typically well above 16 (synthetic ~40). meanCb/meanCr (same units) show the tint.
  // Two thresholds (hysteresis): IR-like below ENTER, day-like above EXIT,
  // in between = ambiguous (state is kept).
  const ENTER_SPREAD = 8;
  const EXIT_SPREAD = 12;

  // tint = |(meanCb, meanCr)|, same units. An IR image from a camera that
  // stays in colour mode is uniformly purple/red (faint purple ~8, strong ~25),
  // a grey/white scene in daylight is neutral (~0-1). So a uniform image only
  // counts as IR when it is clearly tinted (>= ENTER_TINT); a neutral uniform
  // image (< EXIT_TINT) counts as day. Cameras that switch to B&W themselves
  // are neutral at night too, which is fine: grey stays grey.
  const ENTER_TINT = 5;
  const EXIT_TINT = 3.5;

  // Consecutive agreeing samples needed to switch (3 samples ~ 6 s).
  const SWITCH_SAMPLES = 3;

  // classifyFrame(rgba) -> {spread, meanCb, meanCr, valid, ir, day, unknown}
  // rgba: Uint8ClampedArray/Uint8Array of RGBA pixels (ImageData.data).
  function classifyFrame(rgba) {
    const total = rgba.length >> 2;
    let n = 0, sCb = 0, sCr = 0, sCb2 = 0, sCr2 = 0, sY = 0, sA = 0;
    for (let i = 0; i < rgba.length; i += 4) {
      const r = rgba[i], g = rgba[i + 1], b = rgba[i + 2];
      const y = 0.299 * r + 0.587 * g + 0.114 * b;
      sY += y;
      sA += rgba[i + 3];
      if (y < LUMA_MIN || y > LUMA_MAX) continue;
      // Chroma relative to luma (percent): a fixed colour cast scales with
      // brightness, so absolute Cb/Cr would spread even in a uniformly tinted image.
      const cb = (-0.168736 * r - 0.331264 * g + 0.5 * b) / y * 100;
      const cr = (0.5 * r - 0.418688 * g - 0.081312 * b) / y * 100;
      n++; sCb += cb; sCr += cr; sCb2 += cb * cb; sCr2 += cr * cr;
    }
    const valid = total ? n / total : 0;
    const meanY = total ? sY / total : 0;
    // Fully transparent (nothing drawn) or exactly black (an undecoded/broken
    // frame; a real dark scene still has sensor noise): unknown.
    if (total && (sA === 0 || meanY < 0.5)) {
      return { spread: NaN, meanCb: NaN, meanCr: NaN, valid: 0, meanY, dark: false, ir: false, day: false, unknown: true };
    }
    // A dark frame is night, whatever the (noisy) chroma of its few valid pixels says.
    if (total && meanY < DARK_LUMA) {
      return { spread: NaN, meanCb: NaN, meanCr: NaN, valid, meanY, dark: true, ir: true, day: false, unknown: false };
    }
    if (!total || valid < MIN_VALID_FRACTION || n === 0) {
      return { spread: NaN, meanCb: NaN, meanCr: NaN, valid, meanY, dark: false, ir: false, day: false, unknown: true };
    }
    const meanCb = sCb / n, meanCr = sCr / n;
    const spread = Math.sqrt(Math.max(0, sCb2 / n - meanCb * meanCb) + Math.max(0, sCr2 / n - meanCr * meanCr));
    const tint = Math.hypot(meanCb, meanCr);
    const ir = spread < ENTER_SPREAD && tint >= ENTER_TINT;
    const day = spread > EXIT_SPREAD || tint < EXIT_TINT;
    return { spread, meanCb, meanCr, tint, valid, meanY, dark: false, ir, day, unknown: false };
  }

  // Debounce/hysteresis. state = {gray: bool, run: int}; returns the new state
  // (pure). Unknown and ambiguous samples reset the run without changing gray.
  function step(state, result) {
    const want = result.unknown ? null : result.ir ? true : result.day ? false : null;
    if (want === null || want === state.gray) return { gray: state.gray, run: 0 };
    const run = state.run + 1;
    return run >= SWITCH_SAMPLES ? { gray: want, run: 0 } : { gray: state.gray, run };
  }

  return { SAMPLE_W, SAMPLE_H, SAMPLE_INTERVAL_MS, LUMA_MIN, LUMA_MAX, MIN_VALID_FRACTION,
    DARK_LUMA, ENTER_SPREAD, EXIT_SPREAD, ENTER_TINT, EXIT_TINT, SWITCH_SAMPLES, classifyFrame, step };
})();
if (typeof module !== "undefined") module.exports = NightGray;
