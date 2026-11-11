"use strict";
// BirdCam viewer: camera picker + WHEP (WebRTC) playback with auto-reconnect,
// fit-to-window with rotation, mute, fullscreen, local recording, login/logout.

const $ = (id) => document.getElementById(id);
const video = $("video"), stage = $("stage"), box = $("box");
const overlay = $("overlay"), overlayText = $("overlay-text"), overlayLogin = $("overlay-login");
const muteBtn = $("mute"), hintBtn = $("unmute-hint"), recBtn = $("record"), fsBtn = $("fullscreen");

const store = {
  get(k) { try { return localStorage.getItem(k); } catch (e) { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch (e) {} },
};

let session = { auth_enabled: false, user: null, default_device_id: "" };
let cameras = [];
let sel = null;          // {device, camera}
let pc = null, viewerURL = null, connGen = 0, reconnectTimer = null;
let waitingForCamera = false;
let errorText = "", errorAuth = false;
let hasVideo = false, lastTime = -1, stallTicks = 0;
let rotation = 0;
let mutePref = store.get("birdcam.muted"); // "1" muted, "0" sound, null = never chosen

const keyOf = (c) => c.device_id + "/" + c.camera_id;
const selKey = () => (sel ? sel.device + "/" + sel.camera : "");
const current = () => cameras.find((c) => keyOf(c) === selKey());
const normRot = (r) => ((Math.round((+r || 0) / 90) * 90) % 360 + 360) % 360;

// ---------- session / auth ----------

async function loadSession() {
  try {
    const res = await fetch("api/session", { cache: "no-store", headers: { Accept: "application/json" } });
    if (res.ok) session = Object.assign(session, await res.json());
  } catch (e) { /* no session endpoint: auth disabled */ }
  renderAuth();
}

function login() {
  location.href = (session.login_url || "/auth/login") + "?return=" +
    encodeURIComponent(location.pathname + location.search);
}

async function logout() {
  const url = session.logout_url || "/auth/logout";
  try {
    const res = await fetch(url, { method: "POST", redirect: "manual", headers: { Accept: "application/json" } });
    if (res.type === "opaqueredirect") throw new Error("redirect"); // let the browser follow it
    let to = "/";
    try { to = (await res.json()).redirect || "/"; } catch (e) {}
    location.href = to;
  } catch (e) {
    const f = document.createElement("form");
    f.method = "POST";
    f.action = url;
    document.body.append(f);
    f.submit();
  }
}

function renderAuth() {
  const el = $("auth");
  el.replaceChildren();
  if (!session.auth_enabled) return;
  const b = document.createElement("button");
  b.type = "button";
  if (session.user) {
    const n = document.createElement("span");
    n.className = "user";
    n.textContent = session.user.name || session.user.username || session.user.email || "";
    n.title = n.textContent;
    b.textContent = "Log out";
    b.onclick = logout;
    el.append(n, b);
  } else {
    b.textContent = "Log in";
    b.onclick = login;
    el.append(b);
  }
}
overlayLogin.onclick = login;

// ---------- camera list / picker ----------

async function poll() {
  try {
    const res = await fetch("api/cameras", { cache: "no-store" });
    if (res.ok) cameras = await res.json();
  } catch (e) { /* server unreachable; keep last list */ }
  if (!sel && cameras.length) select(initialSelection());
  const c = current();
  if (c) {
    const r = normRot(c.rotation);
    if (r !== rotation) { rotation = r; fit(); }
    if (waitingForCamera) restart(0);
  }
  renderNav();
  updateStatus();
}

function initialSelection() {
  const q = new URLSearchParams(location.search);
  const qd = q.get("device"), qc = q.get("camera");
  if (qd && qc) return { device: qd, camera: qc }; // explicit; may not be visible (yet)
  const asSel = (c) => c && { device: c.device_id, camera: c.camera_id };
  const ofDev = (d) => d && cameras.find((c) => c.device_id === d);
  const saved = store.get("birdcam.camera");
  return asSel(ofDev(qd)) || asSel(ofDev(session.default_device_id)) ||
    asSel(cameras.find((c) => keyOf(c) === saved)) ||
    asSel(cameras.find((c) => c.online) || cameras[0]);
}

const lockIcon = () => {
  const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  s.setAttribute("class", "lock");
  s.setAttribute("aria-label", "private");
  s.setAttribute("role", "img");
  const u = document.createElementNS("http://www.w3.org/2000/svg", "use");
  u.setAttribute("href", "#i-lock");
  s.append(u);
  return s;
};

let navSig = "";
function renderNav() {
  const nav = $("cameras");
  const sig = JSON.stringify(cameras.map((c) => [keyOf(c), c.name, c.device_name, c.online, c.public])) + selKey();
  if (sig === navSig) return;
  navSig = sig;
  nav.replaceChildren();
  const only = cameras.length === 1 && current();
  nav.hidden = !!only;
  if (only) return;
  if (!cameras.length) {
    const p = document.createElement("span");
    p.className = "empty";
    p.textContent = "No cameras yet";
    nav.append(p);
    return;
  }
  const groups = new Map();
  for (const c of cameras) {
    if (!groups.has(c.device_id)) groups.set(c.device_id, { name: c.device_name || c.device_id, cams: [] });
    groups.get(c.device_id).cams.push(c);
  }
  for (const g of groups.values()) {
    const div = document.createElement("div");
    div.className = "group";
    const single = g.cams.length === 1;
    if (!single) {
      const n = document.createElement("span");
      n.className = "group-name";
      n.textContent = g.name;
      div.append(n);
      div.setAttribute("role", "group");
      div.setAttribute("aria-label", g.name);
    }
    for (const c of g.cams) {
      const b = document.createElement("button");
      b.type = "button";
      if (keyOf(c) === selKey()) b.setAttribute("aria-current", "true");
      const dot = document.createElement("span");
      dot.className = "dot" + (c.online ? " on" : "");
      const label = single ? g.name : (c.name || c.camera_id);
      b.append(dot, document.createTextNode(label));
      if (c.public === false) b.append(lockIcon());
      b.title = (single ? g.name + " · " + (c.name || c.camera_id) : label) +
        (c.online ? "" : " (offline)") + (c.public === false ? " · private" : "");
      b.onclick = () => select({ device: c.device_id, camera: c.camera_id });
      div.append(b);
    }
    nav.append(div);
  }
}

function select(s) {
  if (!s || (sel && s.device === sel.device && s.camera === sel.camera)) return;
  sel = s;
  errorText = "";
  errorAuth = false;
  const c = current();
  rotation = c ? normRot(c.rotation) : 0;
  if (c) store.set("birdcam.camera", keyOf(c));
  const u = new URL(location.href);
  u.searchParams.set("device", s.device);
  u.searchParams.set("camera", s.camera);
  u.hash = "";
  history.replaceState(null, "", u);
  navSig = "";
  renderNav();
  restart(0);
  fit();
}

// ---------- status ----------

function setHasVideo(v) {
  if (hasVideo !== v) { hasVideo = v; updateStatus(); }
}

function updateStatus() {
  const c = current();
  let text = "", isError = false, showLogin = false;
  if (!c) {
    if (sel) {
      isError = true;
      if (session.auth_enabled && !session.user) { text = "Log in to watch this camera"; showLogin = true; }
      else text = "Camera not found";
    }
  } else if (errorText) {
    text = errorText; isError = true;
    showLogin = errorAuth && session.auth_enabled && !session.user;
  } else if (!c.online) text = "Camera offline – reconnecting…";
  else if (!hasVideo) text = c.streaming ? "Connecting…" : "Waking camera…";
  overlay.hidden = !text;
  overlay.classList.toggle("error", isError);
  overlayText.textContent = text;
  overlayLogin.hidden = !showLogin;
  updateControls();

  const parts = [];
  if (c) {
    parts.push((c.device_name || c.device_id) + " · " + (c.name || c.camera_id));
    if (c.stats && c.streaming) parts.push(Math.round(c.stats.kbps) + " kbps");
    if (c.mode) parts.push(c.mode);
    if (typeof c.viewers === "number") parts.push(c.viewers + (c.viewers === 1 ? " viewer" : " viewers"));
    document.title = (c.name || c.camera_id) + " – BirdCam";
  }
  $("info").textContent = parts.join("  ·  ");
}

// ---------- fit to window (with rotation) ----------

function fit() {
  const W = stage.clientWidth, H = stage.clientHeight;
  const vw = video.videoWidth || 16, vh = video.videoHeight || 9;
  const side = rotation % 180 !== 0;
  const ew = side ? vh : vw, eh = side ? vw : vh; // effective (displayed) size
  const s = Math.max(0, Math.min(W / ew, H / eh));
  box.style.width = Math.floor(ew * s) + "px";
  box.style.height = Math.floor(eh * s) + "px";
  video.style.width = vw * s + "px";
  video.style.height = vh * s + "px";
  video.style.transform = "translate(-50%, -50%) rotate(" + rotation + "deg)";
}
new ResizeObserver(fit).observe(stage);
window.addEventListener("resize", fit);
window.addEventListener("orientationchange", fit);
video.addEventListener("loadedmetadata", fit);
video.addEventListener("resize", fit);

// ---------- connection ----------

function teardown() {
  connGen++;
  clearTimeout(reconnectTimer);
  stopRecording();
  if (pc) { pc.onconnectionstatechange = null; pc.ontrack = null; pc.close(); pc = null; }
  if (viewerURL) { fetch(viewerURL, { method: "DELETE", keepalive: true }).catch(() => {}); viewerURL = null; }
  video.srcObject = null;
  setHasVideo(false);
}

function restart(delay) {
  teardown();
  waitingForCamera = false;
  const gen = connGen;
  reconnectTimer = setTimeout(() => { if (gen === connGen) connect(gen); }, delay);
}

function waitIce(p, ms) {
  if (p.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    p.addEventListener("icegatheringstatechange", () => {
      if (p.iceGatheringState === "complete") { clearTimeout(t); resolve(); }
    });
  });
}

