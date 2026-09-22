const wails = await import("/wails/runtime.js").catch(() => null);
const hasBackend = Boolean(wails?.Call);
const $ = (id) => document.getElementById(id);

let snapshot = null;
let timeline = [];
let chatFilter = "all";
let availability = {};
let hosted = { running: false };
let lastConnectionRequest = null;
let connectionState = "disconnected";
let playlistHistory = [];
let draggedPlaylistIndex = -1;
let toastTimer = 0;
let lastToast = { message: "", at: 0 };
let dragDepth = 0;
let activeSource = "";
let activeWheel = null;
let wheelAnimation = 0;
let wheelServerOffsetMs = 0;
let wheelRotation = 0;
let isScrubbing = false;
let seekCommandPending = false;
let seekReleaseTimer = 0;
let appVersion = "2";
let timelineSegments = [];
let timelineSegmentKey = "";
let timelineSegmentsRetryKey = "";
let wheelAudioContext = null;
let wheelSpinOutput = null;
let lastWheelTick = -1;
let lastWheelSoundTime = -Infinity;
let addingPlaylistURL = false;
let streamingAvailable = false;
let streamState = { state: "idle", offerId: "", route: "" };
let legalInfoLoaded = false;
let legalSourceURL = "";
const youtubeInfoCache = new Map();
// The wheel is drawn on a canvas, so it cannot inherit the CSS tokens; these
// two palettes mirror them instead. Both keep neighbouring segments in
// different hues, and each theme owns its own label treatment.
const wheelThemes = {
  dark: {
    segments: [
      "#2563eb", // Royal Blue
      "#059669", // Emerald Green
      "#d97706", // Amber
      "#dc2626", // Crimson
      "#7c3aed", // Violet
      "#0891b2", // Cyan
      "#ea580c", // Orange
      "#db2777", // Rose
      "#0d9488", // Teal
      "#4f46e5", // Indigo
      "#65a30d", // Lime
      "#c026d3"  // Fuchsia
    ],
    // White on every segment with a drop shadow, as the dark wheel has always
    // drawn itself.
    label: () => ({ color: "#ffffff", shadow: "rgba(0, 0, 0, 0.6)" }),
    separator: "rgba(22, 22, 29, 0.42)",
    rim: "rgba(255, 255, 255, 0.3)",
    empty: "#18181a"
  },
  light: {
    segments: [
      "#4D699B", // muted blue
      "#A97834", // ochre
      "#6E5A8C", // plum
      "#587D5B", // green
      "#B55353", // muted red
      "#4F7E75", // muted teal
      "#B06A3B", // terracotta
      "#5F6B8A", // indigo gray
      "#A85F73", // rose
      "#6F7A3F", // olive
      "#8A6A4F", // umber
      "#3F7A8C"  // cyan
    ],
    // The muted paper set runs from light ochre to dark indigo, so each label
    // takes whichever of the two inks reads better on its own segment. The
    // shadow only earns its place under the light one.
    label: (segment) => {
      const ink = wheelLabelColor(segment, "#FCFBF9", "#25272A");
      return { color: ink, shadow: ink === "#25272A" ? "transparent" : "rgba(0, 0, 0, 0.6)" };
    },
    separator: "rgba(37, 39, 42, 0.3)",
    rim: "rgba(37, 39, 42, 0.18)",
    empty: "#FFFFFF"
  },
  sage: {
    segments: [
      "#4E7A63", // sage green
      "#BA5220", // terracotta
      "#4A6D7C", // slate blue
      "#8C874A", // olive
      "#7D5875", // mauve
      "#387780", // teal
      "#A86F30", // ochre
      "#5A6585", // indigo gray
      "#A35467", // rose
      "#657A36", // moss
      "#675284", // plum
      "#49784D"  // fern
    ],
    // Same per-segment ink logic as the light wheel, re-based onto the calculator
    // palette and its crisp LCD charcoal ink.
    label: (segment) => {
      const ink = wheelLabelColor(segment, "#F2F6EC", "#222922");
      return { color: ink, shadow: ink === "#222922" ? "transparent" : "rgba(0, 0, 0, 0.6)" };
    },
    separator: "rgba(34, 41, 34, 0.3)",
    rim: "rgba(34, 41, 34, 0.18)",
    empty: "#E2E7DC"
  }
};
function wheelTheme() {
  return wheelThemes[document.documentElement.dataset.theme] || wheelThemes.dark;
}
function relativeLuminance(hex) {
  const value = parseInt(hex.slice(1), 16);
  return [16, 8, 0].reduce((total, shift, index) => {
    const channel = ((value >> shift) & 255) / 255;
    const linear = channel <= 0.04045 ? channel / 12.92 : Math.pow((channel + 0.055) / 1.055, 2.4);
    return total + linear * [0.2126, 0.7152, 0.0722][index];
  }, 0);
}
function wheelLabelColor(background, light, ink) {
  const ratio = (color) => {
    const [high, low] = [relativeLuminance(color), relativeLuminance(background)].sort((a, b) => b - a);
    return (high + 0.05) / (low + 0.05);
  };
  return ratio(light) >= ratio(ink) ? light : ink;
}
const sunIcon = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.42 1.42M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.42-1.41M17.66 6.34l1.41-1.41"/></svg>';
const moonIcon = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8Z"/></svg>';
const playIcon = '<svg class="play-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 9 6-9 6V6Z" fill="currentColor" stroke="none"/></svg>';
const pauseIcon = '<svg class="play-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6v12M15 6v12"/></svg>';

const defaultPreferences = {
  theme: "system", compact: false, reduceMotion: false, skipSeconds: 10, wheelSound: true,
  pauseOnLeave: false, sponsorBlock: true, youtubeQualities: {}, name: "", player: "mpv", executable: "",
  playerArgs: "", publicHost: "localhost", listenAddress: ":8999", room: "watch"
};
let preferences = loadPreferences();

function invoke(name, ...args) {
  if (!hasBackend) return Promise.reject(new Error("Faro desktop bridge is not ready"));
  return wails.Call.ByName(`main.Desktop.${name}`, ...args);
}

function showToast(message, type = "success") {
  message = String(message || "Something went wrong");
  const now = Date.now();
  if (message === lastToast.message && now - lastToast.at < 3000) return;
  lastToast = { message, at: now };
  const toast = $("toast");
  if (!toast) return;
  const msgEl = $("toast-message");
  if (msgEl) msgEl.textContent = message;
  const iconEl = $("toast-icon");
  if (iconEl) {
    iconEl.innerHTML = type === "error"
      ? '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.8"/><path d="M8 5v3.5M8 11.2v.3" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>'
      : '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><path d="M3 8.5L6.5 12L13 4" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
  }
  toast.classList.toggle("error", type === "error");
  toast.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("show"), 3200);
}

function showError(error) { showToast(error?.message || String(error), "error"); }

async function withButtonLoading(button, task, loadingLabel = "") {
  if (!button || button.dataset.busy === "true") return;
  const label = button.querySelector("span");
  const previousLabel = label?.textContent || "";
  button.dataset.busy = "true";
  button.disabled = true;
  button.classList.add("is-loading");
  button.setAttribute("aria-busy", "true");
  if (label && loadingLabel) label.textContent = loadingLabel;
  try { return await task(); }
  finally {
    if (label && loadingLabel) label.textContent = previousLabel;
    button.dataset.busy = "false";
    button.classList.remove("is-loading");
    button.removeAttribute("aria-busy");
    button.disabled = false;
    if (snapshot) render();
  }
}

function setDisabled(id, disabled) {
  const element = $(id);
  if (element) element.disabled = Boolean(disabled) || element.dataset.busy === "true";
}

function formatTime(seconds) {
  if (!Number.isFinite(seconds) || seconds < 0) return "00:00";
  const whole = Math.floor(seconds);
  const hours = Math.floor(whole / 3600);
  const minutes = Math.floor((whole % 3600) / 60);
  const secs = whole % 60;
  if (hours > 0) {
    return `${hours}:${String(minutes).padStart(2, "0")}:${String(secs).padStart(2, "0")}`;
  }
  return `${String(minutes).padStart(2, "0")}:${String(secs).padStart(2, "0")}`;
}

function clamp(value, minimum, maximum) {
  return Math.min(maximum, Math.max(minimum, Number(value) || 0));
}

function referenceMedia() {
  const me = self();
  return me?.media || snapshot?.participants?.find((person) => person.media)?.media || null;
}

function playbackDuration() {
  const duration = Number(referenceMedia()?.durationSeconds || 0);
  return Number.isFinite(duration) && duration > 0 ? duration : 0;
}

function updateScrubberProgress(position = 0, duration = playbackDuration()) {
  const progress = duration > 0 ? clamp(position / duration * 100, 0, 100) : 0;
  $("position").style.setProperty("--range-progress", `${progress}%`);
  document.querySelector(".timeline-control-row")?.style.setProperty("--range-progress", `${progress}%`);
}

function renderTimelineSegments(duration = playbackDuration()) {
  const container = $("timeline-segments");
  if (!container) return;
  if (!duration) {
    container.replaceChildren();
    return;
  }
  container.replaceChildren(...timelineSegments.filter((segment) => Number(segment.startSeconds) >= 0 && Number(segment.startSeconds) <= duration).map((segment) => {
    const marker = document.createElement("span");
    marker.className = `timeline-segment ${segment.kind === "chapter" ? "chapter" : "sponsorblock"}`;
    marker.style.left = `${clamp(Number(segment.startSeconds) / duration * 100, 0, 100)}%`;
    if (segment.kind !== "chapter") {
      const end = clamp(Number(segment.endSeconds || segment.startSeconds) / duration * 100, 0, 100);
      marker.style.width = `${Math.max(0.2, end - Number(segment.startSeconds) / duration * 100)}%`;
    }
    marker.title = segment.title || (segment.kind === "chapter" ? "Chapter" : "SponsorBlock segment");
    return marker;
  }));
}

function fetchTimelineSegments(key) {
  invoke("TimelineSegments").then((segments) => {
    if (timelineSegmentKey !== key) return;
    timelineSegments = Array.isArray(segments) ? segments : [];
    renderTimelineSegments();
    if (!timelineSegments.some((segment) => segment.kind === "chapter") && timelineSegmentsRetryKey !== key) {
      timelineSegmentsRetryKey = key;
      setTimeout(() => { if (timelineSegmentKey === key) fetchTimelineSegments(key); }, 2000);
    }
  }).catch(() => {});
}

function refreshTimelineSegments() {
  const media = referenceMedia();
  const key = `${selectedSourceURL()}|${media?.fingerprint || ""}|${media?.durationSeconds || 0}`;
  if (key === timelineSegmentKey) {
    renderTimelineSegments();
    return;
  }
  timelineSegmentKey = key;
  timelineSegments = [];
  renderTimelineSegments();
  if (!media || !hasBackend) return;
  fetchTimelineSegments(key);
}

