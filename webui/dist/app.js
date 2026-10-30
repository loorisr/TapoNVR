"use strict";
import { VideoRTC } from './video-rtc.js';

if (!customElements.get('video-rtc')) customElements.define('video-rtc', VideoRTC);

function esc(s) {
  return String(s).replace(/[&<>"']/g, function (c) {
    return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
  });
}

function headers() {
  // Auth relies on the browser native Basic dialog (page itself requires
  // auth, so credentials are cached and sent automatically on fetch).
  return new Headers();
}

function api(path, method) {
  return fetch(path, { method: method || "GET", headers: headers() }).then(function (r) {
    if (r.status === 401) throw new Error("unauthorized");
    if (!r.ok && r.status !== 202) {
      // Surface the server error (or the status) instead of handing a JSON
      // error body to a renderer that expects a list.
      return r.json().catch(function () { return {}; }).then(function (j) {
        throw new Error((j && j.error) || ("request failed " + r.status));
      });
    }
    return r.json();
  });
}

// toast(msg, kind): non-blocking notification. kind is "info" (default),
// "ok" or "error". Creates div#toasts on demand, auto-removes after 4s.
function toast(msg, kind) {
  var c = document.getElementById("toasts");
  if (!c) {
    c = document.createElement("div");
    c.id = "toasts";
    c.style.cssText = "position:fixed;right:12px;bottom:12px;z-index:9999;display:flex;flex-direction:column;gap:8px;max-width:min(360px,90vw)";
    document.body.appendChild(c);
  }
  var d = document.createElement("div");
  d.textContent = String(msg);
  var bg = kind === "error" ? "#7a1f1f" : kind === "ok" ? "#1f6f3a" : "#26303a";
  d.style.cssText = "background:" + bg + ";color:#fff;padding:10px 12px;border-radius:8px;font-size:13px;box-shadow:0 4px 16px rgba(0,0,0,.4);opacity:.97";
  c.appendChild(d);
  setTimeout(function () { if (d.parentNode) d.parentNode.removeChild(d); }, 4000);
}

// pollManualJob(statusUrl): poll GET statusUrl every 2s up to ~130s after a
// POST returning 202 with status_url (legacy ?duration= manual record).
// Shows a toast on done/error and refreshes the detection list.
function pollManualJob(statusUrl) {
  if (!statusUrl) return;
  var tries = 0, max = 65;
  toast("Recording… assembling clip, please wait.");
  var t = setInterval(function () {
    tries++;
    fetch(statusUrl, { headers: headers() })
      .then(function (r) { if (!r.ok) throw new Error("job status " + r.status); return r.json(); })
      .then(function (j) {
        if (j && j.status === "done") {
          clearInterval(t);
          toast("Video saved — open it from the Recordings tab.", "ok");
          refreshRecordings();
        } else if (j && j.status === "error") {
          clearInterval(t);
          toast("Manual record failed: " + (j.error || "error"), "error");
        } else if (tries >= max) {
          clearInterval(t);
          toast("Still processing — check the Recordings tab.");
          refreshRecordings();
        }
      })
      .catch(function (e) {
        if (tries >= max) { clearInterval(t); toast("Job poll failed: " + e.message, "error"); }
      });
  }, 2000);
}

// loadCamDetectionTuning: prefill per-camera debounce/hold inputs via
// GET /api/cameras/{id}/detection, falling back to global defaults.
function loadCamDetectionTuning(camId, debInput, holdInput) {
  fetch(camURL(camId, "/detection"), { headers: headers() })
    .then(function (r) {
      if (!r.ok) throw new Error("http " + r.status);
      return r.json();
    })
    .then(function (j) {
      if (j && j.debounce_s != null && debInput) debInput.value = j.debounce_s;
      if (j && j.hold_s != null && holdInput) holdInput.value = j.hold_s;
    })
    .catch(function () {
      // No per-camera override yet (or endpoint missing): fall back to
      // global defaults so the inputs are still usable.
      if (debInput && !debInput.value) debInput.value = 10;
      if (holdInput && !holdInput.value) holdInput.value = 2;
    });
}

// makeCamTuningEl: per-camera detection tuning UI (debounce_s 0..3600,
// hold_s 1..60 + save button POSTing /api/cameras/{id}/detection).
// The POST endpoint may not exist yet: a 404 is handled with a toast.
function makeCamTuningEl(cam) {
  var wrap = document.createElement("div");
  wrap.className = "cam-tune";
  wrap.style.cssText = "display:flex;gap:6px;align-items:center;margin-top:6px;flex-wrap:wrap";
  var lab1 = document.createElement("label");
  lab1.className = "meta";
  lab1.textContent = "debounce_s ";
  var deb = document.createElement("input");
  deb.type = "number"; deb.min = "0"; deb.max = "3600"; deb.step = "1";
  deb.value = "10"; deb.title = "debounce_s (0..3600)"; deb.style.width = "64px";
  lab1.appendChild(deb);
  var lab2 = document.createElement("label");
  lab2.className = "meta";
  lab2.textContent = "hold_s ";
  var hold = document.createElement("input");
  hold.type = "number"; hold.min = "1"; hold.max = "60"; hold.step = "1";
  hold.value = "2"; hold.title = "hold_s (1..60)"; hold.style.width = "56px";
  lab2.appendChild(hold);
  var save = document.createElement("button");
  save.className = "btn-sm"; save.type = "button"; save.textContent = "Save detection";
  save.title = "Save per-camera detection tuning";
  save.onclick = function () {
    var ds = Math.max(0, Math.min(3600, parseInt(deb.value, 10) || 0));
    var hs = Math.max(1, Math.min(60, parseInt(hold.value, 10) || 2));
    deb.value = ds; hold.value = hs;
    save.disabled = true;
    fetch(camURL(cam.id, "/detection"), {
      method: "POST", headers: headers(),
      body: JSON.stringify({ debounce_s: ds, hold_s: hs })
    })
      .then(function (r) {
        if (r.status === 404) throw new Error("not supported by server (404)");
        if (!r.ok) return r.json().catch(function () { return {}; }).then(function (j) { throw new Error((j && j.error) || ("save failed " + r.status)); });
        return r.json();
      })
      .then(function () { toast("Detection tuning saved for " + cam.name + ".", "ok"); })
      .catch(function (e) { toast("Detection tuning save failed: " + e.message, "error"); })
      .finally(function () { save.disabled = false; });
  };
  wrap.appendChild(lab1); wrap.appendChild(lab2); wrap.appendChild(save);
  loadCamDetectionTuning(cam.id, deb, hold);
  return wrap;
}

// camURL encodes a camera id into an API path (ids come from config and may
// contain "/", "?" or spaces).
function camURL(id, suffix) {
  return "/api/cameras/" + encodeURIComponent(id) + (suffix || "");
}
// camSel builds a CSS selector for a camera tile without letting a quote or
// backslash in the id turn into a selector syntax error.
function camSel(id) {
  return '.tile[data-cam="' + String(id).replace(/["\\]/g, "\\$&") + '"] .rec-btn';
}
// _filterBarHasFocus is true while the user is typing in a filter control: the
// periodic re-render must not rebuild the bar under their fingers.
function _filterBarHasFocus() {
  var a = document.activeElement;
  return !!(a && a.closest && a.closest(".filters, .filters-row"));
}

function ptz(id, action, pan, tilt) {
  var q = camURL(id, "/ptz?action=" + action);
  if (pan !== undefined) q += "&pan=" + pan;
  if (tilt !== undefined) q += "&tilt=" + tilt;
  return api(q, "POST").catch(function () {});
}

function makePtzPad(c) {
  var pad = document.createElement("div");
  pad.className = "ptz";
  var btn = function (label, cls, pan, tilt) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "p-btn " + cls;
    b.textContent = label;
    b.onmousedown = function (e) { e.preventDefault(); ptz(c.id, "move", pan, tilt); };
    b.onmouseup = function (e) { e.preventDefault(); ptz(c.id, "stop"); };
    b.onmouseleave = function (e) { if (e.buttons) ptz(c.id, "stop"); };
    b.onclick = function (e) { e.preventDefault(); };
    return b;
  };
  pad.appendChild(btn("▲", "up", 0, 0.8));
  pad.appendChild(btn("◀", "left", -0.8, 0));
  pad.appendChild(btn("▼", "down", 0, -0.8));
  pad.appendChild(btn("▶", "right", 0.8, 0));
  return pad;
}

function tickClock() {
  var el = document.getElementById("clockTxt");
  if (!el) return;
  el.textContent = new Date().toLocaleString();
}
setInterval(tickClock, 1000);
tickClock();

var detOn = true;

function renderDetection(on) {
  detOn = !!on;
  var b = document.getElementById("detToggle");
  if (!b) return;
  b.className = detOn ? "det-on" : "det-off";
  b.textContent = "Detection: " + (detOn ? "ON" : "OFF");
}

function refreshDetection() {
  api("/api/detection/state").then(function (s) { renderDetection(s.enabled); }).catch(function () {});
}

var _detToggleEl = document.getElementById("detToggle");
if (_detToggleEl) _detToggleEl.onclick = function () {
  api("/api/detection/state?enabled=" + !detOn, "POST")
    .then(function (s) { renderDetection(s.enabled); })
    .catch(function () {});
};

// Generic header toggle: GETs state on load, POSTs flipped value on click.
function makeToggle(id, path, label) {
  var el = document.getElementById(id);
  if (!el) return;
  var on = false;
  function render(v) {
    on = !!v;
    el.hidden = false;
    el.className = on ? "det-on" : "det-off";
    el.textContent = label + ": " + (on ? "ON" : "OFF");
  }
  el.onclick = function () {
    api(path + "?enabled=" + !on, "POST")
      .then(function (s) { render(s.enabled); })
      .catch(function () {});
  };
  api(path).then(function (s) { render(s.enabled); }).catch(function () { el.hidden = false; });
}
function refreshToggles() {
  makeToggle("recToggle", "/api/recording/state", "Clip");
  makeToggle("snapToggle", "/api/snapshots/state", "Snap");
  makeToggle("s3Toggle", "/api/s3/state", "S3");
  makeToggle("pushToggle", "/api/pushover/state", "Push");
  makeToggle("visionToggle", "/api/vision/state", "Vision");
}

// ---------- vision (YOLO) helpers ----------
function visionBadgeText(d){
  if(!d || !d.vision) return "";
  var v=d.vision;
  if(v.count>0) return "👁 "+(v.top||(v.count+" obj"))+(v.count>1?" (+"+(v.count-1)+")":"");
  if(v.error) return "👁—";
  return "";
}
// bboxBadgeText summarizes the camera-side RTSP SEI boxes (distinct types).
function bboxBadgeText(d){
  if(!d || !d.bbox || !d.bbox.length) return "";
  var seen={}, arr=[];
  for(var i=0;i<d.bbox.length;i++){
    var t=d.bbox[i] && d.bbox[i].type;
    if(t && !seen[t]){ seen[t]=1; arr.push(t); }
  }
  return arr.length ? "▣ "+arr.join("/") : "";
}
// annotatedURL returns the snapshot URL with boxes burned in (server-side
// ?boxes=1) when this detection has a positive YOLO result or camera-side RTSP
// SEI boxes. The server serves the plain JPEG when neither applies.
function visionBoxesURL(d, u, maxPx){
  if(!u) return u;
  var hasVision = d && d.vision && d.vision.count>0;
  var hasRTSP = d && d.bbox && d.bbox.length>0;
  if(hasVision || hasRTSP){
    var q = (u.indexOf("?")>=0?"&":"?")+"boxes=1";
    if(maxPx) q += "&max="+encodeURIComponent(maxPx);
    return u+q;
  }
  return u;
}
function refreshVisionDetail(d){
  var el=document.getElementById("dvision");
  if(!el) return;
  el.innerHTML="";
  var v=d.vision;
  if(!v || (v.count===0 && !v.error)){ el.hidden=true; return; }
  el.hidden=false;
  var head=document.createElement("div"); head.className="meta";
  head.textContent=v.count>0?("👁 YOLO: "+v.count+" objet(s) · top "+(v.top||"—")+" · "+v.ms+" ms"):("👁 YOLO: "+(v.error||"pas de résultat"));
  el.appendChild(head);
  if(v.detections && v.detections.length){
    var ul=document.createElement("div"); ul.className="meta";
    ul.style.cssText = "display:flex;flex-direction:column;gap:4px;margin:4px 0";
    v.detections.slice(0,10).forEach(function(dt, idx){
      var row=document.createElement("div");
      row.style.cssText="display:flex;gap:8px;align-items:center;flex-wrap:wrap";
      var lab=document.createElement("span");
      lab.textContent=(dt.label||"?")+" "+(dt.confidence!=null?Number(dt.confidence).toFixed(2):"");
      row.appendChild(lab);
      var wb=document.createElement("button");
      wb.className="btn-sm"; wb.type="button"; wb.textContent="wrong label";
      wb.title="Report this YOLO label as wrong (vision feedback)";
      wb.onclick=function(){
        wb.disabled=true;
        var snapIdx = (dt.snap_index != null) ? dt.snap_index : (dt.snapIndex != null ? dt.snapIndex : 0);
        fetch("/api/detections/"+encodeURIComponent(d.id)+"/vision/exclude",{method:"POST",headers:headers(),body:JSON.stringify({snap_index:snapIdx,label:dt.label||""})})
          .then(function(r){
            if(r.status===404) throw new Error("not supported by server (404)");
            return r.json().then(function(j){ return {ok:r.ok,body:j}; });
          })
          .then(function(res){
            if(!res.ok) throw new Error((res.body&&res.body.error)||"exclude failed");
            toast("Vision feedback saved — label excluded.", "ok");
          })
          .catch(function(e){ toast("Vision feedback failed: "+e.message, "error"); })
          .finally(function(){ wb.disabled=false; });
      };
      row.appendChild(wb);
      ul.appendChild(row);
    });
    el.appendChild(ul);
  }
  var btn=document.createElement("button"); btn.className="btn-sm"; btn.textContent="↻ Re-analyser";
  btn.onclick=function(){
    btn.disabled=true; btn.textContent="… analyse";
    fetch("/api/detections/"+encodeURIComponent(d.id)+"/vision",{method:"POST",headers:headers()})
      .then(function(r){ return r.json().then(function(j){ return {ok:r.ok,body:j}; }); })
      .then(function(res){
        if(!res.ok) throw new Error((res.body&&res.body.error)||"analyse failed");
        if(res.body&&res.body.queued){
          // Workers saturated: queued server-side, result lands on refresh.
          btn.textContent="⏳ en file";
          setTimeout(function(){ refreshRecordings(); if(curDetail===d) refreshVisionDetail(d); }, 8000);
          return;
        }
        // d is the detection this panel was built for: curDetail may be null
        // (modal closed mid-request) or point at another recording by now.
        d.vision=res.body.vision;
        if(curDetail===d) refreshVisionDetail(d);
        refreshRecordings();
      })
      .catch(function(e){ toast("Vision re-analyse failed: "+e.message, "error"); })
      .finally(function(){ btn.disabled=false; btn.textContent="↻ Re-analyser"; });
  };
  el.appendChild(btn);
}