async function errorMessage(res) {
  const body = (await res.text().catch(() => "")).trim();
  try { const j = JSON.parse(body); if (j && j.error) return j.error; } catch (e) {}
  return body || "HTTP " + res.status;
}

async function connect(gen) {
  const c = current();
  if (!c) { waitingForCamera = true; updateStatus(); return; }
  const p = new RTCPeerConnection();
  pc = p;
  p.addTransceiver("video", { direction: "recvonly" });
  p.addTransceiver("audio", { direction: "recvonly" });
  const stream = new MediaStream();
  p.ontrack = (e) => {
    stream.addTrack(e.track);
    e.track.addEventListener("ended", () => { if (gen === connGen) stopRecording(); });
    if (video.srcObject !== stream) video.srcObject = stream;
    startPlayback();
  };
  let discTimer = null;
  p.onconnectionstatechange = () => {
    if (gen !== connGen) return;
    const st = p.connectionState;
    if (st === "failed" || st === "closed") restart(2000);
    else if (st === "disconnected") discTimer = setTimeout(() => { if (gen === connGen) restart(0); }, 5000);
    else if (st === "connected") clearTimeout(discTimer);
  };
  try {
    await p.setLocalDescription(await p.createOffer());
    await waitIce(p, 2000);
    const url = "api/cameras/" + encodeURIComponent(c.device_id) + "/" + encodeURIComponent(c.camera_id) + "/whep";
    const res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/sdp" }, body: p.localDescription.sdp });
    if (gen !== connGen) return;
    if (!res.ok) {
      const err = new Error(await errorMessage(res));
      err.status = res.status;
      throw err;
    }
    const loc = res.headers.get("Location");
    if (loc) viewerURL = new URL(loc, location.href).href;
    const answer = await res.text();
    if (gen !== connGen) return;
    await p.setRemoteDescription({ type: "answer", sdp: answer });
    errorText = "";
    errorAuth = false;
    updateStatus();
  } catch (e) {
    if (gen !== connGen) return;
    if (e.status === 401) {
      errorAuth = true;
      errorText = "Log in to watch this camera";
      teardown();
      // No point retrying while logged out; retry slowly when auth is off/unknown.
      if (!(session.auth_enabled && !session.user)) { const g = connGen; reconnectTimer = setTimeout(() => { if (g === connGen) restart(0); }, 15000); }
    } else {
      errorAuth = false;
      errorText = (e.status === 404 ? "Camera not available" : "Cannot watch: " + e.message) + " — retrying…";
      restart(5000);
    }
    updateStatus();
  }
}