function projectedPlaybackPosition() {
  if (!snapshot) return 0;
  const playback = snapshot.playback;
  const elapsed = playback.paused ? 0 : Math.max(0, Date.now() - Number(playback.updatedAtUnixMs || Date.now())) / 1000;
  const projected = Math.max(0, Number(playback.positionSeconds || 0) + elapsed * Number(playback.rate || 1));
  const duration = playbackDuration();
  return duration ? clamp(projected, 0, duration) : projected;
}

function releaseScrubber(delay = 650) {
  clearTimeout(seekReleaseTimer);
  seekReleaseTimer = setTimeout(() => {
    isScrubbing = false;
    if (snapshot) render();
  }, delay);
}

function parseArguments(value) {
  const matches = value.match(/(?:[^\s"']+|"[^"]*"|'[^']*')+/g) || [];
  return matches.map((item) => item.replace(/^("|')|("|')$/g, ""));
}

function basename(path) { return String(path).split(/[\\/]/).filter(Boolean).pop() || path; }
function isYouTubeURL(source) { return /^https?:\/\/(?:www\.|m\.)?(?:youtube\.com\/|youtu\.be\/)/i.test(String(source || "").trim()); }
function self() { return snapshot?.participants?.find((person) => person.id === snapshot.selfId); }
function canControl() { const me = self(); return snapshot?.room?.mode === "collaborative" || me?.role === "owner" || me?.role === "moderator"; }
function playlistInputs() { return (snapshot?.playlist?.items || []).map(({ id, label, url, media }) => ({ id, label, url: url || "", media: media || null, source: "" })); }

function normalizeSnapshot(value) {
  if (!value) return null;
  value.participants = Array.isArray(value.participants) ? value.participants : [];
  value.playlist ||= { revision: 0, selected: -1, items: [] };
  value.playlist.items = Array.isArray(value.playlist.items) ? value.playlist.items : [];
  value.playback ||= { positionSeconds: 0, paused: true, rate: 1, updatedAtUnixMs: Date.now() };
  value.room ||= { id: "room", mode: "collaborative" };
  return value;
}

function loadPreferences() {
  try { return { ...defaultPreferences, ...JSON.parse(localStorage.getItem("faro.preferences") || "{}") }; }
  catch (_) { return { ...defaultPreferences }; }
}

function savePreferences(values = {}) {
  preferences = { ...preferences, ...values };
  localStorage.setItem("faro.preferences", JSON.stringify(preferences));
  applyPreferences();
}

function applyPreferences() {
  const systemDark = matchMedia("(prefers-color-scheme: dark)").matches;
  const resolved = preferences.theme === "system" ? (systemDark ? "dark" : "light") : preferences.theme;
  // data-theme carries the resolved theme, never the preference, so styles.css
  // defines each palette once.
  document.documentElement.dataset.theme = resolved;
  document.documentElement.style.colorScheme = resolved === "dark" ? "dark" : "light";
  document.body.classList.toggle("compact", Boolean(preferences.compact));
  document.body.classList.toggle("reduce-motion", Boolean(preferences.reduceMotion));
  const themeButton = $("welcome-theme");
  if (themeButton) {
    const iconSpan = themeButton.querySelector(".theme-btn-icon");
    if (iconSpan) {
      iconSpan.innerHTML = resolved === "dark" ? sunIcon : moonIcon;
    } else {
      themeButton.innerHTML = resolved === "dark" ? sunIcon : moonIcon;
    }
  }
  if ($("theme-select")) {
    $("theme-select").value = preferences.theme;
    $("theme-select")._syncCustomSelect?.();
  }
  if ($("compact-mode")) $("compact-mode").checked = Boolean(preferences.compact);
  if ($("reduce-motion")) $("reduce-motion").checked = Boolean(preferences.reduceMotion);
  if ($("skip-seconds")) $("skip-seconds").value = preferences.skipSeconds;
  document.querySelectorAll(".skip-num").forEach((node) => node.textContent = preferences.skipSeconds);
  if ($("pause-on-leave")) $("pause-on-leave").checked = Boolean(preferences.pauseOnLeave);
  if ($("sponsorblock-enabled")) $("sponsorblock-enabled").checked = preferences.sponsorBlock !== false;
  if ($("wheel-sound-enabled")) $("wheel-sound-enabled").checked = preferences.wheelSound !== false;
}

function hydrateConnectForms() {
  for (const prefix of ["join", "host"]) {
    $(`${prefix}-name`).value = preferences.name;
    $(`${prefix}-player`).value = preferences.player;
    $(`${prefix}-executable`).value = preferences.executable;
    $(`${prefix}-args`).value = preferences.playerArgs;
    $(`${prefix}-player`).addEventListener("change", () => detectPlayer(prefix, true));
    $(`${prefix}-executable`).addEventListener("input", (event) => {
      playerDetectionSequence[prefix]++;
      event.target.dataset.detected = "";
      event.target.classList.remove("player-detected");
      const status = $(`${prefix}-executable-status`);
      status.className = "field-status";
      status.textContent = event.target.value.trim() ? "Using custom location" : "";
    });
    detectPlayer(prefix, false);
  }
  $("host-public").value = preferences.publicHost;
  $("host-listen").value = preferences.listenAddress;
  $("host-room").value = preferences.room;
}

async function configurePlayerOptions() {
  if (!hasBackend) return;
  let supported;
  try {
    supported = new Set((await invoke("SupportedPlayers")).map((player) => player.toLowerCase()));
  } catch (_) {
    return;
  }
  for (const prefix of ["join", "host"]) {
    const select = $(`${prefix}-player`);
    [...select.options].forEach((option) => {
      if (!supported.has(option.value.toLowerCase())) option.remove();
    });
  }
  if (!supported.has(String(preferences.player || "").toLowerCase())) {
    preferences = { ...preferences, player: "mpv", executable: "", playerArgs: "" };
  }
}

const playerDetectionSequence = { join: 0, host: 0 };
async function detectPlayer(prefix, force) {
  const input = $(`${prefix}-executable`);
  const status = $(`${prefix}-executable-status`);
  const player = $(`${prefix}-player`).value;
  const sequence = ++playerDetectionSequence[prefix];
  status.className = "field-status";
  status.textContent = "Looking for player…";
  input.classList.remove("player-detected");
  try {
    const path = await invoke("DetectPlayer", player);
    if (sequence !== playerDetectionSequence[prefix]) return;
    const useDetected = force || !input.value.trim() || Boolean(input.dataset.detected);
    if (useDetected) {
      input.value = path;
      input.dataset.detected = path;
      input.classList.add("player-detected");
      status.className = "field-status detected";
      status.textContent = "Detected automatically";
      input.title = path;
    } else {
      input.dataset.detected = "";
      status.textContent = "Using custom location";
    }
  } catch (_) {
    if (sequence !== playerDetectionSequence[prefix]) return;
    if (force || input.dataset.detected) input.value = "";
    input.dataset.detected = "";
    status.className = "field-status missing";
    status.textContent = input.value.trim() ? "Using custom location" : "Not found — choose the executable manually";
  }
}

function connectionRequest(prefix, invite) {
  return {
    invite,
    name: $(`${prefix}-name`).value.trim(),
    player: $(`${prefix}-player`).value,
    executable: $(`${prefix}-executable`).value.trim(),
    playerArgs: parseArguments($(`${prefix}-args`).value)
  };
}

function rememberConnectPreferences(prefix) {
  savePreferences({
    name: $(`${prefix}-name`).value.trim(),
    player: $(`${prefix}-player`).value,
    executable: $(`${prefix}-executable`).value.trim(),
    playerArgs: $(`${prefix}-args`).value.trim(),
    publicHost: $("host-public").value.trim(),
    listenAddress: $("host-listen").value.trim(),
    room: $("host-room").value.trim()
  });
}

function mediaDirectories() {
  try { return JSON.parse(localStorage.getItem("faro.mediaDirectories") || "[]"); } catch (_) { return []; }
}

function renderMediaDirectories() {
  const container = $("media-directories");
  if (!container) return;
  container.replaceChildren(...mediaDirectories().map((directory) => {
    const row = document.createElement("li");
    const path = document.createElement("span"); path.textContent = directory; path.title = directory;
    const remove = document.createElement("button"); remove.type = "button"; remove.className = "mini-button danger-quiet"; remove.textContent = "Remove";
    remove.onclick = () => {
      localStorage.setItem("faro.mediaDirectories", JSON.stringify(mediaDirectories().filter((item) => item !== directory)));
      renderMediaDirectories();
    };
    row.append(path, remove);
    return row;
  }));
}

async function indexSavedDirectories() {
  for (const directory of mediaDirectories()) {
    try { await invoke("IndexMediaDirectory", directory); } catch (error) { showError(error); }
  }
  await refreshAvailability();
}

async function refreshAvailability() {
  if (!snapshot || !hasBackend) return;
  try { availability = await invoke("PlaylistAvailability"); renderPlaylist(); } catch (_) {}
}

async function refreshStreamingSupport() {
  if (!snapshot || !hasBackend) return;
  try { streamingAvailable = Boolean(await invoke("StreamingAvailable")); }
  catch (_) { streamingAvailable = false; }
  if (snapshot) render();
}

function render() {
  if (!snapshot) return;
  document.body.classList.add("room-connected");
  $("connect-view").classList.add("hidden");
  $("room-view").classList.remove("hidden");
  $("room-name").textContent = snapshot.room.id;
  $("window-context").textContent = `Room · ${snapshot.room.id}`;
  const me = self(), allowed = canControl();

  $("room-mode").value = snapshot.room.mode;
  $("room-mode").disabled = me?.role !== "owner";
  $("room-mode")._syncCustomSelect?.();
  $("owner-invite").classList.toggle("hidden", me?.role !== "owner");
  $("settings-owner-invite").classList.toggle("hidden", me?.role !== "owner");

  for (const id of ["pause", "position", "back-ten", "forward-ten", "playback-rate", "rate-decrease", "rate-increase", "add-file", "empty-add-file", "add-stream-btn", "add-playlist", "shuffle-playlist", "shuffle-all", "load-playlist-file", "save-playlist-file", "clear-playlist", "spin-wheel", "wheel-spin-again", "previous-media", "next-media"]) setDisabled(id, !allowed);
  setDisabled("add-playlist", !allowed || addingPlaylistURL);

  const media = me?.media || snapshot.participants.find((person) => person.media)?.media;
  const title = media?.title || "No media loaded";
  $("media-title").textContent = title;
  $("media-title").title = title;
  const mediaSourceKind = streamState.state === "active" ? "Direct P2P" : media?.fingerprint ? "Local media" : "Stream";
  $("media-kind").textContent = media ? `${media.sizeBytes ? formatBytes(media.sizeBytes) + " · " : ""}${mediaSourceKind}` : "No media loaded";
  const mismatches = media ? snapshot.participants.filter((person) => person.media && person.media.fingerprint !== media.fingerprint) : [];
  $("media-warning").classList.toggle("hidden", mismatches.length === 0);
  $("media-warning").textContent = mismatches.length ? `${mismatches.length} participant${mismatches.length === 1 ? " has" : "s have"} different media loaded. Sync paused for mismatched copies.` : "";

  const syncState = $("sync-state");
  syncState.textContent = media ? (mismatches.length ? "Mismatch" : "In sync") : "Waiting for media";
  syncState.className = `badge badge-sync ${media ? (mismatches.length ? "badge-warning" : "badge-success") : ""}`;

  const playback = snapshot.playback;
  const duration = Number(media?.durationSeconds || 0);
  const hasDuration = Number.isFinite(duration) && duration > 0;
  const position = $("position");
  position.max = hasDuration ? String(duration) : "1";
  position.disabled = !allowed || !hasDuration;
  if (!isScrubbing) {
    const projected = projectedPlaybackPosition();
    const displayedPosition = hasDuration ? clamp(projected, 0, duration) : Math.max(0, projected);
    position.value = hasDuration ? String(displayedPosition) : "0";
    $("current-time").textContent = formatTime(displayedPosition);
    updateScrubberProgress(displayedPosition, duration);
  }
  $("duration").textContent = hasDuration ? formatTime(duration) : "--:--";
  refreshTimelineSegments();

  const pauseBtn = $("pause");
  pauseBtn.innerHTML = playback.paused ? playIcon : pauseIcon;
  pauseBtn.setAttribute("aria-label", playback.paused ? "Play" : "Pause");

  updateRateControl(playback.rate || 1);
  renderParticipants(media);
  renderPlaylist();
  renderServerStatus();
  renderConnectionInfo();
}

const playbackRates = [0.5, 0.75, 1, 1.25, 1.5, 2];
function updateRateControl(rate) {
  const value = Number(rate) || 1;
  const control = $("playback-rate");
  control.dataset.value = String(value);
  control.textContent = `${Number(value.toFixed(2))}×`;
  const index = playbackRates.reduce((best, candidate, candidateIndex) => Math.abs(candidate - value) < Math.abs(playbackRates[best] - value) ? candidateIndex : best, 0);
  $("rate-decrease").disabled = !canControl() || index === 0;
  $("rate-increase").disabled = !canControl() || index === playbackRates.length - 1;
}

function stepPlaybackRate(direction) {
  const current = Number($("playback-rate").dataset.value || 1);
  const index = playbackRates.reduce((best, candidate, candidateIndex) => Math.abs(candidate - current) < Math.abs(playbackRates[best] - current) ? candidateIndex : best, 0);
  const target = playbackRates[clamp(index + direction, 0, playbackRates.length - 1)];
  if (target !== current) invoke("SetRate", target).catch(showError);
}

function wheelTargetRotation(wheel) {
  const count = Math.max(1, wheel.items?.length || 0);
  const arc = Math.PI * 2 / count;
  const seed = Number(wheel.visualSeed || 0);
  const jitter = (((seed % 1000) / 999) - 0.5) * arc * 0.34;
  return Number(wheel.turns || 8) * Math.PI * 2 - (Number(wheel.winner || 0) + 0.5) * arc - jitter;
}

function drawWheel(wheel, rotation = 0, highlight = -1) {
  const canvas = $("wheel-canvas"), ctx = canvas.getContext("2d");
  const items = wheel?.items?.length ? wheel.items : (snapshot?.playlist?.items || []).map(({ id, label }) => ({ id, label }));
  const theme = wheelTheme();
  const size = canvas.width, center = size / 2, radius = center - 14;
  ctx.clearRect(0, 0, size, size);
  if (!items.length) {
    ctx.beginPath(); ctx.arc(center, center, radius, 0, Math.PI * 2);
    ctx.fillStyle = theme.empty; ctx.fill();
    return;
  }
  const arc = Math.PI * 2 / items.length;
  ctx.save();
  ctx.translate(center, center);
  ctx.rotate(rotation);
  for (let index = 0; index < items.length; index++) {
    const start = -Math.PI / 2 + index * arc, end = start + arc;
    ctx.beginPath(); ctx.moveTo(0, 0); ctx.arc(0, 0, radius, start, end); ctx.closePath();
    // A queue position owns its color. The spin seed only affects trajectory,
    // so opening or spinning the same wheel never repaints its segments.
    ctx.fillStyle = theme.segments[index % theme.segments.length];
    ctx.fill();
    ctx.strokeStyle = theme.separator; ctx.lineWidth = 3; ctx.stroke();
    if (index === highlight) {
      ctx.save(); ctx.globalAlpha = 0.28; ctx.fillStyle = "#ffffff"; ctx.fill(); ctx.restore();
    }
    if (items.length <= 24) {
      const label = String(items[index].label || `Item ${index + 1}`);
      const maximum = items.length > 12 ? 20 : 30;
      const text = label.length > maximum ? `${label.slice(0, maximum - 1)}…` : label;
      const ink = theme.label(theme.segments[index % theme.segments.length]);
      ctx.save();
      ctx.rotate(start + arc / 2);
      ctx.textAlign = "right";
      ctx.textBaseline = "middle";
      ctx.font = `600 ${items.length > 12 ? 32 : 38}px -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif`;
      ctx.fillStyle = ink.color;
      ctx.shadowColor = ink.shadow;
      ctx.shadowBlur = 4;
      ctx.fillText(text, radius - 34, 0, radius * 0.65);
      ctx.restore();
    }
  }
  ctx.restore();
  ctx.beginPath();
  ctx.arc(center, center, radius, 0, Math.PI * 2);
  ctx.strokeStyle = theme.rim;
  ctx.lineWidth = 4;
  ctx.stroke();
}

function wheelCandidate(wheel, rotation) {
  const items = wheel?.items || [];
  if (!items.length) return null;
  const arc = Math.PI * 2 / items.length;
  const normalized = ((-rotation % (Math.PI * 2)) + Math.PI * 2) % (Math.PI * 2);
  return items[Math.floor(normalized / arc) % items.length];
}

function ensureWheelAudio() {
  if (preferences.wheelSound === false) return null;
  const AudioContext = window.AudioContext || window.webkitAudioContext;
  if (!AudioContext) return null;
  if (!wheelAudioContext) {
    try { wheelAudioContext = new AudioContext(); }
    catch (_) { return null; }
  }
  if (wheelAudioContext.state === "suspended") {
    wheelAudioContext.resume().catch(() => {});
  }
  return wheelAudioContext;
}

function playWheelTone(frequency, gainValue = 0.09, duration = 0.035, delay = 0) {
  const audio = ensureWheelAudio();
  if (!audio || audio.state !== "running") return;
  try {
    const startTime = Math.max(audio.currentTime, 0) + 0.002 + delay;
    const oscillator = audio.createOscillator();
    const gain = audio.createGain();
    oscillator.type = "triangle";
    oscillator.frequency.setValueAtTime(frequency, startTime);
    oscillator.frequency.exponentialRampToValueAtTime(Math.max(90, frequency * .72), startTime + duration);
    oscillator.onended = () => { oscillator.disconnect(); gain.disconnect(); };
    gain.gain.setValueAtTime(.0001, startTime);
    gain.gain.linearRampToValueAtTime(gainValue, startTime + .004);
    gain.gain.linearRampToValueAtTime(.0001, startTime + duration);
    oscillator.connect(gain).connect(audio.destination);
    oscillator.start(startTime);
    oscillator.stop(startTime + duration + .01);
  } catch (_) {}
}

function playWheelTick(progress) {
  const audio = ensureWheelAudio();
  if (!audio || audio.state !== "running" || audio.currentTime - lastWheelSoundTime < .028) return;
  lastWheelSoundTime = audio.currentTime;
  try {
    if (!wheelSpinOutput) {
      const compressor = audio.createDynamicsCompressor();
      const output = audio.createGain();
      compressor.threshold.value = -18;
      compressor.knee.value = 12;
      compressor.ratio.value = 5;
      compressor.attack.value = .003;
      compressor.release.value = .14;
      output.gain.value = .9;
      compressor.connect(output).connect(audio.destination);
      wheelSpinOutput = compressor;
    }

    // Long, overlapping decays make the fast part feel like a real wheel
    // rattling across pegs. The shared compressor controls their combined
    // level without stopping or shortening any individual tick.
    const startTime = audio.currentTime + .002;
    const duration = .12;
    const oscillator = audio.createOscillator();
    const gain = audio.createGain();
    oscillator.type = "triangle";
    oscillator.frequency.setValueAtTime(620 - progress * 130, startTime);
    oscillator.frequency.exponentialRampToValueAtTime(230, startTime + duration);
    gain.gain.setValueAtTime(.0001, startTime);
    gain.gain.linearRampToValueAtTime(.14, startTime + .002);
    gain.gain.exponentialRampToValueAtTime(.0001, startTime + duration);
    oscillator.connect(gain).connect(wheelSpinOutput);
    oscillator.onended = () => { oscillator.disconnect(); gain.disconnect(); };
    oscillator.start(startTime);
    oscillator.stop(startTime + duration + .01);
  } catch (_) {}
}

function playWheelResult() {
  // Let the result cut through the layered spin texture while preserving the
  // original two-note voicing, timing, and pitch fall.
  playWheelTone(440, .19, .1);
  playWheelTone(587.33, .16, .16, .085);
}

function animateWheel(wheel) {
  cancelAnimationFrame(wheelAnimation);
  const target = wheelTargetRotation(wheel);
  const arc = Math.PI * 2 / Math.max(1, wheel.items?.length || 0);
  lastWheelTick = -1;
  const frame = () => {
    if (!activeWheel || activeWheel.id !== wheel.id) return;
    const serverNow = Date.now() - wheelServerOffsetMs;
    const raw = (serverNow - Number(wheel.startsAtUnixMs)) / Math.max(1, Number(wheel.durationMs));
    const progress = preferences.reduceMotion ? 1 : Math.max(0, Math.min(1, raw));
    const eased = 1 - Math.pow(1 - progress, 5);
    wheelRotation = target * eased;
    drawWheel(wheel, wheelRotation);
    const tick = Math.floor(Math.abs(wheelRotation) / arc);
    if (!preferences.reduceMotion && tick !== lastWheelTick) {
      if (lastWheelTick >= 0) playWheelTick(progress);
      lastWheelTick = tick;
    }
    const candidate = wheelCandidate(wheel, wheelRotation);
    if (candidate) $("wheel-candidate").textContent = candidate.label;
    if (progress < 1) wheelAnimation = requestAnimationFrame(frame);
  };
  frame();
}

function showWheel(wheel, serverNowUnixMs = Date.now()) {
  if (!wheel) return;
  wheelServerOffsetMs = Date.now() - Number(serverNowUnixMs || Date.now());
  const dialog = $("wheel-dialog");
  if (!dialog.open) dialog.showModal();
  $("wheel-count").textContent = `${wheel.items?.length || 0} queue item${wheel.items?.length === 1 ? "" : "s"}`;
  if (wheel.phase === "started") {
    const changed = activeWheel?.id !== wheel.id;
    activeWheel = wheel;
    $("wheel-status").textContent = `${wheel.requesterName || "A participant"} is spinning…`;
    $("wheel-spin-again").disabled = true;
    if (changed) animateWheel(wheel);
  } else if (wheel.phase === "completed") {
    const wasSpinning = activeWheel?.id === wheel.id && activeWheel?.phase === "started";
    activeWheel = wheel;
    cancelAnimationFrame(wheelAnimation);
    wheelRotation = wheelTargetRotation(wheel);
    drawWheel(wheel, wheelRotation, wheel.winner);
    $("wheel-status").textContent = "Selected for the room";
    $("wheel-candidate").textContent = wheel.items?.[wheel.winner]?.label || "Queue item selected";
    $("wheel-spin-again").disabled = !canControl();
    if (wasSpinning) playWheelResult();
  } else if (wheel.phase === "cancelled") {
    activeWheel = null;
    cancelAnimationFrame(wheelAnimation);
    dialog.close();
    showToast(wheel.reason || "The wheel spin was cancelled", "error");
  }
}

function openWheelWindow() {
  ensureWheelAudio();
  const dialog = $("wheel-dialog");
  if (!dialog.open) dialog.showModal();
  const items = (snapshot?.playlist?.items || []).map(({ id, label }) => ({ id, label }));
  if (!activeWheel) {
    const preview = { items, winner: 0, turns: 0, visualSeed: 0 };
    wheelRotation = 0;
    drawWheel(preview, 0);
    $("wheel-status").textContent = items.length >= 2 ? "The server chooses one item for everyone" : "Add at least two queue items";
    $("wheel-candidate").textContent = items.length ? "Ready to spin" : "Queue is empty";
    $("wheel-count").textContent = `${items.length} queue item${items.length === 1 ? "" : "s"}`;
    $("wheel-spin-again").disabled = !canControl() || items.length < 2;
  }
}

function formatBytes(bytes) {
  if (!bytes) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** index).toFixed(index > 1 ? 1 : 0)} ${units[index]}`;
}

function renderParticipants(referenceMedia) {
  const me = self();
  const list = $("participants");
  if (!list || !snapshot) return;
  document.querySelectorAll(".participant-custom-menu").forEach((menu) => menu.remove());
  if ($("people-count")) $("people-count").textContent = snapshot.participants.length;

  list.replaceChildren(...snapshot.participants.map((person) => {
    const item = document.createElement("li");
    item.className = "participant-row";
    item.dataset.id = person.id;

    const avatar = document.createElement("span");
    avatar.className = "avatar-badge";
    avatar.textContent = person.name.slice(0, 1).toUpperCase();

    const details = document.createElement("div");
    details.className = "participant-details";

    const name = document.createElement("span");
    name.className = "participant-name";
    name.textContent = person.id === snapshot.selfId ? `${person.name} (you)` : person.name;

    const meta = document.createElement("span");
    meta.className = "participant-status";
    const mediaState = !person.media ? "no media" : referenceMedia && person.media.fingerprint !== referenceMedia.fingerprint ? "different media" : "synced";
    meta.textContent = `${capitalize(person.role)} · ${mediaState}`;

    details.append(name, meta);
    item.append(avatar, details);

    if (me?.role === "owner" && person.role !== "owner") {
      const role = document.createElement("select");
      role.className = "participant-role-select";
      role.setAttribute("aria-label", `Role for ${person.name}`);
      role.innerHTML = '<option value="member">Member</option><option value="moderator">Moderator</option>';
      role.value = person.role;
      role.onchange = () => invoke("SetRole", person.id, role.value).catch(showError);
      item.append(role);
      enhanceSelect(role);
    }
    return item;
  }));
}

function capitalize(value) { return value ? value[0].toUpperCase() + value.slice(1) : ""; }

function renderPlaylist() {
  if (!snapshot) return;
  const allowed = canControl(), items = snapshot.playlist.items || [];
  $("playlist-count").textContent = items.length;
  $("playlist-empty").classList.toggle("hidden", items.length > 0);
  $("undo-playlist").disabled = !allowed || playlistHistory.length === 0;
  $("shuffle-playlist").disabled = !allowed || items.length < 2;
  $("shuffle-all").disabled = !allowed || items.length < 2;
  $("save-playlist-file").disabled = items.length === 0;
  $("spin-wheel").disabled = !allowed || items.length < 2 || activeWheel?.phase === "started";
  $("previous-media").disabled = !allowed || snapshot.playlist.selected <= 0;
  $("next-media").disabled = !allowed || snapshot.playlist.selected < 0 || snapshot.playlist.selected >= items.length - 1;

  const list = $("playlist");
  if (draggedPlaylistIndex >= 0) return;
  list.replaceChildren(...items.map((item, index) => {
    const row = document.createElement("li");
    row.className = `queue-item ${index === snapshot.playlist.selected ? "active" : ""}`;
    row.draggable = false;
    row.dataset.index = String(index);

    const handle = document.createElement("span");
    handle.className = "queue-drag-handle";
    handle.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="9" cy="6" r="1" fill="currentColor" stroke="none"/><circle cx="15" cy="6" r="1" fill="currentColor" stroke="none"/><circle cx="9" cy="12" r="1" fill="currentColor" stroke="none"/><circle cx="15" cy="12" r="1" fill="currentColor" stroke="none"/><circle cx="9" cy="18" r="1" fill="currentColor" stroke="none"/><circle cx="15" cy="18" r="1" fill="currentColor" stroke="none"/></svg>';
    handle.title = "Drag to reorder";

    const indexBadge = document.createElement("span");
    indexBadge.className = "queue-index";
    indexBadge.textContent = String(index + 1);

    const metaCol = document.createElement("div");
    metaCol.className = "queue-meta-col";

    const label = document.createElement("span");
    label.className = "queue-title";
    label.textContent = item.label;
    label.title = item.label;

    const local = item.url || availability[item.id];
 const receiving = streamState.state === "active" && (snapshot.streamOffers || []).some((offer) => offer.id === streamState.offerId && offer.media?.fingerprint === item.media?.fingerprint);
 row.classList.toggle("needs-file", !local && !receiving);
    const status = document.createElement("span");
    status.className = `queue-status-tag ${local || receiving ? "available" : "missing"}`;
    status.textContent = item.url ? "Online video" : local ? "Ready to watch" : receiving ? "Watching shared file" : "Choose a local copy";

    metaCol.append(label, status);

    const duration = document.createElement("span");
    duration.className = "queue-duration";
    duration.textContent = item.media?.durationSeconds ? formatTime(item.media.durationSeconds) : "--:--";

    const actions = document.createElement("div");
    actions.className = "queue-row-actions";

    if (isYouTubeURL(item.url)) {
      const qualityButton = actionButton("Quality", () => showYouTubeQualityMenu(qualityButton, item.url), !allowed, `YouTube quality: ${savedYouTubeQuality(item.url) ? `${savedYouTubeQuality(item.url)}p` : "Auto"}`);
      actions.append(qualityButton);
    }
    if (!local && item.media) {
      actions.append(actionButton("Locate file", () => locateItem(item.id), false, "Choose your matching copy"));
    }
    const ownOffer = (snapshot.streamOffers || []).find((offer) => offer.providerId === snapshot.selfId);
    const itemOwnOffer = ownOffer?.media?.fingerprint === item.media?.fingerprint ? ownOffer : null;
    if (itemOwnOffer) {
      actions.append(actionButton("Stop sharing", () => invoke("StopOfferingStream").then(() => showToast("File sharing stopped")), false, `Stop sharing · ${itemOwnOffer.viewerCount} viewer${itemOwnOffer.viewerCount === 1 ? "" : "s"}`));
    } else if (local && !item.url && streamingAvailable && friendsMissing(item.media) && !ownOffer) {
      actions.append(actionButton("Share file", () => invoke("OfferPlaylistStream", item.id).then(() => showToast("Sharing started; friends without the file will connect automatically")).catch(showError), false, "Automatically stream your copy to friends who need it"));
    }
    if (isPlaylistItemPlayable(item) && index !== snapshot.playlist.selected) actions.append(actionButton("Play", () => playPlaylist(index), !allowed, "Play now"));
    actions.append(actionButton("×", () => removePlaylist(index), !allowed, "Remove from queue"));

    row.append(handle, indexBadge, metaCol, duration, actions);

    row.ondblclick = () => { if (allowed && isPlaylistItemPlayable(item)) playPlaylist(index); };
    handle.onpointerdown = (event) => beginPlaylistReorder(event, row, index, allowed);

    return row;
  }));
}

function beginPlaylistReorder(event, sourceRow, sourceIndex, allowed) {
  if (!allowed || event.button !== 0) return;
  event.preventDefault();
  event.stopPropagation();
  draggedPlaylistIndex = sourceIndex;
  let targetIndex = sourceIndex;
  const startY = event.clientY;
  const rowHeight = sourceRow.getBoundingClientRect().height;
  sourceRow.classList.add("dragging");
  const clearPreview = () => document.querySelectorAll(".queue-item").forEach((node) => {
    node.classList.remove("drag-over");
    node.style.transform = "";
  });
  const updatePreview = (pointerY, nextTarget) => {
    document.querySelectorAll(".queue-item").forEach((node) => {
      const index = Number(node.dataset.index);
      node.style.transform = "";
      if (node === sourceRow) node.style.transform = `translateY(${pointerY - startY}px) scale(.99)`;
      else if (sourceIndex < nextTarget && index > sourceIndex && index <= nextTarget) node.style.transform = `translateY(${-rowHeight}px)`;
      else if (sourceIndex > nextTarget && index >= nextTarget && index < sourceIndex) node.style.transform = `translateY(${rowHeight}px)`;
    });
  };
  const move = (moveEvent) => {
    const target = document.elementFromPoint(moveEvent.clientX, moveEvent.clientY)?.closest?.(".queue-item");
    if (target && $("playlist").contains(target)) {
      document.querySelectorAll(".queue-item.drag-over").forEach((node) => node.classList.remove("drag-over"));
      target.classList.add("drag-over");
      targetIndex = Number(target.dataset.index);
    }
    updatePreview(moveEvent.clientY, targetIndex);
  };
  const finish = (finishEvent) => {
    document.removeEventListener("pointermove", move, true);
    document.removeEventListener("pointerup", finish, true);
    document.removeEventListener("pointercancel", finish, true);
    sourceRow.classList.remove("dragging");
    clearPreview();
    draggedPlaylistIndex = -1;
    if (targetIndex !== sourceIndex) reorderPlaylist(sourceIndex, targetIndex);
  };
  document.addEventListener("pointermove", move, true);
  document.addEventListener("pointerup", finish, true);
  document.addEventListener("pointercancel", finish, true);
}

async function youtubeInfo(source) {
  if (!isYouTubeURL(source)) return null;
  if (!youtubeInfoCache.has(source)) {
    const request = invoke("YouTubeInfo", source).catch((error) => {
      youtubeInfoCache.delete(source);
      throw error;
    });
    youtubeInfoCache.set(source, request);
  }
  return youtubeInfoCache.get(source);
}

function savedYouTubeQuality(source) {
  return Number(preferences.youtubeQualities?.[source] || 0);
}

async function prepareYouTubeSource(source) {
  if (!isYouTubeURL(source)) return null;
  const info = await youtubeInfo(source);
  await invoke("SetYouTubeQuality", source, savedYouTubeQuality(source));
  return info;
}

async function playPlaylist(index) {
  const item = snapshot?.playlist?.items?.[index];
  if (!item || !canControl()) return;
  try {
    if (item.url) await prepareYouTubeSource(item.url);
    activeSource = item.url || "";
    await invoke("SelectPlaylist", index);
    await invoke("SetPaused", false);
  } catch (error) { showError(error); }
}

function friendsMissing(media) {
  return Boolean(media?.fingerprint && (snapshot?.participants || []).some((person) => person.id !== snapshot.selfId && !(person.availableMedia || []).includes(media.fingerprint)));
}

function isPlaylistItemPlayable(item) {
  if (!item) return false;
  if (item.url || availability[item.id]) return true;
  return streamState.state === "active" && (snapshot?.streamOffers || []).some((offer) => offer.id === streamState.offerId && offer.media?.fingerprint === item.media?.fingerprint);
}

function actionButton(label, action, disabled = false, title = "") {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "queue-action";
  if (label === "×") {
    button.classList.add("queue-action-icon", "queue-action-danger");
    button.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h16M9 7V4h6v3M18 7l-1 13H7L6 7M10 11v5M14 11v5"/></svg>';
    button.setAttribute("aria-label", title || "Remove from queue");
  } else if (label === "Play") {
    button.classList.add("queue-action-icon");
    button.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 9 6-9 6V6Z" fill="currentColor" stroke="none"/></svg>';
    button.setAttribute("aria-label", title || "Play now");
  } else if (label === "Quality") {
    button.classList.add("queue-action-icon");
    button.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 7h10M18 7h2M4 12h2M10 12h10M4 17h7M15 17h5"/><circle cx="16" cy="7" r="2"/><circle cx="8" cy="12" r="2"/><circle cx="13" cy="17" r="2"/></svg>';
    button.setAttribute("aria-label", title || "YouTube quality");
  } else if (label === "Locate file") {
    button.classList.add("queue-action-icon");
    button.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6.5h7l2 2h9v9.5a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6.5Z"/><path d="m9 14 2 2 4-4"/></svg>';
    button.setAttribute("aria-label", title || "Locate matching local file");
  } else if (label === "Share file") {
    button.classList.add("queue-action-icon");
    button.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 16V4M7 9l5-5 5 5"/><path d="M5 13v6h14v-6"/></svg>';
    button.setAttribute("aria-label", title || "Share this playlist file");
  } else if (label === "Stop sharing") {
    button.classList.add("queue-action-icon", "queue-action-danger");
    button.innerHTML = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><rect x="6" y="6" width="12" height="12" rx="2"/></svg>';
    button.setAttribute("aria-label", title || "Stop sharing this file");
  } else {
    button.textContent = label;
    if (label === "Playing") button.classList.add("queue-action-current");
  }
  button.disabled = disabled;
  button.title = title;
  button.onclick = () => withButtonLoading(button, action).catch(showError);
  return button;
}

async function showYouTubeQualityMenu(button, source) {
  const rect = button.getBoundingClientRect();
  const anchor = { clientX: rect.left, clientY: rect.bottom + 4 };
  showContextMenu(anchor, [menuButton("Loading qualities…", () => {}, { disabled: true })]);
  let info;
  try { info = await youtubeInfo(source); }
  catch (error) { closePopovers(); showError(error); return; }
  const current = savedYouTubeQuality(source);
  const choices = [{ height: 0, label: "Auto" }, ...(info.qualities || []).map((quality) => ({
    height: quality.height,
    label: `${quality.height}p${quality.fps > 30 ? quality.fps : ""}${quality.hdr ? " HDR" : ""}`
  }))];
  showContextMenu(anchor, choices.map((choice) => menuButton(choice.label, async () => {
    try {
      closePopovers();
      savePreferences({ youtubeQualities: { ...(preferences.youtubeQualities || {}), [source]: choice.height } });
      if (selectedSourceURL() === source) {
        showToast(`Switching YouTube quality to ${choice.label}…`);
        await invoke("ChangeYouTubeQuality", source, choice.height);
        showToast("YouTube quality changed");
      } else {
        await invoke("SetYouTubeQuality", source, choice.height);
        showToast(`YouTube quality set to ${choice.label}`);
      }
      renderPlaylist();
    } catch (error) { showError(error); }
  }, { checked: choice.height === current })));
}

function selectedSourceURL() {
  const selected = snapshot?.playlist?.selected ?? -1;
  return selected >= 0 ? snapshot.playlist.items[selected]?.url || "" : activeSource;
}

async function updatePlaylist(items, remember = true) {
  if (remember && snapshot) {
    playlistHistory.push(playlistInputs());
    if (playlistHistory.length > 20) playlistHistory.shift();
  }
  try { await invoke("SetPlaylist", items); }
  catch (error) { if (remember) playlistHistory.pop(); throw error; }
}

async function removePlaylist(index) { const items = playlistInputs(); items.splice(index, 1); try { await updatePlaylist(items); } catch (error) { showError(error); } }
async function reorderPlaylist(from, to) { if (from === to) return; const items = playlistInputs(), [item] = items.splice(from, 1); items.splice(to, 0, item); try { await updatePlaylist(items); } catch (error) { showError(error); } }
async function locateItem(id) { try { const path = await invoke("ChooseMediaFile"); if (!path) return; await invoke("LocatePlaylistItem", id, path); await refreshAvailability(); showToast("Matching file located"); } catch (error) { showError(error); } }

function shuffled(items) {
  const result = [...items];
  for (let index = result.length - 1; index > 0; index--) {
    const target = Math.floor(Math.random() * (index + 1));
    [result[index], result[target]] = [result[target], result[index]];
  }
  return result;
}

function playlistSourceLabel(source) {
  try {
    const parsed = new URL(source);
    return decodeURIComponent(parsed.pathname.split("/").filter(Boolean).pop() || parsed.hostname);
  } catch (_) {
    return basename(source);
  }
}

async function loadPlaylistFromFile() {
  closePopovers();
  try {
    const sources = await invoke("LoadPlaylistFile");
    if (!sources?.length) return;
    const inputs = sources.map((source) => ({ id: "", label: playlistSourceLabel(source), source, url: "", media: null }));
    await updatePlaylist(inputs);
    showToast(`Loaded ${inputs.length} queue item${inputs.length === 1 ? "" : "s"}`);
  } catch (error) { showError(error); }
}

function renderServerStatus() {
  $("stop-server")?.classList.toggle("hidden", !hosted.running);
  renderConnectionInfo();
}

function renderConnectionInfo() {
  const serverStr = hosted.running
    ? `Local · ${hosted.listenAddress || ":8999"}`
    : (lastConnectionRequest?.server ? `${lastConnectionRequest.server}` : (snapshot ? "Remote Faro server" : "—"));
  const certStr = hosted.fingerprint
    ? `${String(hosted.fingerprint).slice(0, 20)}…`
    : (snapshot ? "Pinned by invite" : "—");
  const playerStr = lastConnectionRequest?.player || preferences.player || "—";
  const roomStr = snapshot?.room?.id ? `#${snapshot.room.id}` : "—";
  const peopleStr = snapshot?.participants ? `${snapshot.participants.length} user${snapshot.participants.length === 1 ? "" : "s"}` : "—";
  const statusStr = capitalize(connectionState);

  const values = {
    "info-client-version": `Faro ${appVersion}`,
    "info-player": playerStr,
    "info-room": roomStr,
    "info-connection": statusStr,
    "info-server": serverStr,
    "info-certificate": certStr,
    "stats-server-val": serverStr,
    "stats-room-val": roomStr,
    "stats-player-val": playerStr,
    "stats-people-val": peopleStr,
    "stats-cert-val": certStr,
    "stats-client-val": `Faro ${appVersion}`
  };
  for (const [id, value] of Object.entries(values)) if ($(id)) $(id).textContent = value;

  const statusPill = $("stats-status-pill");
  if (statusPill) {
    statusPill.textContent = statusStr;
    statusPill.className = `stats-status-pill ${connectionState}`;
  }
}

function setConnection(status) {
  connectionState = status?.state || "disconnected";
  const element = $("connection-state");
  if (!element) return;
  element.className = `connection-status ${connectionState}`;
  const text = connectionState === "reconnecting" ? `Reconnecting${status.attempt ? ` (${status.attempt})` : ""}` : connectionState === "connected" ? "Connected" : "Disconnected";
  element.querySelector(".status-text").textContent = text;
  $("connection-btn")?.setAttribute("aria-label", `${text}; server stats`);
  $("leave-room")?.setAttribute("aria-label", "Room exit options");
  if (status?.message && connectionState !== "connected") element.title = status.message;
  renderConnectionInfo();
}

async function enterRoom(request) {
  lastConnectionRequest = request;
  activeSource = "";
  await invoke("Connect", request);
  snapshot = normalizeSnapshot(await invoke("Snapshot"));
  hosted = await invoke("ServerStatus");
  setConnection({ state: "connected" });
  render();
  await invoke("SetSponsorBlockEnabled", preferences.sponsorBlock !== false);
  // Indexing can hash thousands of files. The room is already usable, so keep
  // discovery in the background and refresh availability as results arrive.
  void indexSavedDirectories();
}

function switchConnectMode(mode) {
  const join = mode === "join";
  $("join-tab").classList.toggle("active", join);
  $("host-tab").classList.toggle("active", !join);
  $("join-tab").setAttribute("aria-selected", String(join));
  $("host-tab").setAttribute("aria-selected", String(!join));
  $("join-form").classList.toggle("hidden", !join);
  $("host-form").classList.toggle("hidden", join);
}

async function copyText(value, success) {
  try {
    if (wails?.Clipboard) await wails.Clipboard.SetText(value);
    else await navigator.clipboard.writeText(value);
    showToast(success);
  } catch (error) { showError(error); }
}

async function copyInvite(owner = false) {
  try {
    await copyText(await invoke(owner ? "OwnerInvite" : "ParticipantInvite"), owner ? "Owner recovery invite copied" : "Invite link copied");
  } catch (error) { showError(error); }
}

async function addMediaPaths(paths, playImmediately = false) {
  if (!snapshot || !canControl() || !paths?.length) return;
  try {
    const expanded = hasBackend ? await invoke("ExpandMediaPaths", paths) : paths;
    if (!expanded.length) return;
    if (playImmediately && expanded.length === 1) {
      const next = [...playlistInputs(), { id: "", label: basename(expanded[0]), source: expanded[0], url: "", media: null }];
      await updatePlaylist(next);
      await invoke("SelectPlaylist", next.length - 1);
      showToast(`Opening ${basename(expanded[0])}`);
      return;
    }
    const additions = expanded.map((source) => ({ id: "", label: basename(source), source, url: "", media: null }));
    await updatePlaylist([...playlistInputs(), ...additions]);
    showToast(`${additions.length} item${additions.length === 1 ? "" : "s"} added to queue`);
  } catch (error) { showError(error); }
}

async function chooseFiles(button) {
  const task = async () => { await addMediaPaths(await invoke("ChooseMediaFiles")); };
  try {
    if (button) await withButtonLoading(button, task, "Adding");
    else await task();
  } catch (error) { showError(error); }
}

function positionPopover(popover, anchor) {
  if (!popover || !anchor) return;
  const rect = anchor.getBoundingClientRect();
  const width = popover.offsetWidth;
  const left = rect.left + width > innerWidth - 8 ? rect.right - width : rect.left;
  popover.style.left = `${Math.max(8, Math.min(innerWidth - width - 8, left))}px`;
  popover.style.top = `${Math.max(8, Math.min(innerHeight - popover.offsetHeight - 8, rect.bottom + 4))}px`;
}

function togglePopover(popover, anchor) {
  if (!popover || !anchor) return;
  const opening = popover.classList.contains("hidden");
  closePopovers();
  if (!opening) return;
  popover.classList.remove("hidden");
  positionPopover(popover, anchor);
}
function closePopovers() {
  document.querySelectorAll(".popover, .popover-menu").forEach((node) => node.classList.add("hidden"));
}

function menuButton(label, action, { checked = false, disabled = false, danger = false } = {}) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `popover-btn ${danger ? "danger-item" : ""}`;
  button.disabled = disabled;
  button.setAttribute("role", "menuitem");
  const mark = document.createElement("span");
  mark.className = "menu-check";
  mark.textContent = checked ? "✓" : "";
  const text = document.createElement("span");
  text.textContent = label;
  button.append(mark, text);
  button.onclick = action;
  return button;
}

function enhanceSelect(select) {
  if (!select || select.dataset.enhanced === "true") return;
  select.dataset.enhanced = "true";
  select.classList.add("native-select-source");
  const wrapper = document.createElement("div");
  wrapper.className = `custom-select ${select.classList.contains("select-compact") || select.classList.contains("participant-role-select") ? "compact" : ""}`;
  select.parentNode.insertBefore(wrapper, select);
  wrapper.append(select);
  const trigger = document.createElement("button");
  trigger.type = "button";
  trigger.className = "custom-select-trigger";
  trigger.setAttribute("aria-haspopup", "menu");
  trigger.innerHTML = '<span class="custom-select-label"></span><svg viewBox="0 0 12 12" aria-hidden="true"><path d="m3 4.5 3 3 3-3"/></svg>';
  wrapper.append(trigger);
  const menu = document.createElement("div");
  menu.className = "popover-menu custom-select-menu hidden";
  if (select.classList.contains("participant-role-select")) menu.classList.add("participant-custom-menu");
  menu.setAttribute("role", "menu");
  (select.closest("dialog") || document.body).append(menu);

  const sync = () => {
    const option = select.options[select.selectedIndex] || select.options[0];
    trigger.querySelector(".custom-select-label").textContent = option?.textContent || "Select";
    trigger.disabled = select.disabled;
    trigger.title = select.title || option?.textContent || "";
  };
  const rebuild = () => {
    menu.replaceChildren(...[...select.options].map((option) => menuButton(option.textContent, () => {
      select.value = option.value;
      select.dispatchEvent(new Event("change", { bubbles: true }));
      sync();
      closePopovers();
    }, { checked: option.value === select.value, disabled: option.disabled })));
  };
  trigger.onclick = (event) => {
    event.stopPropagation();
    rebuild();
    togglePopover(menu, trigger);
  };
  select._syncCustomSelect = sync;
  sync();
}

function enhanceAllSelects() {
  document.querySelectorAll("select").forEach(enhanceSelect);
}

function askConfirmation(title, message, acceptLabel = "Continue") {
  const dialog = $("confirm-dialog");
  $("confirm-title").textContent = title;
  $("confirm-message").textContent = message;
  $("confirm-accept").textContent = acceptLabel;
  dialog.showModal();
  return new Promise((resolve) => dialog.addEventListener("close", () => resolve(dialog.returnValue === "accept"), { once: true }));
}

async function leaveRoom() {
  try {
    if (preferences.pauseOnLeave && canControl() && snapshot && !snapshot.playback.paused) await invoke("SetPaused", true);
    await invoke("Disconnect");
  } catch (error) { showError(error); }
  snapshot = null; timeline = []; playlistHistory = [];
  streamingAvailable = false;
  streamState = { state: "idle", offerId: "", route: "" };
  timelineSegments = []; timelineSegmentKey = ""; timelineSegmentsRetryKey = "";
  isScrubbing = false;
  seekCommandPending = false;
  clearTimeout(seekReleaseTimer);
  $("settings-dialog").close();
  if ($("wheel-dialog").open) $("wheel-dialog").close();
  activeWheel = null;
  cancelAnimationFrame(wheelAnimation);
  $("room-view").classList.add("hidden");
  $("connect-view").classList.remove("hidden");
  document.body.classList.remove("room-connected");
  $("window-context").textContent = "Watch together";
  setConnection({ state: "disconnected" });
}

// Client-side window chrome uses Wails v3's runtime modules directly.
document.body.dataset.platform = wails?.System?.IsMac() ? "mac" : wails?.System?.IsWindows() ? "windows" : /Mac/i.test(navigator.platform) ? "mac" : /Win/i.test(navigator.platform) ? "windows" : "linux";

$("window-minimise").onclick = () => wails?.Window?.Minimise();
$("window-maximise").onclick = () => wails?.Window?.ToggleMaximise();
$("window-close").onclick = () => wails?.Window?.Close();
$("window-titlebar").ondblclick = (event) => {
  if (!event.target.closest("button")) $("window-maximise").click();
};
document.querySelector(".app-command-bar")?.addEventListener("dblclick", (event) => {
  if (!event.target.closest("button, input, select")) $("window-maximise").click();
});

// Connection forms wiring
$("join-tab").onclick = () => switchConnectMode("join");
$("host-tab").onclick = () => switchConnectMode("host");
$("join-form").onsubmit = async (event) => {
  event.preventDefault(); $("join-error").textContent = "";
  const button = event.submitter || $("join-form").querySelector("[type=submit]");
  await withButtonLoading(button, async () => {
    try { rememberConnectPreferences("join"); await enterRoom(connectionRequest("join", $("join-invite").value.trim())); }
    catch (error) { $("join-error").textContent = error?.message || String(error); }
  }, "Connecting");
};
$("host-mode").onchange = () => {
  const advanced = $("host-mode").value === "advanced";
  document.querySelectorAll(".host-advanced").forEach((field) => field.classList.toggle("hidden", !advanced));
  $("host-public").required = advanced;
  $("host-listen").required = advanced;
  $("host-mode-hint").textContent = advanced ? "Use a reachable server address and configure your router or firewall if needed." : "No account or port forwarding. Keep Faro open while your friends are watching.";
};
$("host-mode").onchange();
$("host-form").onsubmit = async (event) => {
  event.preventDefault(); $("host-error").textContent = "";
  const button = event.submitter || $("host-form").querySelector("[type=submit]");
  await withButtonLoading(button, async () => {
    try {
      rememberConnectPreferences("host");
      hosted = await invoke("StartServer", {
        mode: $("host-mode").value,
        listenAddress: $("host-listen").value.trim(),
        publicHost: $("host-public").value.trim(),
        room: $("host-room").value.trim(),
        protected: $("host-protected").checked
      });
      await enterRoom(connectionRequest("host", hosted.localInvite));
    } catch (error) { $("host-error").textContent = error?.message || String(error); try { await invoke("StopServer"); } catch (_) {} }
  }, "Starting");
};

// Room controls wiring
if ($("connection-btn")) {
  $("connection-btn").onclick = (event) => {
    event.stopPropagation();
    renderConnectionInfo();
    togglePopover($("server-stats-menu"), $("connection-btn"));
  };
}
if ($("stats-open-session")) {
  $("stats-open-session").onclick = () => {
    closePopovers();
    renderConnectionInfo();
    document.querySelectorAll("[data-settings]").forEach((node) => node.classList.toggle("active", node.dataset.settings === "session"));
    document.querySelectorAll("[data-page]").forEach((page) => page.classList.toggle("hidden", page.dataset.page !== "session"));
    $("settings-dialog").showModal();
  };
}
$("leave-room").onclick = (event) => { event.stopPropagation(); togglePopover($("session-menu"), $("leave-room")); };
$("disconnect").onclick = () => { closePopovers(); leaveRoom().catch(showError); };
$("settings-leave").onclick = () => withButtonLoading($("settings-leave"), leaveRoom, "Leaving").catch(showError);
$("reconnect").onclick = async () => { if (!lastConnectionRequest) return; await withButtonLoading($("reconnect"), async () => { try { await enterRoom(lastConnectionRequest); showToast("Connection restored"); } catch (error) { showError(error); } }, "Reconnecting"); };
$("room-mode").onchange = (event) => invoke("SetRoomMode", event.target.value).catch(showError);
$("pause").onclick = () => invoke("SetPaused", !snapshot.playback.paused).catch(showError);
const positionControl = $("position");
positionControl.onpointerdown = () => {
  isScrubbing = true;
  clearTimeout(seekReleaseTimer);
};
positionControl.oninput = (event) => {
  isScrubbing = true;
  clearTimeout(seekReleaseTimer);
  $("current-time").textContent = formatTime(Number(event.target.value));
  updateScrubberProgress(Number(event.target.value));
};
positionControl.onchange = async (event) => {
  isScrubbing = true;
  seekCommandPending = true;
  clearTimeout(seekReleaseTimer);
  const duration = playbackDuration();
  if (!duration) {
    seekCommandPending = false;
    releaseScrubber(0);
    return;
  }
  const target = clamp(event.target.value, 0, duration);
  event.target.value = String(target);
  try { await invoke("Seek", target); }
  catch (error) { showError(error); }
  finally {
    seekCommandPending = false;
    releaseScrubber();
  }
};
positionControl.onpointerup = () => { if (!seekCommandPending) releaseScrubber(); };
positionControl.onpointercancel = () => releaseScrubber(0);
positionControl.onblur = () => { if (!seekCommandPending) releaseScrubber(); };
$("back-ten").onclick = () => invoke("Seek", Math.max(0, projectedPlaybackPosition() - Number(preferences.skipSeconds || 10))).catch(showError);
$("forward-ten").onclick = () => {
  const duration = playbackDuration();
  const target = projectedPlaybackPosition() + Number(preferences.skipSeconds || 10);
  invoke("Seek", duration ? Math.min(duration, target) : target).catch(showError);
};
$("rate-decrease").onclick = () => stepPlaybackRate(-1);
$("rate-increase").onclick = () => stepPlaybackRate(1);
$("playback-rate").onclick = () => invoke("SetRate", 1).catch(showError);
$("previous-media").onclick = () => playPlaylist(snapshot.playlist.selected - 1);
$("next-media").onclick = () => playPlaylist(snapshot.playlist.selected + 1);

$("copy-invite").onclick = () => hosted.running && hosted.shareInvite ? copyText(hosted.shareInvite, "Invite copied") : copyInvite(false);
$("settings-copy-invite").onclick = () => $("copy-invite").click();
$("owner-invite").onclick = () => copyInvite(true);
$("settings-owner-invite").onclick = () => copyInvite(true);
$("stop-server").onclick = async () => { closePopovers(); try { await invoke("StopServer"); await leaveRoom(); hosted = { running: false }; } catch (error) { showError(error); } };

// Playlist wiring
$("add-file").onclick = () => chooseFiles($("add-file"));
$("empty-add-file").onclick = () => chooseFiles($("empty-add-file"));

const streamDrawer = $("stream-drawer");
$("add-stream-btn").onclick = () => {
  streamDrawer.classList.toggle("hidden");
  if (!streamDrawer.classList.contains("hidden")) {
    $("playlist-source").focus();
  }
};
$("close-stream-drawer").onclick = () => streamDrawer.classList.add("hidden");

$("add-playlist").onclick = async () => {
  const source = $("playlist-source").value.trim();
  if (!source || addingPlaylistURL) return;
  const button = $("add-playlist");
  const input = $("playlist-source");
  addingPlaylistURL = true;
  button.disabled = true;
  button.classList.add("is-loading");
  button.querySelector("span").textContent = "Adding";
  input.disabled = true;
  try {
    const info = await prepareYouTubeSource(source);
    const label = info?.title || playlistSourceLabel(source);
    await updatePlaylist([...playlistInputs(), { id: "", label, source, url: "", media: null }]);
    input.value = "";
    streamDrawer.classList.add("hidden");
  } catch (error) { showError(error); }
  finally {
    addingPlaylistURL = false;
    input.disabled = false;
    button.classList.remove("is-loading");
    button.querySelector("span").textContent = "Add";
    button.disabled = !canControl();
  }
};

$("add-folder").onclick = async () => {
  closePopovers();
  try {
    const directory = await invoke("ChooseMediaDirectory");
    if (!directory) return;
    await addMediaPaths([directory]);
  } catch (error) { showError(error); }
};
$("clear-playlist").onclick = async () => {
  closePopovers();
  if (await askConfirmation("Clear queue?", "This removes every item from the shared queue.", "Clear queue")) updatePlaylist([]).catch(showError);
};
$("undo-playlist").onclick = async () => {
  closePopovers();
  const previous = playlistHistory.pop();
  if (!previous) return;
  try { await updatePlaylist(previous, false); }
  catch (error) { playlistHistory.push(previous); showError(error); }
};
$("shuffle-playlist").onclick = () => withButtonLoading($("shuffle-playlist"), async () => {
  const items = playlistInputs(), selected = snapshot.playlist.selected, start = selected >= 0 ? selected + 1 : 0, tail = items.splice(start);
  await updatePlaylist([...items, ...shuffled(tail)]);
}, "Shuffling").catch(showError);
$("playlist-more").onclick = (event) => { event.stopPropagation(); togglePopover($("playlist-menu"), $("playlist-more")); };
$("add-stream-focus").onclick = () => {
  closePopovers();
  streamDrawer.classList.remove("hidden");
  $("playlist-source").focus();
};
$("save-playlist").onclick = () => { closePopovers(); copyText(snapshot.playlist.items.map((item) => item.url || item.label).join("\n"), "Playlist copied as text"); };
$("shuffle-all").onclick = () => { closePopovers(); updatePlaylist(shuffled(playlistInputs())).catch(showError); };
$("load-playlist-file").onclick = () => loadPlaylistFromFile();
$("save-playlist-file").onclick = async () => { closePopovers(); try { const path = await invoke("SavePlaylistFile"); if (path) showToast("Playlist saved"); } catch (error) { showError(error); } };
$("spin-wheel").onclick = () => { closePopovers(); openWheelWindow(); };
$("close-wheel").onclick = () => $("wheel-dialog").close();
$("wheel-spin-again").onclick = async () => {
  if (!snapshot || !canControl() || snapshot.playlist.items.length < 2) return;
  ensureWheelAudio();
  await withButtonLoading($("wheel-spin-again"), () => invoke("SpinPlaylistWheel"), "Spinning").catch(showError);
};
for (const eventName of ["pointerdown", "keydown", "click"]) {
  document.addEventListener(eventName, () => ensureWheelAudio(), { once: true, capture: true });
}

// Chat and library wiring
$("chat-form").onsubmit = async (event) => {
  event.preventDefault();
  const input = $("chat-message"), message = input.value.trim();
  if (!message) return;
  const button = event.submitter || $("chat-form").querySelector("[type=submit]");
  await withButtonLoading(button, async () => { await invoke("SendChat", message); input.value = ""; }).catch(showError);
};
$("clear-chat").onclick = () => { timeline = []; renderChat(); };
document.querySelectorAll("[data-chat-filter]").forEach((button) => {
  button.onclick = () => {
    chatFilter = button.dataset.chatFilter || "all";
    document.querySelectorAll("[data-chat-filter]").forEach((candidate) => {
      const active = candidate.dataset.chatFilter === chatFilter;
      candidate.classList.toggle("active", active);
      candidate.setAttribute("aria-pressed", String(active));
    });
    renderChat();
  };
});
$("add-media-directory").onclick = () => withButtonLoading($("add-media-directory"), async () => {
  try {
    const directory = await invoke("ChooseMediaDirectory");
    if (!directory) return;
    const dirs = [...new Set([...mediaDirectories(), directory])];
    localStorage.setItem("faro.mediaDirectories", JSON.stringify(dirs));
    renderMediaDirectories();
    const count = await invoke("IndexMediaDirectory", directory);
    await refreshAvailability();
    showToast(`Indexed ${count} media files`);
  } catch (error) { showError(error); }
}, "Indexing");

function addChat(chat) {
  timeline.push({ kind: "chat", value: chat, sentAtUnixMs: chat.sentAtUnixMs });
  if (timeline.length > 500) timeline = timeline.slice(-500);
  renderChat();
}

function addActivity(activity) {
  timeline.push({ kind: "activity", value: activity, sentAtUnixMs: activity.sentAtUnixMs });
  if (timeline.length > 500) timeline = timeline.slice(-500);
  renderChat();
}

function activityDescription(activity) {
  const name = activity.participantName || "Someone";
  switch (activity.action) {
    case "sponsorblock.skipped": return `SponsorBlock skipped a segment at ${formatTime(activity.positionSeconds)}`;
    case "playback.paused": return `${name} paused at ${formatTime(activity.positionSeconds)}`;
    case "playback.resumed": return `${name} resumed playback`;
    case "playback.seeked": {
      const delta = Number(activity.deltaSeconds || 0);
      if (Math.abs(delta) >= 1 && Math.abs(delta) <= 90) {
        return `${name} skipped ${delta > 0 ? "forward" : "back"} ${Math.round(Math.abs(delta))}s`;
      }
      return `${name} seeked to ${formatTime(activity.positionSeconds)}`;
    }
    case "playback.rate": return `${name} changed speed to ${Number(activity.rate || 1).toFixed(2).replace(/\.00$/, "")}×`;
    case "playlist.updated": return `${name} updated the queue · ${activity.itemCount} item${activity.itemCount === 1 ? "" : "s"}`;
    case "playlist.played": return `${name} played ${activity.itemLabel || "a queue item"}`;
    case "wheel.started": return `${name} spun the wheel`;
    case "wheel.completed": return `The wheel chose ${activity.itemLabel || "a queue item"}`;
    default: return `${name} changed the room`;
  }
}

function activityIcon(action) {
  if (action === "sponsorblock.skipped") return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m8 7 8 5-8 5V7Z"/><path d="M18 7v10"/></svg>';
  if (action === "playback.paused") return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 7v10M15 7v10"/></svg>';
  if (action === "playback.resumed" || action === "playlist.played") return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 9 6-9 6V6Z"/></svg>';
  if (action === "playback.seeked") return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12h14M8 9l-3 3 3 3M16 9l3 3-3 3"/></svg>';
  if (action === "playback.rate") return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 17a8 8 0 1 1 16 0M12 13l4-4"/></svg>';
  if (action === "playlist.updated") return '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M8 7h11M8 12h11M8 17h11"/><circle cx="4" cy="7" r="1"/><circle cx="4" cy="12" r="1"/><circle cx="4" cy="17" r="1"/></svg>';
  if (action === "wheel.started" || action === "wheel.completed") return '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8"/><path d="M12 4v16M4 12h16M6.3 6.3l11.4 11.4M6.3 17.7 17.7 6.3"/></svg>';
  return '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3"/></svg>';
}

function renderActivity(entry) {
  const activity = entry.value;
  const row = document.createElement("div");
  row.className = "activity-msg-row";
  const icon = document.createElement("span");
  icon.className = "activity-msg-icon";
  icon.innerHTML = activityIcon(activity.action);
  const text = document.createElement("span");
  text.className = "activity-msg-text";
  text.textContent = activityDescription(activity);
  const time = document.createElement("time");
  time.className = "activity-msg-time";
  time.textContent = new Date(entry.sentAtUnixMs).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  row.append(icon, text, time);
  return row;
}

function renderChatMessage(entry) {
  const message = entry.value;
  const row = document.createElement("div");
  row.className = "chat-msg-row";
  const avatar = document.createElement("div");
  avatar.className = "chat-msg-avatar";
  avatar.textContent = (message.participantName || "?").slice(0, 1).toUpperCase();
  const body = document.createElement("div");
  body.className = "chat-msg-body";
  const head = document.createElement("div");
  head.className = "chat-msg-header";
  const author = document.createElement("span");
  author.className = "chat-msg-author";
  author.textContent = message.participantName;
  const time = document.createElement("time");
  time.className = "chat-msg-time";
  time.textContent = new Date(entry.sentAtUnixMs).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const text = document.createElement("p");
  text.className = "chat-msg-text";
  text.textContent = message.message;
  head.append(author, time);
  body.append(head, text);
  row.append(avatar, body);
  return row;
}

function renderChat() {
  const chatStream = $("chat");
  if (!chatStream) return;
  const visible = timeline.filter((entry) => chatFilter === "all" || entry.kind === chatFilter);
  if (!visible.length) {
    const label = chatFilter === "activity" ? "No room activity yet." : chatFilter === "chat" ? "No chat messages yet." : "Chat messages and room activity will appear here.";
    chatStream.innerHTML = `<div class="chat-empty"><span>✦</span><p>${label}</p></div>`;
    return;
  }
  chatStream.replaceChildren(...visible.map((entry) => entry.kind === "activity" ? renderActivity(entry) : renderChatMessage(entry)));
  chatStream.scrollTop = chatStream.scrollHeight;
}

// Menus and settings dialog
document.addEventListener("click", (event) => {
  if (!event.target.closest(".popover, .popover-menu") && !event.target.closest("#playlist-more") && !event.target.closest("#room-chip") && !event.target.closest("#leave-room") && !event.target.closest("#connection-btn")) {
    closePopovers();
  }
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    closePopovers();
  }
});
$("room-chip").onclick = (event) => { event.stopPropagation(); togglePopover($("room-menu"), $("room-chip")); };
$("copy-room-name").onclick = () => { closePopovers(); copyText(snapshot.room.id, "Room name copied"); };
$("open-settings").onclick = () => { renderConnectionInfo(); $("settings-dialog").showModal(); };
$("settings-dialog").addEventListener("click", (event) => { if (event.target === $("settings-dialog")) $("settings-dialog").close(); });
document.querySelectorAll("[data-settings]").forEach((button) => {
  button.onclick = () => {
    document.querySelectorAll("[data-settings]").forEach((node) => node.classList.toggle("active", node === button));
    document.querySelectorAll("[data-page]").forEach((page) => page.classList.toggle("hidden", page.dataset.page !== button.dataset.settings));
    if (button.dataset.settings === "legal") loadLegalInfo();
  };
});

async function loadLegalInfo() {
  if (legalInfoLoaded) return;
  try {
    const info = await invoke("LegalInfo");
    $("legal-version").textContent = info.version || "";
    $("legal-project-license").textContent = info.projectLicense || "License text unavailable.";
    $("legal-third-party").textContent = info.thirdPartyNotices || "Third-party notices unavailable.";
    legalSourceURL = info.sourceUrl || "";
    legalInfoLoaded = true;
  } catch (error) {
    $("legal-project-license").textContent = error?.message || String(error);
    $("legal-third-party").textContent = "Unable to load third-party notices.";
  }
}

$("legal-source").onclick = async () => {
  await loadLegalInfo();
  if (legalSourceURL) wails?.Browser?.OpenURL(legalSourceURL).catch(showError);
};

function cycleTheme() {
  // data-theme is already resolved, so the toggle never needs the system query.
  const order = ["dark", "light", "sage"];
  const next = order[(order.indexOf(document.documentElement.dataset.theme) + 1) % order.length];
  savePreferences({ theme: next });
}
$("welcome-theme").onclick = cycleTheme;
$("theme-select").onchange = (event) => savePreferences({ theme: event.target.value });
$("compact-mode").onchange = (event) => savePreferences({ compact: event.target.checked });
$("reduce-motion").onchange = (event) => savePreferences({ reduceMotion: event.target.checked });
$("skip-seconds").onchange = (event) => savePreferences({ skipSeconds: Math.max(1, Math.min(120, Number(event.target.value) || 10)) });
function stepSkipInterval(delta) {
  const value = clamp(Number($("skip-seconds").value || preferences.skipSeconds) + delta, 1, 120);
  $("skip-seconds").value = String(value);
  savePreferences({ skipSeconds: value });
}
$("skip-decrease").onclick = () => stepSkipInterval(-1);
$("skip-increase").onclick = () => stepSkipInterval(1);
$("pause-on-leave").onchange = (event) => savePreferences({ pauseOnLeave: event.target.checked });
$("sponsorblock-enabled").onchange = (event) => {
  savePreferences({ sponsorBlock: event.target.checked });
  if (snapshot) invoke("SetSponsorBlockEnabled", event.target.checked).catch(showError);
};
$("wheel-sound-enabled").onchange = (event) => savePreferences({ wheelSound: event.target.checked });
const systemThemeQuery = matchMedia("(prefers-color-scheme: dark)");
const systemThemeChanged = () => {
  if (preferences.theme === "system") applyPreferences();
};
if (systemThemeQuery.addEventListener) systemThemeQuery.addEventListener("change", systemThemeChanged);
else systemThemeQuery.addListener?.(systemThemeChanged);
window.addEventListener("focus", systemThemeChanged);
document.addEventListener("visibilitychange", () => { if (!document.hidden) systemThemeChanged(); });

function showContextMenu(event, items) {
  const menu = $("context-menu");
  menu.replaceChildren(...items);
  closePopovers();
  menu.classList.remove("hidden");
  const width = menu.offsetWidth;
  const height = menu.offsetHeight;
  menu.style.left = `${Math.max(8, Math.min(innerWidth - width - 8, event.clientX))}px`;
  menu.style.top = `${Math.max(8, Math.min(innerHeight - height - 8, event.clientY))}px`;
}

async function pasteIntoControl(control) {
  try {
    const text = wails?.Clipboard ? await wails.Clipboard.Text() : await navigator.clipboard.readText();
    control.setRangeText(text, control.selectionStart ?? control.value.length, control.selectionEnd ?? control.value.length, "end");
    control.dispatchEvent(new Event("input", { bubbles: true }));
  } catch (error) { showError(error); }
}

document.addEventListener("contextmenu", (event) => {
  event.preventDefault();
  const control = event.target.closest("input, textarea");
  if (control) {
    const selected = (control.selectionEnd || 0) > (control.selectionStart || 0);
    showContextMenu(event, [
      menuButton("Cut", () => { control.focus(); document.execCommand("cut"); closePopovers(); }, { disabled: !selected || control.readOnly }),
      menuButton("Copy", () => { control.focus(); document.execCommand("copy"); closePopovers(); }, { disabled: !selected }),
      menuButton("Paste", () => { closePopovers(); pasteIntoControl(control); }, { disabled: control.readOnly }),
      menuButton("Select All", () => { control.focus(); control.select(); closePopovers(); })
    ]);
    return;
  }

  const queueRow = event.target.closest(".queue-item");
  if (queueRow && snapshot) {
    const index = Number(queueRow.dataset.index);
    const item = snapshot.playlist.items[index];
    const controls = [menuButton("Play now", () => { closePopovers(); playPlaylist(index); }, { disabled: !canControl() || !isPlaylistItemPlayable(item) || index === snapshot.playlist.selected })];
    controls.push(
      menuButton("Move up", () => { closePopovers(); reorderPlaylist(index, index - 1); }, { disabled: !canControl() || index === 0 }),
      menuButton("Move down", () => { closePopovers(); reorderPlaylist(index, index + 1); }, { disabled: !canControl() || index === snapshot.playlist.items.length - 1 }),
      menuButton("Remove from queue", () => { closePopovers(); removePlaylist(index); }, { disabled: !canControl(), danger: true })
    );
    showContextMenu(event, controls);
    return;
  }

  const selectedText = String(getSelection()?.toString() || "").trim();
  const items = [];
  if (selectedText) items.push(menuButton("Copy", () => { copyText(selectedText, "Copied"); closePopovers(); }));
  if (snapshot) items.push(menuButton("Copy invite", () => { $("copy-invite").click(); closePopovers(); }));
  items.push(menuButton("Preferences…", () => { closePopovers(); renderConnectionInfo(); $("settings-dialog").showModal(); }));
  showContextMenu(event, items);
});

// External file drops from Wails and browser drop
document.addEventListener("dragenter", (event) => {
  if (event.dataTransfer?.types?.includes("Files")) {
    dragDepth++;
    document.body.classList.add("drag-active");
  }
});
document.addEventListener("dragleave", () => {
  dragDepth = Math.max(0, dragDepth - 1);
  if (!dragDepth) document.body.classList.remove("drag-active");
});
document.addEventListener("dragover", (event) => event.preventDefault());
document.addEventListener("drop", async (event) => {
  event.preventDefault();
  dragDepth = 0;
  document.body.classList.remove("drag-active");
  if (draggedPlaylistIndex >= 0) return;
  const paths = [...(event.dataTransfer?.files || [])].map((file) => file.path).filter(Boolean);
  if (paths.length) {
    await addMediaPaths(paths, Boolean(event.target.closest("#now-playing-drop")));
    return;
  }
  const value = event.dataTransfer?.getData("text/uri-list") || event.dataTransfer?.getData("text/plain") || "";
  if (/^https?:\/\//i.test(value.trim()) && snapshot && canControl()) {
    try {
      const source = value.trim(), info = await prepareYouTubeSource(source);
      await updatePlaylist([...playlistInputs(), { id: "", label: info?.title || "", source, url: "", media: null }]);
    } catch (error) { showError(error); }
  }
});

function receiveFileDrop(paths, target) {
  dragDepth = 0;
  document.body.classList.remove("drag-active");
  addMediaPaths(paths || [], target?.id === "now-playing-drop");
}

function receive(event) {
  if (event.connection) {
    setConnection(event.connection);
    if (event.connection.state === "connected") refreshStreamingSupport();
    if (event.connection.state === "disconnected") {
      streamingAvailable = false;
      streamState = { state: "idle", offerId: "", route: "" };
    }
  }
  if (event.snapshot) {
    snapshot = normalizeSnapshot(event.snapshot);
    render();
    refreshAvailability();
    if (!streamingAvailable) refreshStreamingSupport();
    if (event.snapshot.playlistWheel) showWheel(event.snapshot.playlistWheel, event.serverNowUnixMs);
  }
  if (event.stream) {
    streamState = { state: event.stream.state || "idle", offerId: event.stream.offerId || "", route: event.stream.route || "" };
    if (snapshot) render();
  }
  if (event.wheel) showWheel(event.wheel, event.serverNowUnixMs);
  if (event.chat) addChat(event.chat);
  if (event.activity) addActivity(event.activity);
  if (event.error) {
    if (["stream_connect", "stream_player", "stream_open", "stream_identity", "stream_unavailable"].includes(event.error.code)) {
      streamState = { state: "idle", offerId: "", route: "" };
      if (snapshot) render();
    }
    showError(event.error.message);
  }
}

// Global keyboard shortcuts
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    closePopovers();
    if ($("settings-dialog").open) $("settings-dialog").close();
    if ($("wheel-dialog").open) $("wheel-dialog").close();
  }
  if (!snapshot || event.target.matches("input, select, textarea") || event.target.isContentEditable) return;
  if (event.code === "Space" && canControl()) { event.preventDefault(); $("pause").click(); }
  if (event.key === "ArrowLeft" && canControl()) { event.preventDefault(); $("back-ten").click(); }
  if (event.key === "ArrowRight" && canControl()) { event.preventDefault(); $("forward-ten").click(); }
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "o" && canControl()) { event.preventDefault(); chooseFiles(); }
  if ((event.ctrlKey || event.metaKey) && event.key === ",") { event.preventDefault(); $("settings-dialog").showModal(); }
});

if (wails?.Events) {
  wails.Events.On("faro:event", (event) => receive(event.data));
  wails.Events.On("faro:file-drop", (event) => receiveFileDrop(event.data?.paths, event.data?.target));
}

setInterval(() => {
  if (!snapshot || snapshot.playback.paused || connectionState !== "connected" || isScrubbing) return;
  const projected = projectedPlaybackPosition();
  const duration = playbackDuration();
  if (duration) $("position").value = String(projected);
  updateScrubberProgress(projected, duration);
  $("current-time").textContent = formatTime(projected);
}, 250);

await configurePlayerOptions();
hydrateConnectForms();
enhanceAllSelects();
applyPreferences();
renderMediaDirectories();
if (hasBackend) invoke("Version").then((value) => {
  appVersion = value;
  $("version").textContent = value;
  renderConnectionInfo();
}).catch(() => {});