// Live stream transport: auto (default), webrtc-only or mse-only.
// Persisted in localStorage, applied to every video-rtc player.
var _streamMode = "auto";
try { _streamMode = localStorage.getItem("streamMode") || "auto"; } catch (e) {}
function _playerMode() {
  if (_streamMode === "webrtc") return "webrtc";
  if (_streamMode === "mse") return "mse";
  return "webrtc,mse,hls,mjpeg";
}
function _applyStreamMode() {
  var mode = _playerMode();
  _players.forEach(function (p) {
    try {
      p.el.mode = mode;
      // Reconnect so the new mode takes effect immediately.
      if (p.el.ondisconnect) p.el.ondisconnect();
      _statPrev.delete(p.el);
      p.stats.textContent = "connecting…";
      if (p.el.onconnect) p.el.onconnect();
    } catch (e) {}
  });
}
function initStreamMode() {
  var sel = document.getElementById("streamMode");
  if (!sel) return;
  sel.value = _streamMode;
  sel.onchange = function () {
    _streamMode = sel.value || "auto";
    try { localStorage.setItem("streamMode", _streamMode); } catch (e) {}
    _applyStreamMode();
  };
}

function renderCameras(cams) {
  var body = document.getElementById("gridBody");
  body.innerHTML = "";
  _players = [];
  _bannerByCam = Object.create(null);
  _boxCanvasByCam = Object.create(null);
  _boxKeyAlias = Object.create(null);
  _boxState = Object.create(null);
  cams.forEach(function (c) {
    var tile = document.createElement("div");
    tile.className = "tile";
    tile.dataset.cam = c.id;
    var ph = document.createElement("div");
    ph.className = "live-ph";
    ph.textContent = "● " + c.name;

    var vwrap = document.createElement("div");
    vwrap.className = "tile-vid";
    vwrap.appendChild(ph);
    var player = document.createElement("video-rtc");
    player.style.width = "100%";
    player.style.height = "100%";
    player.style.display = "block";
    player.media = "video,audio";
    player.mode = _playerMode();
    if (c.enabled && c.stream) {
      player.src = c.stream;
    } else {
      ph.textContent = "● " + c.name + " (offline)";
    }
    vwrap.appendChild(player);
    var boxCanvas = document.createElement("canvas");
    boxCanvas.className = "box-overlay";
    boxCanvas.hidden = !_boxesOn;
    vwrap.appendChild(boxCanvas);
    tile.appendChild(vwrap);

    var cap = document.createElement("div");
    cap.className = "cap";
    var top = document.createElement("div");
    top.className = "cap-top";
    var nameEl = document.createElement("span");
    nameEl.textContent = c.name;
    var banner = document.createElement("span");
    banner.className = "live-banner";
    banner.hidden = true;
    banner.textContent = "⚠ motion detected";
    var statusEl = document.createElement("span");
    statusEl.textContent = (c.enabled ? "live" : "disabled") + (c.ptz_available ? " · PTZ" : "");
    top.appendChild(nameEl);
    top.appendChild(banner);
    top.appendChild(statusEl);
    cap.appendChild(top);
    var stats = document.createElement("div");
    stats.className = "live-stats";
    cap.appendChild(stats);
    tile.appendChild(cap);

    _players.push({ el: player, stats: stats, banner: banner, canvas: boxCanvas });
    _bannerByCam[c.id] = banner;
    _bannerByCam[c.name] = banner; // compat: SSE older payloads carry name only
    _boxCanvasByCam[c.id] = boxCanvas;
    _boxKeyAlias[c.name] = c.id; // SSE payloads may carry name only
    boxCanvas._player = player;

    var actions = document.createElement("div");
    actions.className = "tile-actions";
    var snapBtn = document.createElement("button");
    snapBtn.className = "btn-sm snap-btn";
    snapBtn.textContent = "📸 Snapshot";
    snapBtn.onclick = function () {
      snapBtn.disabled = true;
      snapBtn.textContent = "… snapshot";
      fetch(camURL(c.id, "/snapshot"), { method: "POST", headers: headers() })
        .then(function (r) { if (!r.ok) throw new Error("snapshot failed " + r.status); return r.json(); })
        .then(function (j) { if (j.snapshot) showLightbox(j.snapshot); else if (j.error) throw new Error(j.error); })
        .catch(function (e) { toast("Snapshot failed: " + e.message, "error"); })
        .finally(function () { snapBtn.disabled = false; snapBtn.textContent = "📸 Snapshot"; });
    };
    actions.appendChild(snapBtn);
    var recBtn = document.createElement("button");
    recBtn.className = "btn-sm rec-btn";
    recBtn.textContent = _recActive[c.id] ? "⏹ Stop" : "🎬 Record";
    recBtn.style.marginLeft = "6px";
    if (_recActive[c.id]) { recBtn.style.background = "#c0392b"; recBtn.style.color = "#fff"; }
    // poll status
    fetch(camURL(c.id, "/record"), { headers: headers() }).then(function(r){return r.json();}).then(function(j){ _recActive[c.id]=!!j.recording; if(recBtn.disabled) return; recBtn.textContent=_recActive[c.id]?"⏹ Stop":"🎬 Record"; recBtn.style.background=_recActive[c.id]?"#c0392b":""; recBtn.style.color=_recActive[c.id]?"#fff":""; }).catch(function(){});
    recBtn.onclick = function () {
      var isActive = !!_recActive[c.id];
      recBtn.disabled = true;
      recBtn.textContent = isActive ? "… stopping" : "… starting";
      fetch(camURL(c.id, "/record"), { method: "POST", headers: headers() })
        .then(function (r) { if (!r.ok) throw new Error("record failed " + r.status); return r.json(); })
        .then(function (j) {
          if (j.recording) { _recActive[c.id]=true; recBtn.textContent="⏹ Stop"; recBtn.style.background="#c0392b"; recBtn.style.color="#fff"; }
          else if (j.clip) { _recActive[c.id]=false; recBtn.textContent="🎬 Record"; recBtn.style.background=""; recBtn.style.color=""; toast("Video saved — open it from the Recordings tab.", "ok"); }
          else if (j.async && j.status_url) { pollManualJob(j.status_url); }
          else { _recActive[c.id]=!isActive; recBtn.textContent=_recActive[c.id]?"⏹ Stop":"🎬 Record"; recBtn.style.background=_recActive[c.id]?"#c0392b":""; recBtn.style.color=_recActive[c.id]?"#fff":""; }
        })
        .catch(function (e) { toast("Record failed: " + e.message, "error"); })
        .finally(function () { recBtn.disabled = false; if(!_recActive[c.id]) { recBtn.textContent="🎬 Record"; recBtn.style.background=""; } else { recBtn.textContent="⏹ Stop"; } });
    };
    actions.appendChild(recBtn);
    tile.appendChild(actions);
    // Per-camera detection tuning (debounce_s / hold_s). Prefilled via
    // GET /api/cameras/{id}/detection, saved via POST same URL.
    try { tile.appendChild(makeCamTuningEl(c)); } catch (e) {}

    if (c.ptz_available) {
      tile.appendChild(makePtzPad(c));
    }
    body.appendChild(tile);
  });
  refreshStats();
}

function _safeMediaURL(src) {
  if (typeof src !== "string" || !src) return null;
  if (src.indexOf("/api/") === 0) return src;
  if (src.indexOf("blob:") === 0) return src;
  if (src.indexOf("data:image/") === 0) return src;
  try {
    var u = new URL(src, location.href);
    if (u.origin === location.origin) return src;
  } catch (e) {}
  return null;
}
// _imgFallback keeps a broken <img> (missing/pruned media) from showing a
// broken-image icon: neutral placeholder styling + no src-retry loop.
function _imgFallback(img, label) {
  if (!img) return;
  img.onerror = function () {
    img.onerror = null;
    img.removeAttribute("src");
    img.alt = label || "média manquant";
    img.style.background = "#3a424c";
    img.style.minHeight = "80px";
    img.style.display = "block";
    img.title = label || "média manquant";
  };
}
function showLightbox(src) {
  src = _safeMediaURL(src) || src;
  if (typeof src !== "string" || !(src.indexOf("/api/") === 0 || src.indexOf("blob:") === 0 || src.indexOf("data:image/") === 0 || src.indexOf(location.origin) === 0)) return;
  hideLightbox();
  var lb = document.createElement("div");
  lb.id = "lightbox";
  lb.className = "lightbox";
  lb.onclick = hideLightbox;
  var img = document.createElement("img");
  _imgFallback(img, "média manquant");
  img.src = src;
  img.alt = "snapshot large";
  img.onclick = function (e) { e.stopPropagation(); };
  var close = document.createElement("button");
  close.className = "lightbox-close";
  close.textContent = "✕";
  close.onclick = hideLightbox;
  lb.appendChild(img);
  lb.appendChild(close);
  document.body.appendChild(lb);
  lb._esc = function (e) { if (e.key === "Escape") hideLightbox(); };
  window.addEventListener("keydown", lb._esc);
}
function hideLightbox() {
  var lb = document.getElementById("lightbox");
  if (lb) {
    if (lb._esc) window.removeEventListener("keydown", lb._esc);
    lb.remove();
  }
}

var _players = [];
var _statPrev = new WeakMap();
var _bannerByCam = Object.create(null);
var _recActive = Object.create(null);
var _bannerTimers = Object.create(null);

// ---------- live detection boxes overlay (RTSP SEI over SSE) ----------
// The backend parses the camera's own AI boxes from the H.264 SEI metadata
// and pushes them as SSE events (kind:"boxes", throttled to ~5/s). We draw
// them on a transparent canvas layered over the <video>.
var _boxesOn = true;
try { if (localStorage.getItem("liveBoxes") === "0") _boxesOn = false; } catch (e) {}
var _boxCanvasByCam = Object.create(null);
var _boxKeyAlias = Object.create(null); // camera name -> cam id (legacy SSE)
var _boxState = Object.create(null);    // cam id -> {boxes, at, clearT}
var _boxColors = {
  motion: "#ffeb3b", person: "#4caf50", vehicle: "#2196f3",
  pet: "#ff9800", other: "#e91e63"
};
var BOX_HOLD_MS = 900; // drop boxes when the camera stops reporting them
// Display delay: the SSE boxes come from a separate RTSP session and can lead
// the WebRTC/MSE picture by a couple hundred ms. Hold updates briefly so the
// overlay stays on top of the object; tune via
// localStorage.setItem("liveBoxesDelay", "0") if your transport is slower.
var BOX_DELAY_MS = 200;
try {
  var _bd = parseInt(localStorage.getItem("liveBoxesDelay"), 10);
  if (!isNaN(_bd)) BOX_DELAY_MS = Math.max(0, Math.min(3000, _bd));
} catch (e) {}