// Detect whether frames are actually arriving.
video.addEventListener("playing", () => setHasVideo(true));
video.addEventListener("waiting", () => setHasVideo(false));
setInterval(() => {
  if (!video.srcObject) return;
  if (video.currentTime !== lastTime) { lastTime = video.currentTime; stallTicks = 0; if (video.readyState >= 2) setHasVideo(true); }
  else if (++stallTicks >= 3) setHasVideo(false);
}, 1000);

// ---------- audio ----------

let autoplayBlocked = false;
function startPlayback() {
  video.muted = mutePref !== "0" || autoplayBlocked;
  video.play().catch(() => {
    if (video.muted) return;
    autoplayBlocked = true; // sound not allowed without a gesture: fall back to muted
    video.muted = true;
    video.play().catch(() => {});
    updateControls();
  });
}

function setMuted(m) {
  video.muted = m;
  mutePref = m ? "1" : "0";
  store.set("birdcam.muted", mutePref);
  if (!m) { autoplayBlocked = false; video.play().catch(() => {}); }
  updateControls();
  poke();
}

function updateControls() {
  const c = current();
  const hasAudio = !c || c.audio !== false;
  muteBtn.classList.toggle("is-on", !video.muted);
  const label = !hasAudio ? "No audio on this camera" : video.muted ? "Unmute (m)" : "Mute (m)";
  muteBtn.setAttribute("aria-label", label);
  muteBtn.title = label;
  muteBtn.disabled = !hasAudio;
  hintBtn.hidden = !(video.muted && hasAudio && hasVideo && mutePref !== "1");
  recBtn.hidden = !canRecord;
  recBtn.disabled = !recorder && !hasVideo;
  fsBtn.classList.toggle("is-on", !!fsElement());
}