function _boxColor(t) { return _boxColors[t] || "#00bcd4"; }

// _drawBoxes paints one camera's canvas. Boxes are normalized [0,1]; the
// video is letterboxed (object-fit: contain) inside the element, so the
// source rectangle is computed from videoWidth/videoHeight when available.
function _drawBoxes(camID) {
  var c = _boxCanvasByCam[camID];
  if (!c) return;
  var cw = c.clientWidth || 0, ch = c.clientHeight || 0;
  if (cw <= 0 || ch <= 0) return;
  var dpr = window.devicePixelRatio || 1;
  var pw = Math.round(cw * dpr), ph = Math.round(ch * dpr);
  if (c.width !== pw || c.height !== ph) { c.width = pw; c.height = ph; }
  var ctx = c.getContext("2d");
  if (!ctx) return;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, cw, ch);
  var st = _boxState[camID];
  if (!_boxesOn || !st || !st.boxes || !st.boxes.length) return;

  var vw = 0, vh = 0;
  try {
    var v = c._player && c._player.video;
    if (v && v.videoWidth > 0 && v.videoHeight > 0) { vw = v.videoWidth; vh = v.videoHeight; }
  } catch (e) {}
  var sw = cw, sh = ch, ox = 0, oy = 0;
  if (vw > 0 && vh > 0) {
    var s = Math.min(cw / vw, ch / vh);
    sw = vw * s; sh = vh * s;
    ox = (cw - sw) / 2; oy = (ch - sh) / 2;
  }
  var thick = Math.max(2, Math.round(Math.min(cw, ch) / 240));
  var fs = Math.max(11, Math.round(Math.min(cw, ch) / 34));
  ctx.lineWidth = thick;
  ctx.font = "600 " + fs + "px system-ui, sans-serif";
  ctx.textBaseline = "middle";
  st.boxes.forEach(function (b) {
    if (!b) return;
    var x1 = ox + b.x1 * sw, y1 = oy + b.y1 * sh;
    var x2 = ox + b.x2 * sw, y2 = oy + b.y2 * sh;
    if (x2 < ox) x2 = ox; if (y2 < oy) y2 = oy;
    if (x1 > ox + sw) x1 = ox + sw; if (y1 > oy + sh) y1 = oy + sh;
    if (x2 <= x1 || y2 <= y1) return;
    var col = _boxColor(b.type);
    ctx.strokeStyle = col;
    ctx.strokeRect(x1, y1, x2 - x1, y2 - y1);
    var label = String(b.type || "?").toUpperCase();
    if (b.score > 0) label += " " + Number(b.score).toFixed(2);
    var tw = ctx.measureText(label).width;
    var th = fs + 6;
    var ty = y1 - th - 2;
    if (ty < oy) ty = y1 + 2;
    ctx.fillStyle = col;
    ctx.fillRect(x1, ty, tw + 8, th);
    ctx.fillStyle = "#000";
    ctx.fillText(label, x1 + 4, ty + th / 2);
  });
}

function _redrawAllBoxes() {
  Object.keys(_boxCanvasByCam).forEach(function (id) { _drawBoxes(id); });
}

function _setLiveBoxes(camID, boxes) {
  if (!_boxCanvasByCam[camID]) return;
  var st = _boxState[camID] || (_boxState[camID] = { boxes: null, at: 0, clearT: null, timer: null, pending: null });
  st.pending = boxes;
  if (st.timer) return; // a delayed apply is already scheduled
  if (BOX_DELAY_MS <= 0) { _applyLiveBoxes(camID); return; }
  st.timer = setTimeout(function () {
    st.timer = null;
    _applyLiveBoxes(camID);
  }, BOX_DELAY_MS);
}

function _applyLiveBoxes(camID) {
  var st = _boxState[camID];
  if (!st || st.pending === null) return;
  st.boxes = st.pending;
  st.pending = null;
  st.at = Date.now();
  if (st.clearT) clearTimeout(st.clearT);
  st.clearT = setTimeout(function () {
    st.clearT = null;
    if (Date.now() - st.at < BOX_HOLD_MS - 50) return;
    st.boxes = null;
    _drawBoxes(camID);
  }, BOX_HOLD_MS);
  _drawBoxes(camID);
}

function initLiveBoxesToggle() {
  var el = document.getElementById("boxesToggle");
  if (!el) return;
  el.checked = _boxesOn;
  el.onchange = function () {
    _boxesOn = !!el.checked;
    try { localStorage.setItem("liveBoxes", _boxesOn ? "1" : "0"); } catch (e) {}
    Object.keys(_boxCanvasByCam).forEach(function (id) {
      _boxCanvasByCam[id].hidden = !_boxesOn;
      if (!_boxesOn) { _boxState[id] = null; _drawBoxes(id); }
    });
    if (_boxesOn) _redrawAllBoxes();
  };
}
window.addEventListener("resize", _redrawAllBoxes);

function _renderStats(p, fps, bitrate, rtt, mode, delayMs) {
  var fpsTxt = (fps != null && fps > 0) ? Math.round(fps) + " fps" : "— fps";
  var kbpsTxt = (bitrate != null && bitrate > 0) ? bitrate + " kbps" : "— kbps";
  var parts = [mode || "…", fpsTxt, kbpsTxt];
  if (rtt != null && rtt > 0) parts.push(rtt + " ms");
  if (delayMs != null && delayMs >= 0) parts.push("delay " + Math.round(delayMs) + "ms");
  p.stats.textContent = parts.join(" · ");
}

// _bufferDelayMs estimates how far behind the live edge an MSE player is:
// buffered end (what has arrived) minus currentTime (what is shown).
// Returns ms, or null when not measurable (e.g. nothing buffered yet).
function _bufferDelayMs(video) {
  try {
    if (!video || !video.buffered || video.buffered.length === 0) return null;
    var cur = video.currentTime;
    if (!isFinite(cur)) return null;
    var end = video.buffered.end(video.buffered.length - 1);
    if (!isFinite(end)) return null;
    var d = (end - cur) * 1000;
    if (d < 0 || d > 30000) return null;
    return d;
  } catch (e) { return null; }
}

function _fpsFromVideo(video, prev, now) {
  try {
    if (!video || !video.getVideoPlaybackQuality) return { fps: null, frames: prev && prev.frames };
    var total = video.getVideoPlaybackQuality().totalVideoFrames;
    if (total == null) return { fps: null, frames: prev && prev.frames };
    var fps = null;
    if (prev && prev.frameTs != null && prev.frames != null) {
      var dt = (now - prev.frameTs) / 1000;
      if (dt >= 0.5 && total >= prev.frames) fps = (total - prev.frames) / dt;
    }
    return { fps: fps, frames: total };
  } catch (e) { return { fps: null, frames: prev && prev.frames }; }
}

// refreshStats polls each player for fps (always) + bandwidth and delay.
// WebRTC delay comes from getStats jitter-buffer totals; MSE has no
// RTCPeerConnection, so fps is computed from the video element frame
// counter, bitrate shows "— kbps", and delay is estimated from how far
// video.buffered runs ahead of currentTime. Runs every 2s.
function refreshStats() {
  _players.forEach(function (p) {
    var now = Date.now();
    var prev = _statPrev.get(p.el) || {};
    var pc = p.el && p.el.pc;
    var video = p.el && p.el.video;
    if (!pc) {
      var r = _fpsFromVideo(video, prev, now);
      var bd = _bufferDelayMs(video);
      // Smooth the buffered-edge estimate so the number doesn't flicker.
      if (bd != null && prev.delayMs != null) bd = Math.round(prev.delayMs * 0.5 + bd * 0.5);
      else if (bd == null) bd = prev.delayMs != null ? prev.delayMs : null;
      _statPrev.set(p.el, { ts: now, bySsrc: prev.bySsrc || {}, frames: r.frames, frameTs: now, delayMs: bd });
      if (r.fps != null && r.fps > 0) _renderStats(p, r.fps, null, null, "mse", bd);
      else if (prev.frames != null) _renderStats(p, null, null, null, "mse", bd);
      else p.stats.textContent = "mse · connecting…";
      return;
    }
    pc.getStats().then(function (entries) {
      var livePrev = _statPrev.get(p.el) || {};
      var ts = Date.now();
      var rtt = null, fps = null, framesReceived = null;
      var jbDelay = null, jbEmitted = null;
      // Collect current per-SSRC inbound-video byte counters.
      var cur = {};
      entries.forEach(function (r) {
        if (r.type === "inbound-rtp" && r.kind === "video") {
          if (r.ssrc !== undefined && r.bytesReceived !== undefined) {
            cur[r.ssrc] = (cur[r.ssrc] || 0) + r.bytesReceived;
          }
          if (r.framesPerSecond) fps = r.framesPerSecond;
          if (r.framesReceived !== undefined) {
            framesReceived = (framesReceived || 0) + r.framesReceived;
          }
          // Cumulative jitter-buffer totals -> average playout delay.
          if (typeof r.jitterBufferDelay === "number") jbDelay = (jbDelay || 0) + r.jitterBufferDelay;
          if (typeof r.jitterBufferEmittedCount === "number") jbEmitted = (jbEmitted || 0) + r.jitterBufferEmittedCount;
        }
        if (r.type === "candidate-pair" && r.nominated === true && r.state === "succeeded") {
          rtt = Math.round((r.currentRoundTripTime || 0) * 1000);
        }
      });
      var delayMs = null;
      if (jbDelay != null && jbEmitted != null && jbEmitted > 0) delayMs = (jbDelay / jbEmitted) * 1000;

      var bitrate = null;
      if (livePrev.ts) {
        var dt = (ts - livePrev.ts) / 1000;
        // Require a sane sample window so a back-to-back getStats() (tiny dt)
        // can't inflate the number.
        if (dt >= 0.5) {
          var delta = 0;
          for (var ssrc in cur) {
            if (livePrev.bySsrc && livePrev.bySsrc[ssrc] !== undefined && cur[ssrc] >= livePrev.bySsrc[ssrc]) {
              delta += cur[ssrc] - livePrev.bySsrc[ssrc];
            }
          }
          bitrate = Math.round((delta * 8) / dt / 1000);
          if (!(bitrate > 0)) bitrate = null;
        } else {
          bitrate = livePrev.bitrate != null ? livePrev.bitrate : null;
        }
      }
      // Fallback fps from cumulative framesReceived when framesPerSecond missing (Firefox).
      if ((fps == null || !(fps > 0)) && framesReceived != null && livePrev.framesReceived != null && livePrev.ts) {
        var fdt = (ts - livePrev.ts) / 1000;
        if (fdt >= 0.5 && framesReceived >= livePrev.framesReceived) {
          var f = (framesReceived - livePrev.framesReceived) / fdt;
          if (f > 0) fps = f;
        }
      }
      if (fps == null || !(fps > 0)) fps = livePrev.fps != null ? livePrev.fps : null;
      if (rtt == null || !(rtt > 0)) rtt = livePrev.rtt != null ? livePrev.rtt : null;
      if (delayMs == null) delayMs = livePrev.delayMs != null ? livePrev.delayMs : null;
      _statPrev.set(p.el, { ts: ts, bySsrc: cur, framesReceived: framesReceived, fps: fps, bitrate: bitrate, rtt: rtt, delayMs: delayMs });

      if (fps != null || bitrate != null) _renderStats(p, fps, bitrate, rtt, "webrtc", delayMs);
      else {
        // WebRTC connected but no samples yet: try video-frame fallback.
        var vr = _fpsFromVideo(video, { frames: livePrev.frames, frameTs: livePrev.ts }, ts);
        if (vr.fps != null && vr.fps > 0) _renderStats(p, vr.fps, bitrate, rtt, "webrtc", delayMs);
        else p.stats.textContent = "webrtc · connecting…";
      }
    }).catch(function () {
      var cp = _statPrev.get(p.el) || {};
      var vr2 = _fpsFromVideo(video, cp, Date.now());
      if (vr2.fps != null && vr2.fps > 0) _renderStats(p, vr2.fps, null, null, "mse", cp.delayMs != null ? cp.delayMs : null);
      else p.stats.textContent = "mse · connecting…";
    });
  });
}

setInterval(refreshStats, 2000);
function refreshRecStatus(){
  // Single aggregated call (was: 1 + N per-camera /record polls every 3 s).
  api("/api/cameras/record").then(function(j){
    var states = (j && j.cameras) || {};
    Object.keys(states).forEach(function(id){
      var s = states[id] || {};
      _recActive[id]=!!s.recording;
      var btn = document.querySelector(camSel(id));
      if(btn && !btn.disabled){ btn.textContent=_recActive[id]?"⏹ Stop":"🎬 Record"; btn.style.background=_recActive[id]?"#c0392b":""; btn.style.color=_recActive[id]?"#fff":""; }
    });
  }).catch(function(){});
}
setInterval(refreshRecStatus, 3000);
var activeTab = 'live';
function switchTab(tab) {
  activeTab = tab;
  document.querySelectorAll('.vtab').forEach(function(b){ b.classList.toggle('active', b.dataset.tab===tab); });
  document.getElementById('tab-live').hidden = tab!=='live';
  document.getElementById('tab-events').hidden = tab!=='events';
  document.getElementById('tab-recordings').hidden = tab!=='recordings';
  document.getElementById('tab-status').hidden = tab!=='status';
  // close the event view modal when switching tabs (user requested)
  hideDetailModal();
}
document.querySelectorAll('.vtab').forEach(function(b){
  b.onclick = function(){ switchTab(b.dataset.tab); if(b.dataset.tab==='events') refreshEvents(); if(b.dataset.tab==='recordings') refreshRecordings(); if(b.dataset.tab==='status') refreshSystemStatus(); };
});

function pill(ok) {
  return "<span class='badge' style='background:" + (ok ? "#2e7d32" : "#6e7681") + "'>" + (ok ? "ON" : "OFF") + "</span>";
}
function upPill(ok) {
  return "<span class='badge' style='background:" + (ok ? "#2e7d32" : "#da3633") + "'>" + (ok ? "UP" : "DOWN") + "</span>";
}
// visionStatusHTML renders the YOLO sidecar state for the Status tab:
// OFF (disabled) | HEALTHY (enabled + /ready ok) | OFFLINE (enabled, API down)
// | PAUSED (failure circuit breaker open, retry in Ns).
function visionStatusHTML(st, w) {
  var on = st.vision_enabled === "true" || st.vision_enabled === true;
  if (!on) return pill(false) + " <span class='meta'>disabled</span>";
  if (w && (w.vision_backoff === true || w.vision_backoff === "true")) {
    var until = +w.vision_backoff_until || 0;
    var secs = Math.max(0, until - Math.floor(Date.now() / 1000));
    return "<span class='badge' style='background:#9e6a03'>PAUSED</span> <span class='meta'>retry in " +
      secs + "s · " + (+w.vision_fail_streak || 0) + " failure(s)</span>";
  }
  var ready = st.vision_ready === "true" || st.vision_ready === true;
  var badge = "<span class='badge' style='background:" + (ready ? "#2e7d32" : "#da3633") + "'>" + (ready ? "HEALTHY" : "OFFLINE") + "</span>";
  var meta = [];
  if (st.vision_model) meta.push(esc(st.vision_model));
  if (st.vision_analyzed != null) meta.push(esc(st.vision_analyzed) + " analyzed");
  if (st.vision_avg_ms != null && +st.vision_analyzed > 0) meta.push("avg " + esc(st.vision_avg_ms) + " ms ORT");
  if (st.vision_avg_total_ms != null && +st.vision_avg_total_ms > 0) meta.push("avg " + esc(st.vision_avg_total_ms) + " ms tot");
  if (!ready && st.vision_error_last) meta.push(esc(String(st.vision_error_last).slice(0, 60)));
  return badge + (meta.length ? " <span class='meta'>" + meta.join(" · ") + "</span>" : "");
}
function statCard(label, value) {
  return "<div class='stat-card'><div class='stat-label'>" + esc(label) + "</div><div class='stat-value'>" + value + "</div></div>";
}
function fmtUptime(sec) {
  if (sec == null || isNaN(sec)) return "-";
  var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d > 0) return d + "d " + h + "h";
  if (h > 0) return h + "h " + m + "m";
  if (m > 0) return m + "m " + (sec % 60) + "s";
  return sec + "s";
}
function refreshSystemStatus() {
  api("/api/system/status").then(function (j) {
    var st = j.status || {}, sx = j.stats || {}, wk = j.workers || {};
    var box = document.getElementById("sysStatus");
    box.innerHTML =
      statCard("Streams", esc(st.streams_up) + " / " + esc(st.streams_enabled) + " up <span class='meta'>(" + esc(st.streams_total) + " total)</span>") +
      statCard("Backend RAM", esc(fmtSize(st.backend_mem_alloc)) + " <span class='meta'>sys " + esc(fmtSize(st.backend_mem_sys)) + "</span>") +
      statCard("go2rtc RAM", st.go2rtc_mem_rss ? esc(fmtSize(st.go2rtc_mem_rss)) : "-") +
      statCard("go2rtc", upPill(st.go2rtc_running && st.go2rtc_http_ok) + " <span class='meta'>:" + esc(st.go2rtc_port) + " / rtsp :" + esc(st.go2rtc_rtsp_port) + "</span>") +
      statCard("S3 upload", pill(st.s3_enabled === "true" || st.s3_enabled === true)) +
      statCard("Push notification", pill(st.pushover_enabled === "true" || st.pushover_enabled === true)) +
      statCard("MQTT", (st.mqtt_enabled === "true" || st.mqtt_enabled === true ? pill(true) + " " + upPill(st.mqtt_connected === "true" || st.mqtt_connected === true) : pill(false)) + (st.mqtt_broker ? " <span class='meta'>" + esc(st.mqtt_broker) + "</span>" : "")) +
      statCard("Detection", pill(st.detection_enabled === "true" || st.detection_enabled === true) + (st.detection_source ? " <span class='meta'>src " + esc(st.detection_source) + "</span>" : "")) +
      statCard("Recording", pill(st.recording_enabled === "true" || st.recording_enabled === true) + " <span class='meta'>" + esc(st.record_mode || "") + "</span>") +
      statCard("Snapshots", pill(st.snapshots_enabled === "true" || st.snapshots_enabled === true)) +
      statCard("Vision (YOLO)", visionStatusHTML(st, wk)) +
      statCard("Uptime", esc(fmtUptime(st.uptime))) +
      statCard("Retention", esc(st.retention_days) + " <span class='meta'>days</span>");
    var sbox = document.getElementById("sysStats");
    sbox.innerHTML =
      statCard("Events", esc(sx.events)) +
      statCard("Detections", esc(sx.detections)) +
      statCard("Recordings disk", esc(fmtSize(sx.disk_bytes))) +
      statCard("Screenshots", esc(sx.screenshots)) +
      statCard("Videos", esc(sx.videos)) +
      statCard("S3 pending", esc(sx.s3_pending)) +
      statCard("Locked", esc(sx.locked)) +
      statCard("Buffer RAM", esc(fmtSize(sx.buffer_bytes))) +
      statCard("Data free", esc(fmtSize(sx.data_free)) + " <span class='meta'>/ " + esc(fmtSize(sx.data_total)) + "</span>") +
      statCard("Last event", sx.last_event && +sx.last_event > 0 ? esc(new Date((+sx.last_event) * 1000).toLocaleString()) : "-") +
      statCard("Last detection", sx.last_detection && +sx.last_detection > 0 ? esc(new Date((+sx.last_detection) * 1000).toLocaleString()) : "-");
    var cbox = document.getElementById("sysCams");
    var cams = j.cameras || [];
    if (!cams.length) { cbox.innerHTML = "<div class='meta' style='padding:8px'>No cameras</div>"; return; }
    var h = "<table class='dtable'><thead><tr><th>Camera</th><th>Enabled</th><th>Stream</th><th>Source</th><th>Detections</th><th>RTSP boxes</th><th>Last detection</th></tr></thead><tbody>";
    cams.forEach(function (c) {
      var boxes = c.rtsp_last_box && +c.rtsp_last_box > 0 ? (esc(c.rtsp_boxes) + " <span class='meta'>" + esc(c.rtsp_last_type || "") + " · " + esc(new Date((+c.rtsp_last_box) * 1000).toLocaleTimeString()) + "</span>") : "0";
      h += "<tr><td>" + esc(c.name) + " <span class='mono' style='color:#9da7b3'>" + esc(c.id) + "</span></td><td>" + pill(c.enabled) + "</td><td>" + upPill(c.stream_up) + "</td><td class='mono'>" + esc(c.detection_source || "-") + "</td><td class='mono'>" + esc(c.detections) + "</td><td class='mono'>" + boxes + "</td><td>" + (c.last_detection && +c.last_detection > 0 ? esc(new Date((+c.last_detection) * 1000).toLocaleString()) : "-") + "</td></tr>";
    });
    cbox.innerHTML = h + "</tbody></table>";
  }).catch(function () {});
}
setInterval(function () { if (activeTab === "status") refreshSystemStatus(); }, 5000);
var recView = 'table';
function switchRecView(v){
  recView = v;
  document.querySelectorAll('#rectabs .tab').forEach(function(b){ b.classList.toggle('active', b.dataset.view===v); });
  document.getElementById('dlist').hidden = v!=='table';
  document.getElementById('recGrid').hidden = v!=='grid';
  if(v==='grid') renderRecordingsGrid(_lastRecList);
}
document.querySelectorAll('#rectabs .tab').forEach(function(b){ b.onclick = function(){ switchRecView(b.dataset.view); }; });

// The API now sends a credential-free "host"; parseIp is kept as a fallback
// for older backends that still answer with rtsp_url.
function parseIp(url){ try{ var u=new URL(url); return u.hostname; }catch(e){ var m=(url||'').match(/@([^:\/]+)/); return m?m[1]:''; } }
var _camIpMap=Object.create(null);
function refreshCamMap(){ api("/api/cameras").then(function(list){ _camIpMap=Object.create(null); list.forEach(function(c){ _camIpMap[c.id]=c.host||parseIp(c.rtsp_url||'')||'-'; }); }).catch(function(){}); }
refreshCamMap(); setInterval(refreshCamMap,30000);
function camIpFor(id){ return _camIpMap[id]||'-'; }

// ---------- shared column-filter helpers (events + recordings) ----------
var _filterCams = []; // [{id,name}]
var _knownTypes = ["motion", "person", "vehicle", "pet", "other", "manual-snapshot", "manual-video"];
function refreshFilterCams(){
  api("/api/cameras").then(function(list){
    _filterCams = (list||[]).map(function(c){ return {id: c.id, name: c.name}; });
    buildFilterSelects();
  }).catch(function(){});
}
function dtLocalToUnix(v){
  if(!v) return 0;
  var t = new Date(v);
  if(isNaN(t.getTime())) return 0;
  return Math.floor(t.getTime()/1000);
}
function typeOptions(observed){
  var seen = Object.create(null);
  var out = [];
  _knownTypes.concat(observed||[]).forEach(function(t){
    if(t && !seen[t]){ seen[t]=1; out.push(t); }
  });
  return out.sort();
}
function observedTypes(list, key){
  var s = Object.create(null);
  (list||[]).forEach(function(d){
    var ts = (d.types && d.types.length) ? d.types : [d[key||"type"]];
    (ts||[]).forEach(function(t){ if(t) s[t]=1; });
  });
  return Object.keys(s);
}
function presetRange(name){
  var now = new Date();
  var start, end = now;
  if(name==="today"){ start = new Date(now); start.setHours(0,0,0,0); }
  else if(name==="24h"){ start = new Date(now.getTime()-24*3600*1000); }
  else if(name==="7d"){ start = new Date(now.getTime()-7*24*3600*1000); }
  else return null;
  return {from: start, to: end};
}
function toDtLocal(d){
  if(!d) return "";
  var p = function(n){ return (n<10?"0":"")+n; };
  return d.getFullYear()+"-"+p(d.getMonth()+1)+"-"+p(d.getDate())+"T"+p(d.getHours())+":"+p(d.getMinutes());
}
function buildFilterSelects(){
  // repopulate camera dropdowns wherever they exist
  document.querySelectorAll("select[data-fcam]").forEach(function(sel){
    var cur = sel.value;
    sel.innerHTML = "";
    var all = document.createElement("option"); all.value=""; all.textContent="All cameras"; sel.appendChild(all);
    _filterCams.forEach(function(c){
      var o = document.createElement("option"); o.value=c.id; o.textContent=c.name; sel.appendChild(o);
    });
    sel.value = cur;
  });
}