muteBtn.onclick = () => setMuted(!video.muted);
hintBtn.onclick = () => setMuted(false);
video.addEventListener("volumechange", updateControls);

// ---------- fullscreen ----------

const fsElement = () => document.fullscreenElement || document.webkitFullscreenElement;
const fsSupported = !!(stage.requestFullscreen || stage.webkitRequestFullscreen || video.webkitEnterFullscreen);
fsBtn.hidden = !fsSupported;

function toggleFullscreen() {
  if (fsElement()) {
    (document.exitFullscreen || document.webkitExitFullscreen).call(document);
    return;
  }
  const lock = () => {
    const side = rotation % 180 !== 0;
    const w = side ? video.videoHeight : video.videoWidth, h = side ? video.videoWidth : video.videoHeight;
    try { screen.orientation.lock(w >= h ? "landscape" : "portrait").catch(() => {}); } catch (e) {}
  };
  if (stage.requestFullscreen) stage.requestFullscreen().then(lock, () => {});
  else if (stage.webkitRequestFullscreen) stage.webkitRequestFullscreen();
  else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen(); // iPhone: native player
}
fsBtn.onclick = toggleFullscreen;
box.addEventListener("dblclick", (e) => { if (!e.target.closest("button")) toggleFullscreen(); });
document.addEventListener("fullscreenchange", () => { updateControls(); fit(); });
document.addEventListener("webkitfullscreenchange", () => { updateControls(); fit(); });

// ---------- controls auto-hide (desktop only) ----------

const finePointer = matchMedia("(hover: hover) and (pointer: fine)");
let idleTimer = null;
function poke() {
  box.classList.remove("idle");
  clearTimeout(idleTimer);
  if (!finePointer.matches) return;
  idleTimer = setTimeout(() => {
    if (hasVideo && !$("controls").matches(":hover, :focus-within") && !(hintBtn.offsetParent)) box.classList.add("idle");
  }, 3000);
}
for (const ev of ["pointermove", "pointerdown", "focusin"]) box.addEventListener(ev, poke);
box.addEventListener("pointerleave", () => { if (finePointer.matches && hasVideo) { clearTimeout(idleTimer); idleTimer = setTimeout(() => box.classList.add("idle"), 1000); } });

// ---------- keyboard ----------

document.addEventListener("keydown", (e) => {
  if (e.ctrlKey || e.metaKey || e.altKey || e.repeat) return;
  if (e.target.closest && e.target.closest("input, textarea, select, [contenteditable]")) return;
  const k = e.key.toLowerCase();
  if (k === "m") { if (!muteBtn.disabled) setMuted(!video.muted); }
  else if (k === "f" && fsSupported) toggleFullscreen();
  else return;
  e.preventDefault();
  poke();
});

// ---------- recording ----------

const canRecord = typeof MediaRecorder !== "undefined" && typeof MediaRecorder.isTypeSupported === "function";
let recorder = null, recStop = null, recTimer = null;

function pickMime() {
  for (const t of ["video/mp4;codecs=avc1,opus", "video/mp4", "video/webm;codecs=vp8,opus", "video/webm"]) {
    if (MediaRecorder.isTypeSupported(t)) return t;
  }
  return "";
}