// ---------- event list filters ----------
var _evtFilter = {camera:"", type:"", from:"", to:"", day:""};
function evtQuery(){
  var p = [];
  if(_evtFilter.camera) p.push("camera="+encodeURIComponent(_evtFilter.camera));
  if(_evtFilter.type) p.push("type="+encodeURIComponent(_evtFilter.type));
  if(_evtFilter.day){
    p.push("date="+encodeURIComponent(_evtFilter.day));
  } else {
    var f = dtLocalToUnix(_evtFilter.from), t = dtLocalToUnix(_evtFilter.to);
    // datetime-local "to" is inclusive of that minute: extend to end of minute
    if(t) t += 59;
    if(f) p.push("from="+f);
    if(t) p.push("to="+t);
  }
  return "/api/onvif_events"+(p.length?"?"+p.join("&"):"");
}
function refreshEvents(){ api(evtQuery()).then(renderEvents).catch(function(){}); }
var _eventsGroupBy=false;
var _lastEventsList=[];
function buildEvtFilterBar(){
  // Slim range bar: camera/type/single-day live in the per-column filter row
  // below; only the From/To range + presets stay here.
  var bar=document.createElement("div"); bar.className="filters";
  // range from/to
  var fFrom=document.createElement("label"); fFrom.className="fld"; fFrom.textContent="From";
  var inFrom=document.createElement("input"); inFrom.type="datetime-local"; inFrom.value=_evtFilter.from||"";
  inFrom.onchange=function(){ _evtFilter.from=inFrom.value; _evtFilter.day=""; refreshEvents(); };
  fFrom.appendChild(inFrom); bar.appendChild(fFrom);
  var fTo=document.createElement("label"); fTo.className="fld"; fTo.textContent="To";
  var inTo=document.createElement("input"); inTo.type="datetime-local"; inTo.value=_evtFilter.to||"";
  inTo.onchange=function(){ _evtFilter.to=inTo.value; _evtFilter.day=""; refreshEvents(); };
  fTo.appendChild(inTo); bar.appendChild(fTo);
  // presets + clear
  var presets=document.createElement("div"); presets.className="preset-row";
  [["Today","today"],["Last 24h","24h"],["Last 7d","7d"]].forEach(function(pr){
    var b=document.createElement("button"); b.className="btn-sm"; b.textContent=pr[0];
    b.onclick=function(){ var r=presetRange(pr[1]); if(r){ _evtFilter.day=""; _evtFilter.from=toDtLocal(r.from); _evtFilter.to=toDtLocal(r.to); refreshEvents(); } };
    presets.appendChild(b);
  });
  var clr=document.createElement("button"); clr.className="btn-sm"; clr.textContent="✕ Clear";
  clr.onclick=function(){ _evtFilter={camera:"",type:"",from:"",to:"",day:""}; refreshEvents(); };
  presets.appendChild(clr);
  bar.appendChild(presets);
  return bar;
}
function buildEvtColumnFilterRow(ncols){
  // per-column filter row directly under the headers: date | camera | type
  var tr=document.createElement("tr"); tr.className="filters-row";
  var tdD=document.createElement("th");
  var inD=document.createElement("input"); inD.type="date"; inD.title="Filter by date"; inD.value=_evtFilter.day||"";
  inD.onchange=function(){ _evtFilter.day=inD.value; if(inD.value){ _evtFilter.from=""; _evtFilter.to=""; } refreshEvents(); };
  tdD.appendChild(inD); tr.appendChild(tdD);
  var tdC=document.createElement("th");
  var selC=document.createElement("select"); selC.title="Filter by camera name";
  var all=document.createElement("option"); all.value=""; all.textContent="All"; selC.appendChild(all);
  _filterCams.forEach(function(c){ var o=document.createElement("option"); o.value=c.id; o.textContent=c.name; selC.appendChild(o); });
  selC.value=_evtFilter.camera||"";
  selC.onchange=function(){ _evtFilter.camera=selC.value; refreshEvents(); };
  tdC.appendChild(selC); tr.appendChild(tdC);
  var tdT=document.createElement("th");
  var selT=document.createElement("select"); selT.title="Filter by type";
  var allt=document.createElement("option"); allt.value=""; allt.textContent="All"; selT.appendChild(allt);
  typeOptions(observedTypes(_lastEventsList)).forEach(function(t){ var o=document.createElement("option"); o.value=t; o.textContent=detectionLabel(t); selT.appendChild(o); });
  selT.value=_evtFilter.type||"";
  selT.onchange=function(){ _evtFilter.type=selT.value; refreshEvents(); };
  tdT.appendChild(selT); tr.appendChild(tdT);
  for(var i=3;i<ncols;i++){ var th=document.createElement("th"); tr.appendChild(th); }
  return tr;
}
function renderEvents(list){
  _lastEventsList=list||[];
  var box = document.getElementById("elist");
  if(!box) return;
  box.innerHTML="";
  // toolbar with group checkbox
  var toolbar=document.createElement("div");
  toolbar.className="toolbar";
  toolbar.style.padding="8px 0";
  var meta=document.createElement("span"); meta.className="meta"; meta.textContent=list.length+" raw ONVIF event(s) • each motion line from camera; recordings merge events within post window";
  var label=document.createElement("label"); label.className="btn-sm"; label.style.display="flex"; label.style.alignItems="center"; label.style.gap="6px"; label.style.cursor="pointer"; label.style.userSelect="none";
  var chk=document.createElement("input"); chk.type="checkbox"; chk.checked=_eventsGroupBy; chk.onchange=function(e){ _eventsGroupBy=e.target.checked; renderEvents(_lastEventsList); };
  label.appendChild(chk); label.appendChild(document.createTextNode("Group by Group ID"));
  var refreshBtn=document.createElement("button"); refreshBtn.className="btn-sm"; refreshBtn.textContent="↻ Refresh"; refreshBtn.onclick=function(){ refreshEvents(); };
  var delAllBtn=document.createElement("button"); delAllBtn.className="del-btn"; delAllBtn.textContent="Delete all"; delAllBtn.title="Delete ALL raw ONVIF events (recordings untouched)";
  delAllBtn.onclick=function(){ if(!confirm("Delete ALL raw ONVIF events? (recordings untouched)")) return; fetch("/api/onvif_events",{method:"DELETE",headers:headers()}).then(function(r){ if(!r.ok) throw new Error("delete failed "+r.status); refreshEvents(); }).catch(function(err){ toast("Delete failed: "+err.message, "error"); }); };
  var expBtn=document.createElement("button"); expBtn.className="btn-sm"; expBtn.textContent="⬇ Export CSV"; expBtn.title="Export filtered events table as CSV";
  expBtn.onclick=function(){
    // Rebuild display rows for grouped mode so the CSV matches the table.
    if(_eventsGroupBy){
      var map=Object.create(null); var rows=[];
      _lastEventsList.forEach(function(d){
        var gid=d.detection_id||d.group_id||d.groupId||'';
        if(!gid||gid==='-'){ rows.push({d:d,_gid:'-',_key:d.id,_count:1,_ts:d.created_at||d.started_at,_types:[d.type]}); }
        else if(!map[gid]){ map[gid]={d:d,_gid:gid,_key:gid,_count:1,_ts:d.created_at||d.started_at,_types:[d.type]}; rows.push(map[gid]); }
        else { map[gid]._count+=1; if(d.type && map[gid]._types.indexOf(d.type)<0) map[gid]._types.push(d.type); var cur=d.created_at||d.started_at; if(cur && map[gid]._ts && cur<map[gid]._ts) map[gid]._ts=cur; }
      });
      exportGroupedEventsCSV(rows, expBtn);
    } else {
      exportQueryToCSV(evtQuery(), "events", expBtn);
    }
  };
  toolbar.appendChild(meta); toolbar.appendChild(label); toolbar.appendChild(refreshBtn); toolbar.appendChild(expBtn); toolbar.appendChild(delAllBtn);
  box.appendChild(toolbar);
  // column filter bar: camera name, type, date / range
  box.appendChild(buildEvtFilterBar());
  var table=document.createElement("table"); table.className="dtable";
  var thead=document.createElement("thead");
  if(_eventsGroupBy){ thead.innerHTML="<tr><th>Local Date Time</th><th>Camera</th><th>Type</th><th>Group ID</th><th>Count</th><th>Actions</th></tr>"; }
  else { thead.innerHTML="<tr><th>Local Date Time</th><th>Camera</th><th>Type</th><th>Group ID</th><th>Actions</th></tr>"; }
  thead.appendChild(buildEvtColumnFilterRow(_eventsGroupBy?6:5));
  table.appendChild(thead);
  var tbody=document.createElement("tbody");
  // build display rows
  var displayRows=[];
  if(_eventsGroupBy){
    var map=Object.create(null);
    var order=[];
    list.forEach(function(d){
      var gid=d.detection_id||d.group_id||d.groupId||'';
      if(!gid||gid==='-'){
        displayRows.push({d:d,_gid:'-',_key:d.id,_count:1,_ts:d.created_at||d.started_at,_types:[d.type]});
      } else {
        if(!map[gid]){ map[gid]={d:d,_gid:gid,_key:gid,_count:1,_ts:d.created_at||d.started_at,_types:[d.type]}; order.push(gid); displayRows.push(map[gid]); }
        else { map[gid]._count+=1; if(d.type && map[gid]._types.indexOf(d.type)<0) map[gid]._types.push(d.type); var cur=d.created_at||d.started_at; if(cur && map[gid]._ts && cur<map[gid]._ts) map[gid]._ts=cur; }
      }
    });
  } else {
    list.forEach(function(d){ displayRows.push({d:d,_gid:d.detection_id||d.group_id||d.groupId||'-',_key:d.id,_count:1,_ts:d.created_at||d.started_at}); });
  }
  if(displayRows.length===0){
    var tr=document.createElement("tr"); var td=document.createElement("td"); td.colSpan=_eventsGroupBy?6:5; td.className="meta"; td.style.textAlign="center"; td.style.padding="16px"; td.textContent="No events"; tr.appendChild(td); tbody.appendChild(tr);
  } else {
    displayRows.forEach(function(row){
      var d=row.d;
      var ts=row._ts;
      var gid=row._gid;
      var hasGroup=gid!=='-'&&gid!=='';
      var tr=document.createElement("tr");
      if(hasGroup){ tr.style.cursor='pointer'; tr.title='Go to recording '+gid.slice(0,8); tr.onclick=function(){ switchTab('recordings'); api("/api/detections").then(function(all){ var found=null; for(var i=0;i<all.length;i++) if(all[i].id===gid){found=all[i];break;} if(found) showDetail(found,'snapshots'); }).catch(function(){}); }; }
      var tdTime=document.createElement("td"); tdTime.textContent=ts?new Date(ts*1000).toLocaleString():"-";
      var tdName=document.createElement("td"); tdName.textContent=d.camera_name;
      var tdType=document.createElement("td"); tdType.innerHTML=_eventsGroupBy?detectionBadgesHTML(row._types||[d.type]):("<span class='badge'>"+esc(detectionLabel(d.type))+"</span>");
      var tdGid=document.createElement("td"); tdGid.textContent=hasGroup?gid.slice(0,8):'—'; tdGid.className="mono"; tdGid.title=gid;
      tr.appendChild(tdTime); tr.appendChild(tdName); tr.appendChild(tdType); tr.appendChild(tdGid);
      if(_eventsGroupBy){ var tdCount=document.createElement("td"); tdCount.textContent=row._count; tdCount.className="mono"; tr.appendChild(tdCount); }
      var tdAct=document.createElement("td"); tdAct.className="actions";
      var del=document.createElement("button"); del.type="button"; del.className="del-btn"; del.textContent="Delete";
      del.title=hasGroup?"Delete all events of this group (recording untouched)":"Delete this event";
      del.onclick=function(e){
        e.stopPropagation();
        if(!confirm(hasGroup?("Delete "+row._count+" event(s) of this group? (recording untouched)"):("Delete this event?"))) return;
        var url=hasGroup?("/api/onvif_events?detection_id="+encodeURIComponent(gid)):("/api/onvif_events/"+encodeURIComponent(d.id));
        fetch(url,{method:"DELETE",headers:headers()}).then(function(r){ if(!r.ok) throw new Error("delete failed "+r.status); refreshEvents(); }).catch(function(err){ toast("Delete failed: "+err.message, "error"); });
      };
      tdAct.appendChild(del); tr.appendChild(tdAct);
      tbody.appendChild(tr);
    });
  }
  table.appendChild(tbody); box.appendChild(table);
}
// ---------- recordings filters (shared table + grid) ----------
var _recFilter = {camera:"", type:"", from:"", to:"", day:"", label:"", has_vision:""};
function recQuery(){
  var p = [];
  if(_recFilter.camera) p.push("camera="+encodeURIComponent(_recFilter.camera));
  if(_recFilter.type) p.push("type="+encodeURIComponent(_recFilter.type));
  if(_recFilter.label) p.push("label="+encodeURIComponent(_recFilter.label));
  if(_recFilter.has_vision) p.push("has_vision=1");
  if(_recFilter.day){
    p.push("date="+encodeURIComponent(_recFilter.day));
  } else {
    var f = dtLocalToUnix(_recFilter.from), t = dtLocalToUnix(_recFilter.to);
    if(t) t += 59;
    if(f) p.push("from="+f);
    if(t) p.push("to="+t);
  }
  return "/api/detections"+(p.length?"?"+p.join("&"):"");
}
function refreshRecordings(){ api(recQuery()).then(function(l){ renderDetections(l); renderRecordingsGrid(l); }).catch(function(){}); }
var _lastRecList=[];
function buildRecFilterBar(full){
  // Table view has a per-column filter row (date/camera/type), so it only
  // needs the range bar. Grid view has no columns, so it gets full filters.
  var bar=document.createElement("div"); bar.className="filters";
  if(full){
    var fCam=document.createElement("label"); fCam.className="fld"; fCam.textContent="Camera";
    var selC=document.createElement("select"); selC.setAttribute("data-fcam","rec");
    var all=document.createElement("option"); all.value=""; all.textContent="All cameras"; selC.appendChild(all);
    _filterCams.forEach(function(c){ var o=document.createElement("option"); o.value=c.id; o.textContent=c.name; selC.appendChild(o); });
    selC.value=_recFilter.camera||"";
    selC.onchange=function(){ _recFilter.camera=selC.value; refreshRecordings(); };
    fCam.appendChild(selC); bar.appendChild(fCam);
    var fTy=document.createElement("label"); fTy.className="fld"; fTy.textContent="Type";
    var selT=document.createElement("select");
    var allt=document.createElement("option"); allt.value=""; allt.textContent="All types"; selT.appendChild(allt);
    typeOptions(observedTypes(_lastRecList)).forEach(function(t){ var o=document.createElement("option"); o.value=t; o.textContent=detectionLabel(t); selT.appendChild(o); });
    selT.value=_recFilter.type||"";
    selT.onchange=function(){ _recFilter.type=selT.value; refreshRecordings(); };
    fTy.appendChild(selT); bar.appendChild(fTy);
    var fDay=document.createElement("label"); fDay.className="fld"; fDay.textContent="Date";
    var inDay=document.createElement("input"); inDay.type="date"; inDay.value=_recFilter.day||"";
    inDay.onchange=function(){ _recFilter.day=inDay.value; if(inDay.value){ _recFilter.from=""; _recFilter.to=""; } refreshRecordings(); };
    fDay.appendChild(inDay); bar.appendChild(fDay);
    var fLab=document.createElement("label"); fLab.className="fld"; fLab.textContent="YOLO label";
    var inLab=document.createElement("input"); inLab.type="text"; inLab.placeholder="person…"; inLab.value=_recFilter.label||"";
    inLab.onchange=function(){ _recFilter.label=inLab.value.trim(); refreshRecordings(); };
    fLab.appendChild(inLab); bar.appendChild(fLab);
    var fVis=document.createElement("label"); fVis.className="fld"; fVis.style.display="inline-flex"; fVis.style.alignItems="center"; fVis.style.gap="4px";
    var chkV=document.createElement("input"); chkV.type="checkbox"; chkV.checked=!!_recFilter.has_vision;
    chkV.onchange=function(){ _recFilter.has_vision=chkV.checked?"1":""; refreshRecordings(); };
    fVis.appendChild(chkV); fVis.appendChild(document.createTextNode("👁 analysés")); bar.appendChild(fVis);
  }
  var fFrom=document.createElement("label"); fFrom.className="fld"; fFrom.textContent="From";
  var inFrom=document.createElement("input"); inFrom.type="datetime-local"; inFrom.value=_recFilter.from||"";
  inFrom.onchange=function(){ _recFilter.from=inFrom.value; _recFilter.day=""; refreshRecordings(); };
  fFrom.appendChild(inFrom); bar.appendChild(fFrom);
  var fTo=document.createElement("label"); fTo.className="fld"; fTo.textContent="To";
  var inTo=document.createElement("input"); inTo.type="datetime-local"; inTo.value=_recFilter.to||"";
  inTo.onchange=function(){ _recFilter.to=inTo.value; _recFilter.day=""; refreshRecordings(); };
  fTo.appendChild(inTo); bar.appendChild(fTo);
  var presets=document.createElement("div"); presets.className="preset-row";
  [["Today","today"],["Last 24h","24h"],["Last 7d","7d"]].forEach(function(pr){
    var b=document.createElement("button"); b.className="btn-sm"; b.textContent=pr[0];
    b.onclick=function(){ var r=presetRange(pr[1]); if(r){ _recFilter.day=""; _recFilter.from=toDtLocal(r.from); _recFilter.to=toDtLocal(r.to); refreshRecordings(); } };
    presets.appendChild(b);
  });
  var clr=document.createElement("button"); clr.className="btn-sm"; clr.textContent="✕ Clear";
  clr.onclick=function(){ _recFilter={camera:"",type:"",from:"",to:"",day:"",label:"",has_vision:""}; refreshRecordings(); };
  presets.appendChild(clr);
  bar.appendChild(presets);
  return bar;
}
function buildRecColumnFilterRow(){
  var tr=document.createElement("tr"); tr.className="filters-row";
  var tdD=document.createElement("th");
  var inD=document.createElement("input"); inD.type="date"; inD.title="Filter by date"; inD.value=_recFilter.day||"";
  inD.onchange=function(){ _recFilter.day=inD.value; if(inD.value){ _recFilter.from=""; _recFilter.to=""; } refreshRecordings(); };
  tdD.appendChild(inD); tr.appendChild(tdD);
  // Duration + Size + S3 columns have no filter
  tr.appendChild(document.createElement("th")); tr.appendChild(document.createElement("th")); tr.appendChild(document.createElement("th"));
  var tdT=document.createElement("th");
  var selT=document.createElement("select"); selT.title="Filter by type";
  var allt=document.createElement("option"); allt.value=""; allt.textContent="All"; selT.appendChild(allt);
  typeOptions(observedTypes(_lastRecList)).forEach(function(t){ var o=document.createElement("option"); o.value=t; o.textContent=detectionLabel(t); selT.appendChild(o); });
  selT.value=_recFilter.type||"";
  selT.onchange=function(){ _recFilter.type=selT.value; refreshRecordings(); };
  tdT.appendChild(selT); tr.appendChild(tdT);
  var tdC=document.createElement("th");
  var selC=document.createElement("select"); selC.title="Filter by camera name";
  var all=document.createElement("option"); all.value=""; all.textContent="All"; selC.appendChild(all);
  _filterCams.forEach(function(c){ var o=document.createElement("option"); o.value=c.id; o.textContent=c.name; selC.appendChild(o); });
  selC.value=_recFilter.camera||"";
  selC.onchange=function(){ _recFilter.camera=selC.value; refreshRecordings(); };
  tdC.appendChild(selC); tr.appendChild(tdC);
  tr.appendChild(document.createElement("th")); tr.appendChild(document.createElement("th"));
  return tr;
}
function renderRecordingsGrid(list){
  _lastRecList=list||[];
  var box=document.getElementById("recGrid");
  if(!box) return;
  box.innerHTML="";
  // Only build cards (and their <img> elements) when the grid is visible:
  // rendering 500 hidden thumbnails on every refresh caused a snapshot/disk
  // and box-render storm just by opening the web page.
  if(recView!=='grid') return;
  box.appendChild(buildRecFilterBar(true));
  if(list.length===0){
    // Append, don't reset innerHTML: that would wipe the filter bar rendered
    // just above and leave the user unable to clear a filter that matched
    // nothing.
    var empty=document.createElement("div");
    empty.className="meta"; empty.style.cssText="padding:16px;text-align:center";
    empty.textContent="No recordings";
    box.appendChild(empty);
    return;
  }
  list.forEach(function(d){
    var duration=d.duration!=null?d.duration:(d.ended_at-d.started_at);
    var card=document.createElement("div"); card.className="rec-card";
    var thumb=document.createElement("div"); thumb.className="rec-thumb"; thumb.onclick=function(){ showDetail(d,'snapshots'); };
    var img;
    if (d.snapshot) { img=document.createElement("img"); _imgFallback(img, "snapshot manquant"); img.src=visionBoxesURL(d, d.snapshot, 480); img.alt="snapshot"; img.loading="lazy"; }
    else { img=document.createElement("div"); img.className="meta"; img.style.cssText="display:flex;align-items:center;justify-content:center;height:100%;min-height:120px"; img.textContent="no snapshot"; }
    var badge=document.createElement("span"); badge.className="rec-badge"; badge.style.cssText="display:inline-flex;flex-wrap:wrap;gap:4px;max-width:calc(100% - 16px)";
    sortDetectionTypes(detectionTypeList(d)).forEach(function(t){ var b=document.createElement("span"); b.className="badge"; b.textContent=detectionLabel(t); badge.appendChild(b); });
    thumb.appendChild(img); thumb.appendChild(badge);
    var vb=visionBadgeText(d);
    if(vb){ var vbadge=document.createElement("span"); vbadge.className="rec-badge badge vision-badge"; vbadge.textContent=vb; vbadge.title=(d.vision&&d.vision.error)||"YOLO"; thumb.appendChild(vbadge); }
    var bb=bboxBadgeText(d);
    if(bb){ var bbadge=document.createElement("span"); bbadge.className="rec-badge badge"; bbadge.textContent=bb; bbadge.title="RTSP SEI boxes"; thumb.appendChild(bbadge); }
    var info=document.createElement("div"); info.className="rec-info";
    var title=document.createElement("div"); title.className="rec-title"; title.innerHTML=esc(d.camera_name)+" <span class='mono' style='color:#9da7b3'>"+esc(d.camera_id)+"</span>";
    var meta1=document.createElement("div"); meta1.className="meta"; meta1.textContent=new Date(d.started_at*1000).toLocaleString()+" • "+fmtDuration(duration)+" • "+fmtSize(d.size);
    var meta2=document.createElement("div"); meta2.className="meta"; meta2.textContent=(d.snapshots?d.snapshots.length:1)+" snapshots • "+(d.clip?"video":(d.clip_pending?"⏳ assemblage…":"no video"))+(d.locked?" • 🔒 verrouillé":"")+(d.uploaded?" • ☁️ S3":"")+(vb?" • "+vb:"")+(bb?" • "+bb:"");
    var actions=document.createElement("div"); actions.className="rec-actions";
    var btnSnap=document.createElement("button"); btnSnap.className="btn-sm"; btnSnap.textContent="Snapshots"; btnSnap.onclick=function(e){ e.stopPropagation(); showDetail(d,'snapshots'); };
    var btnVid=document.createElement("button"); btnVid.className="btn-sm"; btnVid.textContent=d.clip?"Video":(d.clip_pending?"⏳ assemblage…":"Video"); btnVid.disabled=!d.clip; btnVid.title=d.clip?"":(d.clip_pending?"Clip en cours d'assemblage":"Pas de vidéo pour cet enregistrement"); btnVid.onclick=function(e){ e.stopPropagation(); showDetail(d,'video'); };
    var lab=document.createElement("label"); lab.className="btn-sm"; lab.style.display="inline-flex"; lab.style.alignItems="center"; lab.style.gap="4px"; lab.style.cursor="pointer"; lab.title="Verrouiller — exclu du prune"; var chk2=document.createElement("input"); chk2.type="checkbox"; chk2.checked=!!d.locked; chk2.onchange=function(){
      var next=!d.locked;
      chk2.disabled=true;
      fetch("/api/detections/"+encodeURIComponent(d.id)+"/lock",{method:"POST",headers:headers(),body:JSON.stringify({locked:next})})
        .then(function(r){ if(!r.ok) throw new Error("lock failed "+r.status); refreshRecordings(); })
        .catch(function(e){ chk2.checked=!next; toast("Lock failed: "+e.message, "error"); })
        .finally(function(){ chk2.disabled=false; });
    }; lab.appendChild(chk2); lab.appendChild(document.createTextNode(" 🔒"));
    var del=document.createElement("button"); del.className="del-btn"; del.textContent="Delete"; del.onclick=function(e){
      e.stopPropagation();
      if(!confirm("Delete this recording?")) return;
      del.disabled=true;
      fetch("/api/detections/"+encodeURIComponent(d.id),{method:"DELETE",headers:headers()})
        .then(function(r){ if(!r.ok) throw new Error("delete failed "+r.status); refreshRecordings(); })
        .catch(function(err){ toast("Delete failed: "+err.message, "error"); })
        .finally(function(){ del.disabled=false; });
    };
    actions.appendChild(btnSnap); actions.appendChild(btnVid); actions.appendChild(lab); actions.appendChild(del);
    info.appendChild(title); info.appendChild(meta1); info.appendChild(meta2); info.appendChild(actions);
    card.appendChild(thumb); card.appendChild(info); box.appendChild(card);
  });
}

document.getElementById("s3retry").onclick = function () {
  var b = document.getElementById("s3retry");
  b.disabled = true;
  b.textContent = "… uploading";
  fetch("/api/s3/retry", { method: "POST", headers: headers() })
    .then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
    .then(function (res) {
      if (!res.ok) throw new Error((res.body && res.body.error) || ("retry failed " + res.status));
      var q = res.body.queued || 0;
      toast(q > 0 ? ("S3 retry started for " + q + " recording(s).") : "Nothing pending S3 upload.", q > 0 ? "ok" : "info");
      refreshRecordings();
      if (activeTab === "status") refreshSystemStatus();
    })
    .catch(function (e) { toast("S3 retry failed: " + e.message, "error"); })
    .finally(function () { b.disabled = false; b.textContent = "☁️ Retry S3 upload"; });
};

document.getElementById("delAll").onclick = function () {
  if (!confirm("Delete ALL detections (snapshots + clips)?")) return;
  fetch("/api/detections", { method: "DELETE", headers: headers() })
    .then(function (r) { if (!r.ok) throw new Error("delete failed " + r.status); refreshRecordings(); })
    .catch(function (err) { toast("Delete failed: " + err.message, "error"); });
};

document.getElementById("expRec").onclick = function () {
  exportQueryToCSV(recQuery(), "recordings", document.getElementById("expRec"));
};