function fileName(c, start, mime) {
  const p = (n) => String(n).padStart(2, "0");
  const ts = start.getFullYear() + "-" + p(start.getMonth() + 1) + "-" + p(start.getDate()) + "_" +
    p(start.getHours()) + "-" + p(start.getMinutes()) + "-" + p(start.getSeconds());
  const name = String((c && (c.name || c.camera_id)) || "camera").normalize("NFKD")
    .replace(/[^\w-]+/g, "-").replace(/^-+|-+$/g, "").toLowerCase() || "camera";
  return "birdcam-" + name + "-" + ts + "." + (/mp4/.test(mime) ? "mp4" : "webm");
}

// Upright video track: the raw track if not rotated, else a canvas capture of the rotated frames.
function recordVideoTrack(stream) {
  const raw = stream.getVideoTracks();
  const cv = document.createElement("canvas");
  if (!rotation || !cv.captureStream || !video.videoWidth) return { tracks: raw, stop() {} };
  const vw = video.videoWidth, vh = video.videoHeight, rot = rotation;
  const side = rot % 180 !== 0;
  cv.width = side ? vh : vw;
  cv.height = side ? vw : vh;
  const ctx = cv.getContext("2d");
  let alive = true;
  const rvfc = typeof video.requestVideoFrameCallback === "function";
  const draw = () => {
    if (!alive) return;
    ctx.setTransform(1, 0, 0, 1, cv.width / 2, cv.height / 2);
    ctx.rotate(rot * Math.PI / 180);
    ctx.drawImage(video, -vw / 2, -vh / 2, vw, vh);
    if (rvfc) video.requestVideoFrameCallback(draw); else requestAnimationFrame(draw);
  };
  draw();
  const cs = cv.captureStream(30);
  return { tracks: cs.getVideoTracks(), stop() { alive = false; cs.getTracks().forEach((t) => t.stop()); } };
}

function startRecording() {
  const stream = video.srcObject;
  const c = current();
  if (!stream || !c || recorder) return;
  const v = recordVideoTrack(stream);
  // Audio comes straight from the WebRTC track, independent of the <video>'s muted flag.
  const audio = c.audio !== false ? stream.getAudioTracks() : [];
  const mime = pickMime();
  let rec;
  try {
    rec = new MediaRecorder(new MediaStream([...v.tracks, ...audio]), mime ? { mimeType: mime } : undefined);
  } catch (e) {
    v.stop();
    errorFlash("Recording not supported: " + e.message);
    return;
  }
  const chunks = [], start = new Date();
  rec.ondataavailable = (e) => { if (e.data && e.data.size) chunks.push(e.data); };
  rec.onstop = () => {
    v.stop();
    const type = rec.mimeType || mime || "video/webm";
    if (!chunks.length) return;
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob(chunks, { type }));
    a.download = fileName(c, start, type);
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 60000);
  };
  rec.onerror = () => stopRecording();
  try { rec.start(1000); } catch (e) { v.stop(); errorFlash("Recording failed: " + e.message); return; }
  recorder = rec;
  recBtn.classList.add("is-on");
  recBtn.setAttribute("aria-label", "Stop recording");
  recBtn.title = "Stop recording";
  const badge = $("rec-badge"), time = $("rec-time");
  badge.hidden = false;
  const tick = () => {
    const s = Math.floor((Date.now() - start) / 1000);
    time.textContent = (s >= 3600 ? Math.floor(s / 3600) + ":" + String(Math.floor(s / 60) % 60).padStart(2, "0") : Math.floor(s / 60)) +
      ":" + String(s % 60).padStart(2, "0");
  };
  tick();
  recTimer = setInterval(tick, 500);
  updateControls();
}

function stopRecording() {
  if (!recorder) return;
  const rec = recorder;
  recorder = null;
  clearInterval(recTimer);
  $("rec-badge").hidden = true;
  recBtn.classList.remove("is-on");
  recBtn.setAttribute("aria-label", "Start recording");
  recBtn.title = "Record";
  if (rec.state !== "inactive") rec.stop();
  updateControls();
}

let flashTimer = null;
function errorFlash(msg) {
  overlay.hidden = false;
  overlay.classList.add("error");
  overlayText.textContent = msg;
  clearTimeout(flashTimer);
  flashTimer = setTimeout(updateStatus, 4000);
}

recBtn.onclick = () => (recorder ? stopRecording() : startRecording());

// ---------- lifecycle ----------

window.addEventListener("pagehide", () => {
  if (viewerURL) fetch(viewerURL, { method: "DELETE", keepalive: true }).catch(() => {});
});

updateControls();
fit();
poke();
loadSession().then(() => { poll(); setInterval(poll, 2000); });