function fmtSize(bytes) {
  if (bytes == null || isNaN(bytes)) return '-';
  if (bytes === 0) return '0 B';
  var units = ['B','KB','MB','GB'];
  var i = 0; var v = bytes;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
}
function fmtDuration(sec) {
  if (sec == null || isNaN(sec)) return '-';
  if (sec < 60) return sec + 's';
  var m = Math.floor(sec/60); var s = sec%60;
  return m + 'm ' + s + 's';
}
function detectionLabel(t) {
  if (!t) return 'unknown';
  var m = {person:'person', vehicle:'car', car:'car', pet:'pet', motion:'motion', other:'other', 'manual-snapshot':'snapshot', 'manual-video':'manual', manual:'manual'};
  return m[t] || t;
}

// ---------- merged camera tags (detection.types) ----------
// Specific tags first, motion last; ties keep the server's first-seen order.
var _detTypeRank = {person:3, vehicle:3, pet:3, other:2, motion:1};
function detectionTypeList(d){
  d = d || {};
  var raw = d.types || [];
  if(!raw.length && d.type) raw = [d.type];
  var seen = Object.create(null), out = [];
  function add(t){ t = String(t||"").trim().toLowerCase(); if(t && !seen[t]){ seen[t]=1; out.push(t); } }
  if(d.type) add(d.type);
  raw.forEach(add);
  return out;
}
function sortDetectionTypes(types){
  return (types||[]).slice().sort(function(a,b){ return (_detTypeRank[b]||0)-(_detTypeRank[a]||0); });
}
function detectionBadgesHTML(types){
  return sortDetectionTypes(types).map(function(t){ return "<span class='badge'>"+esc(detectionLabel(t))+"</span>"; }).join(" ");
}
function detectionTagsText(types){
  return sortDetectionTypes(types).map(detectionLabel).join(" + ");
}
function detectionTags(d){ return detectionTagsText(detectionTypeList(d)); }

// ---------- CSV export (recordings + events, filtered rows) ----------
// Neutralizes spreadsheet formula injection the same way the backend does:
// a cell starting with =, +, - or @ is evaluated as a formula by Excel.
function csvEscape(v) {
  var s = v == null ? "" : String(v);
  if (s && /[=+\-@\t\r]/.test(s.charAt(0))) s = "'" + s;
  if (/[",\n\r]/.test(s)) return '"' + s.replace(/"/g, '""') + '"';
  return s;
}
function csvName(prefix) {
  var p = function (n) { return (n < 10 ? "0" : "") + n; };
  var d = new Date();
  return prefix + "-" + d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) + "-" + p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds()) + ".csv";
}
function saveBlob(filename, blob) {
  var a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
}
// Download the backend CSV for the current filter (uncapped server-side).
function exportQueryToCSV(baseQuery, prefix, btn) {
  var url = baseQuery + (baseQuery.indexOf("?") >= 0 ? "&" : "?") + "format=csv";
  var label = btn ? btn.textContent : "";
  if (btn) { btn.disabled = true; btn.textContent = "… exporting"; }
  fetch(url, { headers: headers() })
    .then(function (r) { if (!r.ok) throw new Error("export failed " + r.status); return r.blob(); })
    .then(function (b) { saveBlob(csvName(prefix), b); })
    .catch(function (e) { toast("CSV export failed: " + e.message, "error"); })
    .finally(function () { if (btn) { btn.disabled = false; btn.textContent = label; } });
}
// Grouped events view is a client-side aggregation: export displayed rows locally.
function exportGroupedEventsCSV(displayRows, btn) {
  var header = ["local_date_time", "camera", "type", "types", "group_id", "count"];
  var lines = [header.map(csvEscape).join(",")];
  displayRows.forEach(function (row) {
    var d = row.d || {};
    var ts = row._ts ? new Date(row._ts * 1000).toLocaleString() : "";
    lines.push([ts, d.camera_name || "", d.type || "", sortDetectionTypes(row._types || [d.type]).join(","), row._gid === "-" ? "" : row._gid, row._count].map(csvEscape).join(","));
  });
  saveBlob(csvName("events-grouped"), new Blob(["\uFEFF" + lines.join("\r\n") + "\r\n"], { type: "text/csv;charset=utf-8" }));
}

function renderDetections(list) {
  _lastRecList=list||[];
  var box = document.getElementById("dlist");
  box.innerHTML = "";
  document.getElementById("detCount").textContent = list.length + " detection(s): ";
  if(recView==='table'){ box.appendChild(buildRecFilterBar(false)); }
  var wrap = document.createElement("div");
  wrap.className = "dtable-wrap";
  var table = document.createElement("table");
  table.className = "dtable";
  var thead = document.createElement("thead");
  thead.innerHTML = "<tr><th>Local Date Time</th><th>Duration</th><th>Size</th><th>S3</th><th>Detection</th><th>Camera</th><th>Group ID</th><th>Actions</th></tr>";
  thead.appendChild(buildRecColumnFilterRow());
  table.appendChild(thead);
  var tbody = document.createElement("tbody");
  if (list.length === 0) {
    var tr = document.createElement("tr");
    var td = document.createElement("td");
    td.colSpan = 8;
    td.className = "meta";
    td.style.textAlign = "center";
    td.style.padding = "16px";
    td.textContent = "No detections";
    tr.appendChild(td);
    tbody.appendChild(tr);
  } else {
    list.forEach(function (d) {
      var tr = document.createElement("tr");
      tr.style.cursor = "pointer";
      tr.onclick = function(){ showDetail(d, 'snapshots'); };
      var duration = d.duration != null ? d.duration : (d.ended_at && d.started_at ? d.ended_at - d.started_at : 0);
      var tdTime = document.createElement("td");
      tdTime.textContent = new Date(d.started_at * 1000).toLocaleString();
      var tdDur = document.createElement("td");
      tdDur.textContent = fmtDuration(duration);
      var tdSize = document.createElement("td");
      tdSize.textContent = fmtSize(d.size);
      var tdS3 = document.createElement("td");
      if (d.uploaded) { tdS3.textContent = "☁️"; tdS3.title = "Uploaded to S3"; }
      else { tdS3.textContent = "—"; tdS3.className = "meta"; tdS3.title = "Not uploaded to S3"; }
      var tdType = document.createElement("td");
      var vb2 = visionBadgeText(d);
      tdType.innerHTML = detectionBadgesHTML(detectionTypeList(d)) + (vb2 ? " <span class='badge vision-badge' title='"+esc((d.vision&&d.vision.error)||"YOLO")+"'>" + esc(vb2) + "</span>" : "");
      var tdCam = document.createElement("td");
      tdCam.textContent = d.camera_name || d.camera_id || "";
      var tdGid = document.createElement("td"); tdGid.textContent=(d.id||'').slice(0,8); tdGid.className="mono"; tdGid.title=d.id||'';
      var tdAct = document.createElement("td");
      tdAct.className = "actions";
      var btnSnap = document.createElement("button");
      btnSnap.className = "btn-sm";
      btnSnap.textContent = "Snapshots";
      btnSnap.onclick = function (e) { e.stopPropagation(); showDetail(d, 'snapshots'); };
      var btnVid = document.createElement("button");
      btnVid.className = "btn-sm";
      btnVid.textContent = d.clip ? "Video" : (d.clip_pending ? "⏳ assemblage…" : "Video");
      btnVid.title = d.clip ? "" : (d.clip_pending ? "Clip en cours d'assemblage" : "Pas de vidéo pour cet enregistrement");
      btnVid.disabled = !d.clip;
      btnVid.onclick = function (e) { e.stopPropagation(); showDetail(d, 'video'); };
      var btnTl = document.createElement("button");
      btnTl.className = "btn-sm";
      btnTl.textContent = "Timeline";
      btnTl.title = "Chronological view: events, YOLO snapshots, clip/S3 status";
      btnTl.onclick = function (e) { e.stopPropagation(); showDetail(d, 'timeline'); };
      var del = document.createElement("button");
      del.type = "button";
      del.className = "del-btn";
      del.style.marginLeft = "6px";
      del.textContent = "Delete";
      del.onclick = function (e) {
        e.stopPropagation();
        if (!confirm("Delete this detection (snapshots + clip)?")) return;
        fetch("/api/detections/" + encodeURIComponent(d.id), { method: "DELETE", headers: headers() })
          .then(function (r) { if (!r.ok) throw new Error("delete failed " + r.status); refreshRecordings(); })
          .catch(function (err) { toast("Delete failed: " + err.message, "error"); });
      };
      var lockBtn = document.createElement("button");
      lockBtn.className = "btn-sm";
      lockBtn.textContent = d.locked ? "🔒" : "🔓";
      lockBtn.title = d.locked ? "Verrouillé — exclu du prune (cliquer pour déverrouiller)" : "Verrouiller — exclure du prune";
      if(d.locked){ lockBtn.style.background="#2e7d32"; lockBtn.style.color="#fff"; }
      lockBtn.onclick = function(e){
        e.stopPropagation();
        var next=!d.locked;
        lockBtn.disabled=true;
        fetch("/api/detections/"+encodeURIComponent(d.id)+"/lock",{method:"POST",headers:headers(),body:JSON.stringify({locked:next})})
          .then(function(r){ if(!r.ok) throw new Error("lock failed "+r.status); refreshRecordings(); })
          .catch(function(err){ toast("Lock failed: "+err.message, "error"); })
          .finally(function(){ lockBtn.disabled=false; });
      };
      tdAct.appendChild(btnSnap);
      tdAct.appendChild(btnVid);
      tdAct.appendChild(btnTl);
      tdAct.appendChild(lockBtn);
      tdAct.appendChild(del);
      tr.appendChild(tdTime);
      tr.appendChild(tdDur);
      tr.appendChild(tdSize);
      tr.appendChild(tdS3);
      tr.appendChild(tdType);
      tr.appendChild(tdCam);
      tr.appendChild(tdGid);
      tr.appendChild(tdAct);
      tbody.appendChild(tr);
    });
  }
  table.appendChild(tbody);
  wrap.appendChild(table);
  box.appendChild(wrap);
}

var curDetail = null;
var curTab = 'snapshots';

// ---------- detection detail timeline ----------
// One endpoint carries everything: detection + raw events + YOLO per snapshot.
// Entries are rendered chronologically with offsets from the detection start.
function _timelineEl() {
  var el = document.getElementById("dtimeline");
  if (!el) {
    el = document.createElement("div");
    el.id = "dtimeline";
    el.className = "timeline";
    el.hidden = true;
    var detail = document.getElementById("detail");
    var meta = document.getElementById("dmeta");
    if (meta && meta.parentNode === detail) detail.insertBefore(el, meta);
    else detail.appendChild(el);
  }
  return el;
}

function _fmtOffset(sec) {
  sec = Math.max(0, Math.round(sec));
  var m = Math.floor(sec / 60), ss = sec % 60;
  return "+" + (m ? m + "m" : "") + ss + "s";
}

function _timelineRow(entry, base) {
  var row = document.createElement("div");
  row.className = "tl-row " + (entry.cls || "");
  var when = document.createElement("span");
  when.className = "tl-when";
  when.textContent = entry.t ? new Date(entry.t * 1000).toLocaleTimeString() : "—";
  if (entry.t && base) {
    var off = document.createElement("span");
    off.className = "meta";
    off.textContent = " " + _fmtOffset(entry.t - base);
    when.appendChild(off);
  }
  var icon = document.createElement("span");
  icon.className = "tl-icon";
  icon.textContent = entry.icon || "•";
  var text = document.createElement("span");
  text.className = "tl-text";
  text.textContent = entry.text;
  row.appendChild(when);
  row.appendChild(icon);
  row.appendChild(text);
  if (entry.url) {
    var btn = document.createElement("button");
    btn.className = "btn-sm";
    btn.textContent = "snapshot";
    btn.onclick = function () { showLightbox(entry.url); };
    row.appendChild(btn);
  }
  return row;
}

function renderTimeline(d, box, tl) {
  box.innerHTML = "";
  tl = tl || {};
  var det = tl.detection || d;
  var entries = [];
  if (det.started_at) entries.push({t: +det.started_at, icon: "▶", cls: "tl-start", text: "Detection start — " + detectionTags(det)});
  (tl.events || []).forEach(function (e) {
    entries.push({
      t: +e.created_at || +e.started_at, icon: "•", cls: "tl-event",
      text: "Event: " + detectionLabel(e.type) + (e.bbox && e.bbox.length ? " ▣" : "")
    });
  });
  (tl.vision || []).forEach(function (v) {
    var txt = v.error
      ? ("YOLO snapshot #" + v.snap_index + " failed: " + v.error)
      : ("YOLO snapshot #" + v.snap_index + ": " + v.count + " object(s)" +
         (v.top ? " · " + v.top : "") + " · " + v.ms + " ms");
    entries.push({t: +v.at, icon: "👁", cls: v.error ? "tl-error" : "tl-vision", text: txt, url: v.error ? null : v.snapshot});
  });
  if (det.ended_at) entries.push({t: +det.ended_at, icon: "■", cls: "tl-end", text: "Post-detection window ends"});
  if (det.clip) entries.push({t: +det.ended_at, icon: "🎬", cls: "tl-ok", text: "Clip ready"});
  else if (det.clip_pending) entries.push({t: +det.ended_at, icon: "🎬", cls: "tl-pending", text: "Clip assembling…"});
  if (det.s3_expected) entries.push({
    t: +det.ended_at, icon: "☁", cls: det.uploaded ? "tl-ok" : "tl-pending",
    text: det.uploaded ? "S3 upload done" : "S3 upload pending"
  });
  entries.sort(function (a, b) { return (a.t || 0) - (b.t || 0); });
  if (!entries.length) {
    var none = document.createElement("div");
    none.className = "meta";
    none.textContent = "No timeline entries.";
    box.appendChild(none);
    return;
  }
  var base = +det.started_at || entries[0].t;
  entries.forEach(function (e) { box.appendChild(_timelineRow(e, base)); });
  var foot = document.createElement("div");
  foot.className = "meta tl-foot";
  foot.textContent = "group " + String(det.id || "").slice(0, 8) + " · " +
    detectionTags(det) + " · " + (det.camera_name || det.camera_id || "");
  box.appendChild(foot);
}

function renderDetailTab() {
  if (!curDetail) return;
  var d = curDetail;
  var detail = document.getElementById("detail");
  // ensure tabs exist
  var tabsEl = document.getElementById("detailTabs");
  if (!tabsEl) {
    tabsEl = document.createElement("div");
    tabsEl.id = "detailTabs";
    tabsEl.className = "tabs";
    var snapsDiv = document.getElementById("dsnaps");
    detail.insertBefore(tabsEl, snapsDiv);
  }
  tabsEl.innerHTML = "";
  var snapsCount = (d.snapshots && d.snapshots.length) ? d.snapshots.length : (d.snapshot ? 1 : 0);
  var btnSnap = document.createElement("button");
  btnSnap.className = curTab === 'snapshots' ? 'tab active' : 'tab';
  btnSnap.textContent = 'Snapshots (' + snapsCount + ')';
  btnSnap.onclick = function () { curTab = 'snapshots'; renderDetailTab(); };
  var btnVid = document.createElement("button");
  btnVid.className = curTab === 'video' ? 'tab active' : 'tab';
  btnVid.textContent = d.clip ? 'Video' : (d.clip_pending ? '⏳ assemblage…' : 'Video');
  btnVid.title = d.clip ? '' : (d.clip_pending ? "Clip en cours d'assemblage" : "Pas de vidéo pour cet enregistrement");
  btnVid.disabled = !d.clip;
  btnVid.onclick = function () { curTab = 'video'; renderDetailTab(); };
  var btnTl = document.createElement("button");
  btnTl.className = curTab === 'timeline' ? 'tab active' : 'tab';
  btnTl.textContent = 'Timeline';
  btnTl.title = 'Chronological view: events, YOLO snapshots, clip/S3 status';
  btnTl.onclick = function () { curTab = 'timeline'; renderDetailTab(); };
  tabsEl.appendChild(btnSnap);
  tabsEl.appendChild(btnVid);
  tabsEl.appendChild(btnTl);

  var snaps = document.getElementById("dsnaps");
  var clip = document.getElementById("dclip");
  var tlEl = _timelineEl();
  if (curTab === 'timeline') {
    snaps.hidden = true;
    if (clip) { try { clip.pause(); } catch (e) {} clip.hidden = true; }
    tlEl.hidden = false;
    tlEl.innerHTML = "";
    var loading = document.createElement("div");
    loading.className = "meta";
    loading.style.padding = "8px 0";
    loading.textContent = "Loading timeline…";
    tlEl.appendChild(loading);
    var did = d.id;
    api("/api/detections/" + encodeURIComponent(did) + "/timeline").then(function (tl) {
      if (!curDetail || curDetail.id !== did || curTab !== 'timeline') return;
      renderTimeline(d, tlEl, tl);
    }).catch(function (e) {
      if (!curDetail || curDetail.id !== did || curTab !== 'timeline') return;
      tlEl.innerHTML = "";
      var m = document.createElement("div");
      m.className = "meta";
      m.textContent = "Timeline failed: " + e.message;
      tlEl.appendChild(m);
    });
    return;
  }
  tlEl.hidden = true;
  if (curTab === 'snapshots') {
    snaps.hidden = false;
    if (clip) clip.hidden = true;
    snaps.innerHTML = "";
    var urls = d.snapshots && d.snapshots.length ? d.snapshots : (d.snapshot ? [d.snapshot] : []);
    if (!urls.length) {
      var none = document.createElement("div");
      none.className = "meta";
      none.textContent = "No snapshots (snapshots disabled for this recording).";
      snaps.appendChild(none);
    }
    urls.forEach(function (u, i) {
      // every snapshot asks for YOLO boxes burned in (server-side): the
      // server annotates only files with a positive per-snapshot result
      // and serves the plain JPEG otherwise (?boxes=1 is per-file safe)
      var src = visionBoxesURL(d, u);
      var img = document.createElement("img");
      _imgFallback(img, "snapshot manquant");
      img.src = src;
      img.alt = "snapshot";
      img.loading = "lazy";
      img.style.cursor = "zoom-in";
      img.onclick = function () { showLightbox(src); };
      snaps.appendChild(img);
    });
    // vision section (summary + re-analyze) below snapshots
    var dv=document.getElementById("dvision");
    if(!dv){ dv=document.createElement("div"); dv.id="dvision"; dv.className="vision-panel"; snaps.appendChild(dv); }
    else snaps.appendChild(dv);
    refreshVisionDetail(d);
  } else {
    snaps.hidden = true;
    if (clip && _safeMediaURL(d.clip)) { clip.hidden = false; clip.src = d.clip + "#t=0.001"; clip.load(); var p=clip.play(); if(p&&p.catch) p.catch(function(){}); }
    else if (d.clip_pending) {
      // Buffer-mode clip still assembling: show the wait state instead of a
      // dead player. Next list refresh swaps in the real clip.
      if (clip) clip.hidden = true;
      snaps.hidden = false; snaps.innerHTML = "";
      var wait = document.createElement("div");
      wait.className = "meta"; wait.style.cssText = "padding:16px;text-align:center";
      wait.textContent = "⏳ Clip en cours d'assemblage…";
      snaps.appendChild(wait);
    }
  }
}

// Event view modal: the #detail panel floats as a centered dialog over a
// backdrop instead of sitting at the bottom of the page.
function ensureDetailModal(){
  var detail = document.getElementById("detail");
  if(!document.getElementById("modalBackdrop")){
    var bd = document.createElement("div");
    bd.id = "modalBackdrop"; bd.hidden = true; bd.onclick = hideDetailModal;
    document.body.appendChild(bd);
  }
  if(!document.getElementById("detailClose")){
    var x = document.createElement("button");
    x.id = "detailClose"; x.className = "modal-close"; x.textContent = "✕"; x.title = "Close";
    x.onclick = function(e){ e.stopPropagation(); hideDetailModal(); };
    detail.insertBefore(x, detail.firstChild);
  }
}
function openDetailModal(){
  ensureDetailModal();
  var detail = document.getElementById("detail");
  detail.classList.add("modal");
  document.getElementById("modalBackdrop").hidden = false;
  document.body.classList.add("modal-open");
}
function hideDetailModal(){
  var detail = document.getElementById("detail");
  var bd = document.getElementById("modalBackdrop");
  curDetail = null;
  if(detail){ detail.hidden = true; detail.classList.remove("modal"); }
  if(bd) bd.hidden = true;
  document.body.classList.remove("modal-open");
  var vc = document.getElementById("dclip");
  if(vc){ try{ vc.pause(); vc.removeAttribute("src"); vc.load(); }catch(e){} }
}
document.addEventListener("keydown", function(e){
  if(e.key === "Escape"){
    var detail = document.getElementById("detail");
    if(detail && !detail.hidden) hideDetailModal();
  }
});

function showDetail(d, mode) {
  curDetail = d;
  curTab = mode || 'snapshots';
  document.getElementById("detail").hidden = false;
  document.getElementById("dtitle").textContent = d.camera_name + " · " + detectionTags(d) + " · " + new Date(d.started_at * 1000).toLocaleString();
  document.getElementById("dmeta").textContent =
    "duration " + fmtDuration(d.duration != null ? d.duration : (d.ended_at - d.started_at)) + " · size " + fmtSize(d.size) + " · confidence " + d.confidence + (d.uploaded ? " · ☁️ S3" : "") + (visionBadgeText(d) ? " · " + visionBadgeText(d) : "") + (bboxBadgeText(d) ? " · " + bboxBadgeText(d) : "");
  renderDetailTab();
  openDetailModal();
}

document.getElementById("delDetail").onclick = function () {
  if (!curDetail) return;
  if (!confirm("Delete this detection (snapshots + clip)?")) return;
  fetch("/api/detections/" + encodeURIComponent(curDetail.id), { method: "DELETE", headers: headers() })
    .then(function (r) { if (!r.ok) throw new Error("delete failed " + r.status); hideDetailModal(); refreshRecordings(); })
    .catch(function (err) { toast("Delete failed: " + err.message, "error"); });
};

function load() {
  refreshDetection();
  refreshToggles();
  api("/api/cameras").then(renderCameras).catch(function () {});
  refreshFilterCams();
  // Only load the visible tab. The old unconditional calls fetched 500
  // detections + 1000 raw events and built hidden grid thumbnails on every
  // page open, which could pin CPU/disk/RAM before the user clicked anything.
  if (activeTab === "recordings") refreshRecordings();
  else if (activeTab === "events") refreshEvents();
}

// Refresh only the visible tab, and never while the tab is hidden or while
// the user is typing in a filter control: an unconditional re-render rebuilds
// the filter bars and steals focus every 8 seconds.
setInterval(function () {
  if (document.hidden) return;
  if (_filterBarHasFocus()) return;
  if (activeTab === "recordings") refreshRecordings();
  else if (activeTab === "events") refreshEvents();
}, 8000);

// Live "motion detected" banner via Server-Sent Events. Shown under the stream
// of the camera that reported the detection for 5s (repeated events extend it).
// (_bannerTimers declared once near the top with the other UI state.)
var _visionRefreshT = null;

function initEvents() {
  var es = new EventSource("/api/events");
  es.addEventListener("detection", function (ev) {
    var d = {};
    try { d = JSON.parse(ev.data); } catch (e) {}
    // vision analysis landed after the fact: refresh badges/boxes (debounced,
    // bursts analyze several snapshots within seconds).
    if (d.kind === "vision") {
      if (_visionRefreshT) return;
      if (activeTab !== "recordings") return;
      _visionRefreshT = setTimeout(function () { _visionRefreshT = null; refreshRecordings(); }, 2000);
      return;
    }
    // Live SEI boxes: draw them over the video of the reporting camera.
    if (d.kind === "boxes") {
      var bid = d.camera_id || _boxKeyAlias[d.camera] || d.camera;
      if (bid && d.bbox) _setLiveBoxes(bid, d.bbox);
      return;
    }
    var bkey = d.camera_id || d.camera;
    var banner = _bannerByCam[bkey];
    if (!banner) return;
    banner.hidden = false;
    if (_bannerTimers[bkey]) clearTimeout(_bannerTimers[bkey]);
    _bannerTimers[bkey] = setTimeout(function () { banner.hidden = true; }, 5000);
  });
  // Reconnect handled automatically by EventSource.
}

initEvents();
initStreamMode();
initLiveBoxesToggle();
load();