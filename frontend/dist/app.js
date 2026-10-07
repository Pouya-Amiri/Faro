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
let activeWheel = null;
let dismissedWheelID = null;
let wheelAnimation = 0;
let wheelServerOffsetMs = 0;
let wheelRotation = 0;
let isScrubbing = false;
let seekCommandPending = false;
let seekReleaseTimer = 0;
let appVersion = "";
let timelineSegments = [];
let timelineSegmentKey = "";
let timelineSegmentsRetryKey = "";
let wheelAudioContext = null;
let wheelSpinOutput = null;
let lastWheelTick = -1;
let lastWheelSoundTime = -Infinity;
let wheelAudioIdleTimer = 0;
let addingPlaylistURL = false;
let streamingAvailable = false;
let streamState = { state: "idle", offerId: "", route: "" };
// How closely this participant's own player follows the room (from the backend).
let syncStatus = { state: "idle", driftSeconds: 0 };
let legalInfoLoaded = false;
let legalSourceURL = "";
let updateStatus = null;
let updateNotifiedVersion = "";
const youtubeInfoCache = new Map();
// The wheel is drawn on a canvas, so it cannot inherit the CSS tokens; these
// palettes mirror them instead. Each keeps neighbouring segments in
// different hues, and each theme sets its own label ink.
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
    // Every theme labels all of its segments in one light ink over a drop
    // shadow; each palette is dark enough to carry it.
    label: { color: "#ffffff", shadow: "rgba(0, 0, 0, 0.6)" },
    separator: "rgba(255, 255, 255, 0.2)",
    // Lit from the top left: the bezel runs from its first colour to its second.
    bezel: ["#3b3b40", "#1c1c1f"],
    bezelLine: "rgba(255, 255, 255, 0.14)",
    // Pegs, pointer and the winner outline take the theme accent.
    accent: "#0a84ff",
    pointerRim: "#f5f5f7",
    // Washes the segments that did not win.
    dim: "rgba(12, 12, 14, 0.58)",
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
    label: { color: "#FCFBF9", shadow: "rgba(0, 0, 0, 0.6)" },
    separator: "rgba(255, 255, 255, 0.75)",
    // Lit from the top left: the bezel runs from its first colour to its second.
    bezel: ["#FFFFFF", "#DAD6D0"],
    bezelLine: "rgba(37, 39, 42, 0.16)",
    // Pegs, pointer and the winner outline take the theme accent.
    accent: "#4D699B",
    pointerRim: "#FFFFFF",
    // Washes the segments that did not win.
    dim: "rgba(247, 245, 242, 0.66)",
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
    label: { color: "#F2F6EC", shadow: "rgba(0, 0, 0, 0.6)" },
    separator: "rgba(248, 251, 244, 0.7)",
    // Lit from the top left: the bezel runs from its first colour to its second.
    bezel: ["#F8FBF4", "#C6CFBC"],
    bezelLine: "rgba(34, 41, 34, 0.18)",
    // Pegs, pointer and the winner outline take the theme accent.
    accent: "#BA5220",
    pointerRim: "#F8FBF4",
    // Washes the segments that did not win.
    dim: "rgba(226, 231, 220, 0.66)",
    empty: "#E2E7DC"
  },
  midnight: {
    segments: [
      "#1D4ED8", // Deep blue
      "#D97706", // Lighthouse amber
      "#0E7490", // Ocean cyan
      "#DC2626", // Signal red
      "#4338CA", // Deep indigo
      "#059669", // Emerald buoy
      "#C08304", // Maritime gold
      "#7C3AED", // Deep violet
      "#0284C7", // Sky blue
      "#EA580C", // Port orange
      "#0F766E", // Deep teal
      "#BE185D"  // Beacon rose
    ],
    label: { color: "#F0F4F8", shadow: "rgba(0, 0, 0, 0.75)" },
    separator: "rgba(240, 244, 248, 0.18)",
    // Lit from the top left: the bezel runs from its first colour to its second.
    bezel: ["#2A3854", "#101723"],
    bezelLine: "rgba(180, 200, 225, 0.16)",
    // Pegs, pointer and the winner outline take the theme accent.
    accent: "#E5A93C",
    pointerRim: "#F0F4F8",
    // Washes the segments that did not win.
    dim: "rgba(8, 11, 17, 0.6)",
    empty: "#0C1017"
  },
  pine: {
    segments: [
      "#AA8A4E", // Lighthouse brass
      "#2E8B57", // Deep sea green
      "#9C7A3E", // Polished brass
      "#C96767", // Signal red
      "#257A68", // Maritime spruce
      "#7B61FF", // Atlantic dusk
      "#5E9D74", // Sea glass green
      "#A98648", // Burnished brass
      "#2B6CB0", // Deep ocean blue
      "#A3704C", // Teak wood
      "#0EA372", // Emerald beacon
      "#9F5874"  // Coastal heather
    ],
    label: { color: "#E8ECE8", shadow: "rgba(0, 0, 0, 0.75)" },
    separator: "rgba(232, 236, 232, 0.16)",
    // Lit from the top left: the bezel runs from its first colour to its second.
    bezel: ["#323D35", "#141A16"],
    bezelLine: "rgba(232, 236, 232, 0.12)",
    // Pegs, pointer and the winner outline take the theme accent.
    accent: "#C6A15B",
    pointerRim: "#E8ECE8",
    // Washes the segments that did not win.
    dim: "rgba(10, 14, 11, 0.6)",
    empty: "#111713"
  }
};
function wheelTheme() {
  return wheelThemes[document.documentElement.dataset.theme] || wheelThemes.dark;
}
const sunIcon = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.93 4.93l1.42 1.42M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.42-1.41M17.66 6.34l1.41-1.41"/></svg>';
const moonIcon = '<svg class="icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8Z"/></svg>';
const playIcon = '<svg class="play-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 6 9 6-9 6V6Z" fill="currentColor" stroke="none"/></svg>';
const pauseIcon = '<svg class="play-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6v12M15 6v12"/></svg>';

const defaultPreferences = {
  theme: "system", compact: false, reduceMotion: false, skipSeconds: 10, wheelSound: true,
  pauseOnLeave: false, sponsorBlock: true, autoOffer: true, chatOverlay: true, streamCacheLimit: 0, checkUpdates: true, youtubeQualities: {}, name: "", player: "mpv", executable: "",
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

// Backend errors are Go error strings, which start in lower case.
function errorText(error) {
  const text = String(error?.message || error || "Something went wrong").trim();
  return text ? text[0].toUpperCase() + text.slice(1) : "Something went wrong";
}
function showError(error) { showToast(errorText(error), "error"); }

// A busy button ignores repeated presses immediately, but only shows its
// loading state if the task is still running after a moment. Disabling it for
// instant actions made it blink.
async function withButtonLoading(button, task, loadingLabel = "") {
  if (!button || button.dataset.busy === "true") return;
  const label = button.querySelector("span");
  const previousLabel = label?.textContent || "";
  button.dataset.busy = "true";
  button.setAttribute("aria-busy", "true");
  const showLoading = setTimeout(() => {
    button.classList.add("is-loading");
    if (label && loadingLabel) label.textContent = loadingLabel;
  }, 150);
  try { return await task(); }
  finally {
    clearTimeout(showLoading);
    if (label && loadingLabel) label.textContent = previousLabel;
    button.dataset.busy = "false";
    button.classList.remove("is-loading");
    button.removeAttribute("aria-busy");
    if (snapshot) render();
  }
}

function setDisabled(id, disabled) {
  const element = $(id);
  if (element && element.disabled !== Boolean(disabled)) element.disabled = Boolean(disabled);
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

// A copy may not know its duration yet (a stream still opening), while a
// friend's copy or the queue entry for the same file does.
function mediaDuration(media) {
  if (!media) return 0;
  const candidates = [media, ...(snapshot?.participants || []).map((person) => person.media), ...(snapshot?.playlist?.items || []).map((item) => item.media)];
  for (const candidate of candidates) {
    if (candidate !== media && candidate?.fingerprint !== media.fingerprint) continue;
    const duration = Number(candidate?.durationSeconds || 0);
    if (Number.isFinite(duration) && duration > 0) return duration;
  }
  return 0;
}

function playbackDuration() {
  return mediaDuration(referenceMedia());
}

function streamCacheLimit() {
  const value = Number(preferences.streamCacheLimit);
  return [0, -1, 2147483648, 5368709120, 10737418240].includes(value) ? value : 0;
}

// Cached parts of a streamed file, drawn under the played part of the track.
function renderStreamCache() {
  const layer = $("timeline-cache");
  if (!layer) return;
  const ranges = streamState.state === "active" && Array.isArray(streamState.cachedRanges) ? streamState.cachedRanges : [];
  if (!ranges.length) {
    layer.style.background = "";
    return;
  }
  const stops = [];
  for (const [start, end] of ranges) {
    const from = (clamp(start, 0, 1) * 100).toFixed(2), to = (clamp(end, 0, 1) * 100).toFixed(2);
    stops.push(`transparent ${from}%`, `var(--cache-fill) ${from}%`, `var(--cache-fill) ${to}%`, `transparent ${to}%`);
  }
  layer.style.background = `linear-gradient(to right, ${stops.join(", ")})`;
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
  refreshRateRange();
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
  // Quotes group words, wherever they appear: --title="My Movie" becomes
  // --title=My Movie, as a shell would pass it.
  return matches.map((item) => item.replace(/"([^"]*)"|'([^']*)'/g, (_, double, single) => double ?? single));
}

function basename(path) { return String(path).split(/[\\/]/).filter(Boolean).pop() || path; }
function isYouTubeURL(source) { return /^https?:\/\/(?:(?:www\.|m\.|music\.)?youtube\.com|youtu\.be|(?:www\.)?youtube-nocookie\.com)(?:[\/?#:]|$)/i.test(String(source || "").trim()); }
function self() { return snapshot?.participants?.find((person) => person.id === snapshot.selfId); }
function canControl() { const me = self(); return snapshot?.room?.mode === "collaborative" || me?.role === "owner" || me?.role === "moderator"; }
function playlistInputs() { return (snapshot?.playlist?.items || []).map(({ id, label, url, media }) => ({ id, label, url: url || "", media: media || null, source: "" })); }

function normalizeSnapshot(value) {
  if (!value) return null;
  value.participants = Array.isArray(value.participants)
    ? value.participants.filter((person) => person && typeof person === "object").map((person) => ({
      ...person,
      id: typeof person.id === "string" ? person.id : "",
      name: typeof person.name === "string" && person.name ? person.name : "Unknown participant",
      role: typeof person.role === "string" ? person.role : "member",
      availableMedia: Array.isArray(person.availableMedia) ? person.availableMedia : []
    }))
    : [];
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
  const isDark = resolved === "dark" || resolved === "midnight" || resolved === "pine";
  // data-theme carries the resolved theme, never the preference, so styles.css
  // defines each palette once. Every palette is keyed on this attribute, so it
  // is written only when it actually changes: a redundant write restyles the
  // whole document.
  const root = document.documentElement;
  if (root.dataset.theme !== resolved) root.dataset.theme = resolved;
  const colorScheme = isDark ? "dark" : "light";
  if (root.style.colorScheme !== colorScheme) root.style.colorScheme = colorScheme;
  document.body.classList.toggle("compact", Boolean(preferences.compact));
  document.body.classList.toggle("reduce-motion", Boolean(preferences.reduceMotion));
  const themeButton = $("welcome-theme");
  const themeIcon = themeButton?.querySelector(".theme-btn-icon") || themeButton;
  if (themeIcon && themeIcon.dataset.icon !== colorScheme) {
    themeIcon.dataset.icon = colorScheme;
    themeIcon.innerHTML = isDark ? sunIcon : moonIcon;
  }
  rememberWindowBackground();
  if ($("theme-select")) {
    $("theme-select").value = preferences.theme;
    $("theme-select")._syncCustomSelect?.();
  }
  if ($("compact-mode")) $("compact-mode").checked = Boolean(preferences.compact);
  if ($("reduce-motion")) $("reduce-motion").checked = Boolean(preferences.reduceMotion);
  if ($("skip-seconds")) $("skip-seconds").value = preferences.skipSeconds;
  document.querySelectorAll(".skip-num").forEach((node) => {
    const label = String(preferences.skipSeconds);
    if (node.textContent !== label) node.textContent = label;
    node.setAttribute("font-size", label.length > 2 ? "6" : "7.5");
  });
  for (const [id, direction] of [["back-ten", "backward"], ["forward-ten", "forward"]]) {
    const button = $(id);
    if (!button) continue;
    button.title = `Skip ${direction} ${preferences.skipSeconds} seconds`;
    button.setAttribute("aria-label", button.title);
  }
  if ($("pause-on-leave")) $("pause-on-leave").checked = Boolean(preferences.pauseOnLeave);
  if ($("sponsorblock-enabled")) $("sponsorblock-enabled").checked = preferences.sponsorBlock !== false;
  if ($("auto-offer-enabled")) $("auto-offer-enabled").checked = preferences.autoOffer !== false;
  if ($("chat-overlay-enabled")) $("chat-overlay-enabled").checked = preferences.chatOverlay !== false;
  if ($("check-updates")) $("check-updates").checked = preferences.checkUpdates !== false;
  if ($("stream-cache-limit")) {
    $("stream-cache-limit").value = String(streamCacheLimit());
    $("stream-cache-limit")._syncCustomSelect?.();
  }
  if ($("wheel-sound-enabled")) $("wheel-sound-enabled").checked = preferences.wheelSound !== false;
}

// The native window paints this colour before the page exists, so the next
// launch starts in the right theme instead of flashing the default dark one.
// On Linux it is also the GTK frame colour under the rounded corners, which
// are the sidebar and toolbar surfaces, so --panel-alt is the colour the
// anti-aliased corner edge must blend into.
let rememberedWindowBackground = "";
function rememberWindowBackground() {
  if (!hasBackend) return;
  let colour = getComputedStyle(document.documentElement).getPropertyValue("--panel-alt").trim().toLowerCase();
  if (!/^#[0-9a-f]{6}$/.test(colour)) {
    const match = getComputedStyle(document.body).backgroundColor.match(/^rgba?\((\d+),\s*(\d+),\s*(\d+)/);
    if (!match) return;
    colour = `#${match.slice(1, 4).map((channel) => Number(channel).toString(16).padStart(2, "0")).join("")}`;
  }
  if (colour === rememberedWindowBackground) return;
  rememberedWindowBackground = colour;
  invoke("SetWindowBackground", colour).catch(() => {});
}

// Window controls mirror the desktop's title bar conventions.
let windowChrome = { buttonsSide: "right", buttons: ["minimize", "maximize", "close"], doubleClick: "toggle-maximize", style: "windows" };
// Space each control style needs: [per button, gap between buttons, outer padding].
const windowControlMetrics = { windows: [46, 0, 0], gnome: [24, 12, 24], kde: [22, 6, 20] };
const maximiseIcon = '<svg viewBox="0 0 12 12" width="10" height="10" aria-hidden="true"><rect x="2" y="2" width="8" height="8" rx="1.5" fill="none" stroke="currentColor" stroke-width="1.3"/></svg>';
const restoreIcon = '<svg viewBox="0 0 12 12" width="10" height="10" aria-hidden="true"><rect x="2" y="4" width="6" height="6" rx="1.2" fill="none" stroke="currentColor" stroke-width="1.3"/><path d="M4.5 4V3.2c0-.66.54-1.2 1.2-1.2h3.1c.66 0 1.2.54 1.2 1.2v3.1c0 .66-.54 1.2-1.2 1.2H8" fill="none" stroke="currentColor" stroke-width="1.3"/></svg>';

async function loadWindowChrome() {
  if (!hasBackend) return;
  try { windowChrome = { ...windowChrome, ...(await invoke("WindowChrome")) }; } catch (_) {}
}

function applyWindowChrome() {
  const controls = document.querySelector(".window-controls");
  const byName = { minimize: $("window-minimise"), maximize: $("window-maximise"), close: $("window-close") };
  const names = (Array.isArray(windowChrome.buttons) ? windowChrome.buttons : []).filter((name) => byName[name]);
  for (const [name, button] of Object.entries(byName)) button.classList.toggle("hidden", !names.includes(name));
  for (const name of names) controls.append(byName[name]);
  const style = windowControlMetrics[windowChrome.style] ? windowChrome.style : "windows";
  const [button, gap, padding] = windowControlMetrics[style];
  document.body.dataset.controlsSide = windowChrome.buttonsSide === "left" ? "left" : "right";
  document.body.dataset.controlsStyle = style;
  const width = names.length ? names.length * button + (names.length - 1) * gap + padding : 0;
  document.documentElement.style.setProperty("--window-controls-width", `${width}px`);
}

function setWindowState(state = {}) {
  const maximised = Boolean(state.maximised || state.fullscreen);
  const button = $("window-maximise");
  if (button.dataset.maximised === String(maximised)) return;
  button.dataset.maximised = String(maximised);
  button.innerHTML = maximised ? restoreIcon : maximiseIcon;
  button.title = maximised ? "Restore" : "Maximise";
  button.setAttribute("aria-label", maximised ? "Restore window" : "Maximise window");
}

function titlebarDoubleClick(event) {
  if (event.target.closest("button, input, select, textarea, a")) return;
  if (windowChrome.doubleClick === "minimize") wails?.Window?.Minimise();
  else if (windowChrome.doubleClick !== "none") wails?.Window?.ToggleMaximise();
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

  for (const id of ["pause", "position", "back-ten", "forward-ten", "add-file", "empty-add-file", "add-stream-btn", "add-playlist", "shuffle-playlist", "shuffle-all", "load-playlist-file", "save-playlist-file", "clear-playlist", "spin-wheel", "wheel-spin-again", "previous-media", "next-media"]) setDisabled(id, !allowed);
  setDisabled("add-playlist", !allowed || addingPlaylistURL);

  const media = me?.media || snapshot.participants.find((person) => person.media)?.media;
  const title = media?.title || "No media loaded";
  $("media-title").textContent = title;
  $("media-title").title = title;
  renderMediaKind(media);
  renderStreamCache();
  const mismatches = media ? snapshot.participants.filter((person) => person.media && person.media.fingerprint !== media.fingerprint) : [];
  $("media-warning").classList.toggle("hidden", mismatches.length === 0);
  $("media-warning").textContent = mismatches.length ? `${mismatches.length} participant${mismatches.length === 1 ? " has" : "s have"} different media loaded. Sync paused for mismatched copies.` : "";

  renderSyncBadge(media, mismatches.length);

  const playback = snapshot.playback;
  const duration = mediaDuration(media);
  const hasDuration = duration > 0;
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
  const pauseState = playback.paused ? "paused" : "playing";
  if (pauseBtn.dataset.state !== pauseState) {
    pauseBtn.dataset.state = pauseState;
    pauseBtn.innerHTML = playback.paused ? playIcon : pauseIcon;
    pauseBtn.setAttribute("aria-label", playback.paused ? "Play" : "Pause");
  }

  updateRateControl(playback.rate || 1);
  renderParticipants(media);
  renderPlaylist();
  renderServerStatus();
  renderConnectionInfo();
}

const playbackRates = [0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 4];
const rateStep = 0.1;
// The backend reports the speeds the room and this participant's player
// support; until it answers, the protocol range is assumed.
let rateRange = { min: 0.25, max: 4, supported: true };
function rateLabel(value) { return `${Number(value.toFixed(2))}×`; }

// The speed menu lists the standard rates. A rate set elsewhere (for example
// in a participant's own player) gets a temporary entry so it still shows.
function updateRateControl(rate) {
  const value = Number(rate) || 1;
  const control = $("playback-rate");
  const key = String(Number(value.toFixed(2)));
  control.querySelectorAll("option[data-custom]").forEach((option) => { if (option.value !== key) option.remove(); });
  if (![...control.options].some((option) => option.value === key)) {
    const option = new Option(rateLabel(value), key);
    option.dataset.custom = "true";
    const next = [...control.options].find((candidate) => Number(candidate.value) > value);
    control.insertBefore(option, next || null);
  }
  if (control.value !== key) control.value = key;
  control.dataset.value = key;
  for (const option of control.options) option.disabled = Number(option.value) < rateRange.min || Number(option.value) > rateRange.max;
  const unavailable = !canControl() || !rateRange.supported;
  control.disabled = unavailable;
  $("rate-decrease").disabled = unavailable || value <= rateRange.min + 1e-9;
  $("rate-increase").disabled = unavailable || value >= rateRange.max - 1e-9;
  const title = rateRange.supported ? "Playback speed" : "This media player cannot change playback speed";
  control.title = title;
  control.parentElement.title = title;
  control._syncCustomSelect?.();
}

// Fine steps between the presets, rounded so repeated steps never drift.
function nudgePlaybackRate(direction) {
  if (!canControl() || !rateRange.supported) return;
  const current = Number($("playback-rate").dataset.value || 1);
  const target = Math.round(clamp(current + direction * rateStep, rateRange.min, rateRange.max) * 100) / 100;
  if (target !== current) invoke("SetRate", target).catch(showError);
}

async function refreshRateRange() {
  if (!hasBackend || !snapshot) return;
  try {
    const next = await invoke("PlaybackRateRange");
    if (next && typeof next.supported === "boolean") rateRange = next;
  } catch (_) {}
  if (snapshot) updateRateControl(snapshot.playback.rate || 1);
}

function stepPlaybackRate(direction) {
  if (!canControl() || !rateRange.supported) return;
  const current = Number($("playback-rate").dataset.value || 1);
  const index = playbackRates.reduce((best, candidate, candidateIndex) => Math.abs(candidate - current) < Math.abs(playbackRates[best] - current) ? candidateIndex : best, 0);
  let next = clamp(index + direction, 0, playbackRates.length - 1);
  // From an in-between speed, the first step lands on the neighbouring preset.
  if (direction > 0 && playbackRates[index] > current) next = index;
  if (direction < 0 && playbackRates[index] < current) next = index;
  const target = clamp(playbackRates[next], rateRange.min, rateRange.max);
  if (target !== current) invoke("SetRate", target).catch(showError);
}

function wheelTargetRotation(wheel) {
  const count = Math.max(1, wheel.items?.length || 0);
  const arc = Math.PI * 2 / count;
  const seed = Number(wheel.visualSeed || 0);
  const jitter = (((seed % 1000) / 999) - 0.5) * arc * 0.34;
  return Number(wheel.turns || 8) * Math.PI * 2 - (Number(wheel.winner || 0) + 0.5) * arc - jitter;
}

// The wheel is composited from cached layers, so an animation frame only
// copies bitmaps: a fixed base (drop shadow and bezel), the rotating face
// (segments, labels and pegs), a fixed overlay (shading and axle hole) and the
// pointer. Redrawing every segment and shadowed label per frame was expensive
// with CPU rendering. Geometry is expressed against a 1000px reference wheel
// and scaled to the real size.
const wheelFaceCache = { key: "", canvas: null };
const wheelFrameCache = { key: "", base: null, overlay: null, pointer: null };
const wheelGeometry = { radius: 470, bezel: 20, peg: 6, hole: 18, pivot: 36, head: 22, tip: 84 };
const wheelFont = '-apple-system, BlinkMacSystemFont, system-ui, "Segoe UI", Roboto, sans-serif';

function wheelCanvasSize(canvas) {
  const cssSize = canvas.clientWidth || 350;
  const size = Math.max(200, Math.min(1400, Math.round(cssSize * (window.devicePixelRatio || 1))));
  if (canvas.width !== size) {
    canvas.width = size;
    canvas.height = size;
  }
  return size;
}

// A queue position owns its color. When the count is one more than a
// multiple of the palette, the last segment would repeat the first, which is
// its neighbour on the wheel; it takes the palette's middle color instead.
function wheelSegmentColor(index, count, palette) {
  const size = palette.length;
  if (count > size && index === count - 1 && index % size === 0) return palette[Math.floor(size / 2)];
  return palette[index % size];
}

// Moves a #rrggbb colour towards another by the given amount.
function mixWheelColor(hex, target, amount) {
  const from = parseInt(hex.slice(1), 16), to = parseInt(target.slice(1), 16);
  const channel = (shift) => Math.round(((from >> shift) & 255) * (1 - amount) + ((to >> shift) & 255) * amount);
  return `rgb(${channel(16)}, ${channel(8)}, ${channel(0)})`;
}

// Queue labels are often release file names. The wheel shows the part that
// tells them apart ("Show Name S01E02"); the result line keeps the full name.
function wheelLabelText(label) {
  let text = String(label || "").trim();
  if (/\s/.test(text)) return text;
  text = text.replace(/\.[a-z0-9]{2,4}$/i, "").replace(/[._]+/g, " ").trim();
  const episode = text.match(/^(.*?\bS\d{1,2} ?E\d{1,3})\b/i);
  if (episode) return episode[1];
  const tag = text.search(/ (?:2160p|1440p|1080p|720p|576p|480p|4k|uhd|hdr|x26[45]|h ?26[45]|hevc|av1|web(?:-?dl|rip)?|blu-?ray|bdrip|brrip|dvdrip|remux)\b/i);
  return tag > 0 ? text.slice(0, tag) : text;
}

// Labels of one series all start the same; the wheel drops the words every
// label shares when each keeps something of its own ("S01E02").
function wheelLabelTexts(items) {
  const texts = items.map((item, index) => wheelLabelText(item.label) || `Item ${index + 1}`);
  if (texts.length < 2) return texts;
  const words = texts.map((text) => text.split(" "));
  let shared = 0;
  while (words.every((parts) => parts.length > shared + 1 && parts[shared] === words[0][shared])) shared++;
  return shared ? words.map((parts) => parts.slice(shared).join(" ")) : texts;
}

function fitWheelLabel(ctx, text, width) {
  if (ctx.measureText(text).width <= width) return text;
  let low = 0, high = text.length;
  while (low < high) {
    const middle = (low + high + 1) >> 1;
    if (ctx.measureText(`${text.slice(0, middle).trimEnd()}…`).width <= width) low = middle;
    else high = middle - 1;
  }
  return `${text.slice(0, low).trimEnd()}…`;
}

function wheelSector(ctx, radius, start, end) {
  ctx.beginPath(); ctx.moveTo(0, 0); ctx.arc(0, 0, radius, start, end); ctx.closePath();
}

// resting is the rotation the wheel will stop at; labels are laid out to read
// upright there.
function wheelFace(items, theme, size, highlight, resting) {
  const turn = Math.PI * 2, restingAngle = ((resting % turn) + turn) % turn;
  const key = JSON.stringify([document.documentElement.dataset.theme, size, highlight, restingAngle.toFixed(3), items.map((item) => item.label)]);
  if (wheelFaceCache.key === key && wheelFaceCache.canvas) return wheelFaceCache.canvas;
  const face = wheelFaceCache.canvas || document.createElement("canvas");
  face.width = size;
  face.height = size;
  const ctx = face.getContext("2d");
  const scale = size / 1000, center = size / 2, radius = (wheelGeometry.radius - wheelGeometry.bezel) * scale;
  const count = items.length, arc = Math.PI * 2 / count;
  const colors = items.map((_, index) => wheelSegmentColor(index, count, theme.segments));
  const segmentStart = (index) => -Math.PI / 2 + index * arc;
  ctx.clearRect(0, 0, size, size);
  ctx.save();
  ctx.translate(center, center);

  // A queue position owns its color. The spin seed only affects trajectory,
  // so opening or spinning the same wheel never repaints its segments. Each
  // segment deepens towards the centre and brightens towards the rim.
  colors.forEach((color, index) => {
    const shading = ctx.createRadialGradient(0, 0, 0, 0, 0, radius);
    shading.addColorStop(0, mixWheelColor(color, "#000000", 0.24));
    shading.addColorStop(0.62, color);
    shading.addColorStop(1, mixWheelColor(color, "#ffffff", 0.1));
    wheelSector(ctx, radius, segmentStart(index), segmentStart(index + 1));
    ctx.fillStyle = shading;
    ctx.fill();
  });
  if (count > 1) {
    ctx.beginPath();
    for (let index = 0; index < count; index++) {
      ctx.moveTo(0, 0);
      ctx.lineTo(Math.cos(segmentStart(index)) * radius, Math.sin(segmentStart(index)) * radius);
    }
    ctx.strokeStyle = theme.separator;
    ctx.lineWidth = 2.5 * scale;
    ctx.stroke();
  }

  // The winner stays lit and outlined; every other segment is washed out.
  if (highlight >= 0 && highlight < count) {
    ctx.fillStyle = theme.dim;
    for (let index = 0; index < count; index++) {
      if (index === highlight) continue;
      wheelSector(ctx, radius, segmentStart(index), segmentStart(index + 1));
      ctx.fill();
    }
    ctx.save();
    wheelSector(ctx, radius, segmentStart(highlight), segmentStart(highlight + 1));
    ctx.clip();
    ctx.lineJoin = "round";
    ctx.lineWidth = 14 * scale;
    ctx.strokeStyle = theme.accent;
    ctx.stroke();
    ctx.restore();
  }

  if (count <= 24) {
    // One size for every label: as large as the segments allow, shrinking
    // (down to a floor) until the longest fits before any is shortened.
    const outer = radius - 32 * scale, inner = 70 * scale;
    const texts = wheelLabelTexts(items);
    let fontSize = Math.max(20, Math.min(36, radius / scale * 0.6 * arc * 0.55)) * scale;
    const setFont = () => { ctx.font = `700 ${fontSize}px ${wheelFont}`; };
    setFont();
    const longest = Math.max(...texts.map((text) => ctx.measureText(text).width));
    if (longest > outer - inner) {
      fontSize = Math.max(24 * scale, fontSize * (outer - inner) / longest);
      setFont();
    }
    ctx.textBaseline = "middle";
    texts.forEach((label, index) => {
      const text = fitWheelLabel(ctx, label, outer - inner);
      const angle = segmentStart(index) + arc / 2;
      // Labels that will rest on the left half turn over so none reads upside
      // down there; each still starts from the rim.
      const flipped = Math.cos(angle + restingAngle) < -1e-6;
      ctx.save();
      ctx.rotate(flipped ? angle + Math.PI : angle);
      ctx.textAlign = flipped ? "left" : "right";
      ctx.globalAlpha = highlight >= 0 && index !== highlight ? 0.55 : 1;
      ctx.fillStyle = theme.label.color;
      ctx.shadowColor = theme.label.shadow;
      ctx.shadowBlur = 4 * scale;
      ctx.fillText(text, flipped ? -outer : outer, 0);
      ctx.restore();
    });
  }

  // Pegs sit on the bezel between segments; past a few dozen they would
  // merge into a solid ring.
  if (count > 1 && count <= 60) {
    const pegRadius = (wheelGeometry.radius - wheelGeometry.bezel / 2) * scale, peg = wheelGeometry.peg * scale;
    for (let index = 0; index < count; index++) {
      const x = Math.cos(segmentStart(index)) * pegRadius, y = Math.sin(segmentStart(index)) * pegRadius;
      const shine = ctx.createRadialGradient(x - peg * 0.35, y - peg * 0.35, peg * 0.1, x, y, peg);
      shine.addColorStop(0, mixWheelColor(theme.accent, "#ffffff", 0.45));
      shine.addColorStop(1, theme.accent);
      ctx.save();
      ctx.shadowColor = "rgba(0, 0, 0, 0.4)";
      ctx.shadowBlur = 5 * scale;
      ctx.beginPath(); ctx.arc(x, y, peg, 0, Math.PI * 2);
      ctx.fillStyle = shine;
      ctx.fill();
      ctx.restore();
    }
  }
  ctx.restore();
  wheelFaceCache.key = key;
  wheelFaceCache.canvas = face;
  return face;
}

// The layers that do not turn: the bezel with its drop shadow underneath the
// face, the rim shading and axle hole above it, and the pointer sprite.
function wheelFrame(theme, size) {
  const key = `${document.documentElement.dataset.theme}:${size}`;
  if (wheelFrameCache.key === key) return wheelFrameCache;
  const scale = size / 1000, center = size / 2;
  const outer = wheelGeometry.radius * scale, radius = (wheelGeometry.radius - wheelGeometry.bezel) * scale, hole = wheelGeometry.hole * scale;
  const layer = (name, width = size, height = size) => {
    const canvas = wheelFrameCache[name] || document.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    wheelFrameCache[name] = canvas;
    const ctx = canvas.getContext("2d");
    ctx.clearRect(0, 0, width, height);
    return ctx;
  };
  const lit = (ctx, x, y, extent, [light, dark]) => {
    const gradient = ctx.createLinearGradient(x - extent, y - extent, x + extent, y + extent);
    gradient.addColorStop(0, light);
    gradient.addColorStop(1, dark);
    return gradient;
  };

  let ctx = layer("base");
  ctx.save();
  ctx.shadowColor = "rgba(0, 0, 0, 0.3)";
  ctx.shadowBlur = 20 * scale;
  ctx.shadowOffsetY = 6 * scale;
  ctx.beginPath(); ctx.arc(center, center, outer, 0, Math.PI * 2);
  ctx.fillStyle = lit(ctx, center, center, outer, theme.bezel);
  ctx.fill();
  ctx.restore();
  ctx.beginPath(); ctx.arc(center, center, outer - 1.5 * scale, 0, Math.PI * 2);
  ctx.strokeStyle = theme.bezelLine;
  ctx.lineWidth = 3 * scale;
  ctx.stroke();

  ctx = layer("overlay");
  // The face sinks slightly under the bezel, and a soft sheen falls across
  // its upper half.
  const rimShade = ctx.createRadialGradient(center, center, radius - 30 * scale, center, center, radius);
  rimShade.addColorStop(0, "rgba(0, 0, 0, 0)");
  rimShade.addColorStop(1, "rgba(0, 0, 0, 0.26)");
  ctx.beginPath(); ctx.arc(center, center, radius, 0, Math.PI * 2);
  ctx.fillStyle = rimShade;
  ctx.fill();
  const sheen = ctx.createLinearGradient(0, center - radius, 0, center + radius * 0.2);
  sheen.addColorStop(0, "rgba(255, 255, 255, 0.16)");
  sheen.addColorStop(1, "rgba(255, 255, 255, 0)");
  ctx.fillStyle = sheen;
  ctx.fill();
  ctx.beginPath(); ctx.arc(center, center, radius, 0, Math.PI * 2);
  ctx.strokeStyle = "rgba(0, 0, 0, 0.28)";
  ctx.lineWidth = 2 * scale;
  ctx.stroke();
  // A small axle hole: dark inside, shaded from the top, with light
  // catching its lower lip.
  const depth = ctx.createRadialGradient(center, center - hole * 0.4, hole * 0.2, center, center, hole);
  depth.addColorStop(0, "rgba(0, 0, 0, 0.92)");
  depth.addColorStop(1, "rgba(0, 0, 0, 0.6)");
  ctx.beginPath(); ctx.arc(center, center, hole, 0, Math.PI * 2);
  ctx.fillStyle = depth;
  ctx.fill();
  ctx.beginPath(); ctx.arc(center, center, hole + 1.5 * scale, Math.PI * 0.15, Math.PI * 0.85);
  ctx.strokeStyle = "rgba(255, 255, 255, 0.35)";
  ctx.lineWidth = 3 * scale;
  ctx.lineCap = "round";
  ctx.stroke();

  // The pointer is drawn with its pivot at the sprite's origin plus padding,
  // so a frame can rotate it about the pin.
  const pad = 24 * scale, head = wheelGeometry.head * scale, length = (wheelGeometry.tip - wheelGeometry.pivot) * scale;
  ctx = layer("pointer", Math.ceil(2 * (head + pad)), Math.ceil(head + length + 2 * pad));
  const x = head + pad, y = head + pad, spread = Math.acos(head / length);
  ctx.save();
  ctx.shadowColor = "rgba(0, 0, 0, 0.38)";
  ctx.shadowBlur = 12 * scale;
  ctx.shadowOffsetY = 4 * scale;
  ctx.beginPath();
  ctx.moveTo(x, y + length);
  ctx.arc(x, y, head, Math.PI / 2 + spread, Math.PI / 2 - spread + Math.PI * 2);
  ctx.closePath();
  const body = ctx.createLinearGradient(0, y - head, 0, y + length);
  body.addColorStop(0, mixWheelColor(theme.accent, "#ffffff", 0.22));
  body.addColorStop(1, theme.accent);
  ctx.fillStyle = body;
  ctx.fill();
  ctx.restore();
  ctx.lineJoin = "round";
  ctx.lineWidth = 4 * scale;
  ctx.strokeStyle = theme.pointerRim;
  ctx.stroke();
  ctx.beginPath(); ctx.arc(x, y, 8 * scale, 0, Math.PI * 2);
  ctx.fillStyle = theme.pointerRim;
  ctx.fill();
  wheelFrameCache.pivot = [x, y];
  wheelFrameCache.key = key;
  return wheelFrameCache;
}

// pointerAngle tilts the pointer about its pin, in radians; negative is
// towards the left, where the pegs come from. resting is where a spin will
// stop, which decides the label orientation.
function drawWheel(wheel, rotation = 0, highlight = -1, pointerAngle = 0, resting = rotation) {
  const canvas = $("wheel-canvas"), ctx = canvas.getContext("2d");
  const items = wheel?.items?.length ? wheel.items : (snapshot?.playlist?.items || []).map(({ id, label }) => ({ id, label }));
  const theme = wheelTheme();
  const size = wheelCanvasSize(canvas), center = size / 2, scale = size / 1000;
  const frame = wheelFrame(theme, size);
  ctx.clearRect(0, 0, size, size);
  ctx.drawImage(frame.base, 0, 0);
  if (items.length) {
    ctx.save();
    ctx.translate(center, center);
    ctx.rotate(rotation);
    ctx.drawImage(wheelFace(items, theme, size, highlight, resting), -center, -center);
    ctx.restore();
  } else {
    ctx.beginPath(); ctx.arc(center, center, (wheelGeometry.radius - wheelGeometry.bezel) * scale, 0, Math.PI * 2);
    ctx.fillStyle = theme.empty;
    ctx.fill();
  }
  ctx.drawImage(frame.overlay, 0, 0);
  ctx.save();
  ctx.translate(center, wheelGeometry.pivot * scale);
  ctx.rotate(pointerAngle);
  ctx.drawImage(frame.pointer, -frame.pivot[0], -frame.pivot[1]);
  ctx.restore();
}

function wheelCandidate(wheel, rotation) {
  const items = wheel?.items || [];
  if (!items.length) return null;
  const arc = Math.PI * 2 / items.length;
  const normalized = ((-rotation % (Math.PI * 2)) + Math.PI * 2) % (Math.PI * 2);
  return items[Math.floor(normalized / arc) % items.length];
}

// The context is unlocked by the first gesture, but it only runs while the
// wheel makes sound. A running context keeps an audio stream open to the
// sound server, and one left open between spins is the likeliest source of
// stray wheel sounds heard long after a spin with WebKitGTK on GNOME.
function ensureWheelAudio() {
  if (preferences.wheelSound === false) {
    suspendWheelAudio();
    return null;
  }
  const AudioContext = window.AudioContext || window.webkitAudioContext;
  if (!AudioContext) return null;
  if (!wheelAudioContext) {
    try { wheelAudioContext = new AudioContext(); }
    catch (_) { return null; }
  }
  if (wheelAudioContext.state === "suspended") {
    wheelAudioContext.resume().catch(() => {});
  }
  suspendWheelAudioWhenQuiet();
  return wheelAudioContext;
}

// Every sound restarts the countdown, so the context is suspended only after
// the last tone has fully decayed and the stream has rendered silence.
function suspendWheelAudioWhenQuiet() {
  clearTimeout(wheelAudioIdleTimer);
  wheelAudioIdleTimer = setTimeout(suspendWheelAudio, 2000);
}

function suspendWheelAudio() {
  clearTimeout(wheelAudioIdleTimer);
  wheelAudioIdleTimer = 0;
  if (wheelAudioContext?.state === "running") wheelAudioContext.suspend().catch(() => {});
}

function playWheelTone(frequency, gainValue = 0.09, duration = 0.035, delay = 0) {
  const audio = ensureWheelAudio();
  if (!audio) return;
  if (audio.state === "suspended") {
    audio.resume().then(() => {
      if (audio.state === "running") playWheelTone(frequency, gainValue, duration, delay);
    }).catch(() => {});
    return;
  }
  if (audio.state !== "running") return;
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
    if (!activeWheel || activeWheel.id !== wheel.id || activeWheel.phase !== "started" || !$("wheel-dialog").open || dismissedWheelID === wheel.id) return;
    const serverNow = Date.now() - wheelServerOffsetMs;
    const raw = (serverNow - Number(wheel.startsAtUnixMs)) / Math.max(1, Number(wheel.durationMs));
    const progress = preferences.reduceMotion ? 1 : Math.max(0, Math.min(1, raw));
    const eased = 1 - Math.pow(1 - progress, 5);
    wheelRotation = target * eased;
    // The next peg pushes the pointer aside as it arrives and lets it snap
    // back once it has passed.
    const passed = (Math.abs(wheelRotation) % arc) / arc;
    const flick = preferences.reduceMotion || progress >= 1 ? 0 : Math.max(0, (passed - 0.62) / 0.38);
    drawWheel(wheel, wheelRotation, -1, -0.36 * flick, target);
    const tick = Math.floor(Math.abs(wheelRotation) / arc);
    if (!preferences.reduceMotion && !document.hidden && raw >= 0 && raw < 1 && tick !== lastWheelTick) {
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
  $("wheel-count").textContent = `${wheel.items?.length || 0} queue item${wheel.items?.length === 1 ? "" : "s"}`;
  if (wheel.phase === "started") {
    const changed = activeWheel?.id !== wheel.id;
    if (changed) dismissedWheelID = null;
    activeWheel = wheel;
    if (dismissedWheelID !== wheel.id && !dialog.open) dialog.showModal();
    $("wheel-status").textContent = `${wheel.requesterName || "A participant"} is spinning…`;
    dialog.classList.remove("has-winner");
    $("wheel-spin-again").disabled = true;
    if (changed && dismissedWheelID !== wheel.id) {
      // The server starts the spin after a short lead-in; the suspended audio
      // resumes during it.
      ensureWheelAudio();
      animateWheel(wheel);
    }
  } else if (wheel.phase === "completed") {
    const wasSpinning = activeWheel?.id === wheel.id && activeWheel?.phase === "started";
    activeWheel = wheel;
    cancelAnimationFrame(wheelAnimation);
    if (dismissedWheelID !== wheel.id && !dialog.open) dialog.showModal();
    wheelRotation = wheelTargetRotation(wheel);
    drawWheel(wheel, wheelRotation, wheel.winner);
    $("wheel-status").textContent = "Selected for the room";
    dialog.classList.add("has-winner");
    $("wheel-candidate").textContent = wheel.items?.[wheel.winner]?.label || "Queue item selected";
    $("wheel-spin-again").disabled = !canControl();
    if (wasSpinning && dialog.open && !document.hidden && dismissedWheelID !== wheel.id) playWheelResult();
  } else if (wheel.phase === "cancelled") {
    activeWheel = null;
    dismissedWheelID = null;
    cancelAnimationFrame(wheelAnimation);
    dialog.close();
    showToast(wheel.reason || "The wheel spin was cancelled", "error");
  }
}

function openWheelWindow() {
  ensureWheelAudio();
  const dialog = $("wheel-dialog");
  if (!dialog.open) dialog.showModal();
  dismissedWheelID = null;
  if (activeWheel?.phase === "started") animateWheel(activeWheel);
  const items = (snapshot?.playlist?.items || []).map(({ id, label }) => ({ id, label }));
  if (!activeWheel) {
    const preview = { items, winner: 0, turns: 0, visualSeed: 0 };
    wheelRotation = 0;
    drawWheel(preview, 0);
    $("wheel-status").textContent = items.length >= 2 ? "The server chooses one item for everyone" : "Add at least two queue items";
    dialog.classList.remove("has-winner");
    $("wheel-candidate").textContent = items.length ? "Ready to spin" : "Queue is empty";
    $("wheel-count").textContent = `${items.length} queue item${items.length === 1 ? "" : "s"}`;
    $("wheel-spin-again").disabled = !canControl() || items.length < 2;
  }
}

function dismissWheelWindow() {
  if (activeWheel) dismissedWheelID = activeWheel.id;
  cancelAnimationFrame(wheelAnimation);
  if ($("wheel-dialog").open) $("wheel-dialog").close();
}

function mismatchCount(media) {
  return media ? (snapshot?.participants || []).filter((person) => person.media && person.media.fingerprint !== media.fingerprint).length : 0;
}

// The badge reports this participant's own player: other people's positions
// are not shared, so "In sync" claims nothing about them.
function renderSyncBadge(media = referenceMedia(), mismatches = mismatchCount(media)) {
  const badge = $("sync-state");
  let text = "Waiting for media", tone = "", title = "";
  if (media && mismatches) {
    text = "Mismatch"; tone = "badge-warning";
    title = "Someone has a different file loaded; their playback is not synced";
  } else if (media) {
    const drift = Math.abs(Number(syncStatus.driftSeconds) || 0);
    const behind = Number(syncStatus.driftSeconds) > 0;
    switch (syncStatus.state) {
      case "synced":
        text = "In sync"; tone = "badge-success";
        title = `Your player is within ${drift < 0.05 ? "0.05" : drift.toFixed(2)} s of the room`;
        break;
      case "catching-up":
        text = "Syncing"; tone = "badge-warning";
        title = `Your player is ${drift.toFixed(1)} s ${behind ? "behind" : "ahead of"} the room and is catching up`;
        break;
      case "buffering":
        text = "Buffering"; tone = "badge-warning";
        title = "Your player is buffering";
        break;
      default:
        text = "Waiting for player";
        title = "Sync starts once your player has the media open";
    }
  }
  // Drift updates arrive every second or so; unchanged values are not
  // rewritten, so the badge is only restyled when it changes.
  if (badge.textContent !== text) badge.textContent = text;
  if (badge.title !== title) badge.title = title;
  const className = `badge badge-sync ${tone}`;
  if (badge.className !== className) badge.className = className;
}

function renderMediaKind(media) {
  const badge = $("media-kind");
  if (!media) {
    badge.textContent = "No media loaded";
    badge.title = "";
    return;
  }
  const parts = media.sizeBytes ? [formatBytes(media.sizeBytes)] : [];
  if (streamState.state === "active") {
    parts.push("Direct P2P");
    const { cachedBytes = 0, totalBytes = 0, bytesPerSecond = 0 } = streamState;
    if (totalBytes > 0) {
      const percent = Math.floor(cachedBytes / totalBytes * 100);
      parts.push(cachedBytes >= totalBytes ? "Cached" : `${percent}% cached`);
      if (bytesPerSecond >= 1024 && cachedBytes < totalBytes) parts.push(`${formatBytes(bytesPerSecond)}/s`);
    }
  } else {
    parts.push(media.fingerprint ? "Local media" : "Stream");
  }
  badge.textContent = parts.join(" · ");
  badge.title = streamState.state === "active" && streamState.totalBytes > 0
    ? `${formatBytes(streamState.cachedBytes) || "0 B"} of ${formatBytes(streamState.totalBytes)} cached on this device`
    : "";
}

function formatBytes(bytes) {
  if (!bytes) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  return `${(bytes / 1024 ** index).toFixed(index > 1 ? 1 : 0)} ${units[index]}`;
}

// Rebuilding a list replaces the element under the pointer (hover flicker),
// the focused control and any open role menu, so lists are rebuilt only when
// what they display has changed.
let participantsRenderKey = "";
function renderParticipants(referenceMedia) {
  const me = self();
  const list = $("participants");
  if (!list || !snapshot) return;
  if ($("people-count")) $("people-count").textContent = snapshot.participants.length;
  const key = JSON.stringify([snapshot.selfId, me?.role, referenceMedia?.fingerprint || "", snapshot.participants.map((person) => [person.id, person.name, person.role, person.media?.fingerprint || ""])]);
  if (key === participantsRenderKey && list.childElementCount === snapshot.participants.length) return;
  participantsRenderKey = key;
  document.querySelectorAll(".participant-custom-menu").forEach((menu) => menu.remove());

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

// Owners can remove anyone but another owner; moderators can remove members.
function canKick(person) {
  const role = self()?.role;
  if (!person || person.id === snapshot?.selfId || person.role === "owner") return false;
  return role === "owner" || role === "moderator" && person.role === "member";
}

async function kickParticipant(person) {
  const confirmed = await askConfirmation(`Remove ${person.name}?`, `${person.name} leaves the room now. They can rejoin with an invite.`, "Remove");
  if (!confirmed) return;
  await invoke("KickParticipant", person.id).catch(showError);
}

function capitalize(value) { return value ? value[0].toUpperCase() + value.slice(1) : ""; }

let playlistRenderKey = "";
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
  const key = JSON.stringify([
    items, snapshot.playlist.selected, allowed, availability, streamingAvailable, streamState,
    snapshot.selfId, snapshot.streamOffers || [], preferences.youtubeQualities || {},
    snapshot.participants.map((person) => [person.id, person.availableMedia])
  ]);
  if (key === playlistRenderKey && list.childElementCount === items.length) return;
  playlistRenderKey = key;
  list.replaceChildren(...items.map((item, index) => {
    const row = document.createElement("li");
    row.className = `queue-item ${index === snapshot.playlist.selected ? "active" : ""}`;
    row.draggable = false;
    row.dataset.index = String(index);
    row.dataset.id = item.id;

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
      // Quality is a personal preference, so it stays available in moderated rooms.
      const qualityButton = actionButton("Quality", () => showYouTubeQualityMenu(qualityButton, item.url), false, `YouTube quality: ${savedYouTubeQuality(item.url) ? `${savedYouTubeQuality(item.url)}p` : "Auto"}`);
      qualityButton.setAttribute("aria-haspopup", "menu");
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
    actions.append(actionButton("×", () => removePlaylistItem(item.id), !allowed, "Remove from queue"));

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
  let targetIndex = sourceIndex, targetID = sourceRow.dataset.id;
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
      targetID = target.dataset.id;
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
    // Rows are not rebuilt during a drag, so their IDs are the ones the
    // person saw; the backend moves them wherever they are now.
    if (targetIndex !== sourceIndex) movePlaylistItem(sourceRow.dataset.id, targetID);
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
    // The ID picks the right item even if the queue moved meanwhile.
    await invoke("SelectPlaylist", index, item.id);
    await invoke("SetPaused", false);
  } catch (error) { showError(error); }
}

function friendsMissing(media) {
  return Boolean(media?.fingerprint && (snapshot?.participants || []).some((person) => person.id !== snapshot.selfId && !(person.availableMedia || []).includes(media.fingerprint)));
}

function isPlaylistItemPlayable(item) {
  if (!item) return false;
  if (item.url || availability[item.id]) return true;
  return (snapshot?.streamOffers || []).some((offer) => offer.media?.fingerprint === item.media?.fingerprint && (
    offer.id === streamState.offerId && streamState.state === "active" || offer.providerId !== snapshot.selfId && offer.viewerCount < offer.maxViewers
  ));
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
  return selected >= 0 ? snapshot.playlist.items[selected]?.url || "" : "";
}

function rememberPlaylist(previous) {
  if (!previous) return;
  playlistHistory.push(previous);
  if (playlistHistory.length > 20) playlistHistory.shift();
}

// Replaces the whole queue. Edits computed from the queue on screen (a
// shuffle, an undo) pass its revision, so if a friend changed the queue in the
// meantime the server refuses instead of silently discarding their change.
async function updatePlaylist(items, { remember = true, basedOnScreen = false } = {}) {
  const previous = remember && snapshot ? playlistInputs() : null;
  const baseRevision = basedOnScreen && snapshot ? snapshot.playlist.revision : null;
  await invoke("SetPlaylist", items, baseRevision);
  rememberPlaylist(previous);
}

// Single-item edits go by item ID and are applied by the backend to the
// latest queue, so they never overwrite a concurrent edit.
async function editPlaylistItem(method, ...args) {
  const previous = snapshot ? playlistInputs() : null;
  await invoke(method, ...args);
  rememberPlaylist(previous);
}

async function removePlaylistItem(itemID) {
  try { await editPlaylistItem("RemovePlaylistItem", itemID); } catch (error) { showError(error); }
}
// Moves an item to the place targetID holds, as dropping one row onto another.
async function movePlaylistItem(itemID, targetID) {
  if (!itemID || !targetID || itemID === targetID) return;
  try { await editPlaylistItem("MovePlaylistItem", itemID, targetID); } catch (error) { showError(error); }
}
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
  await invoke("Connect", request);
  snapshot = normalizeSnapshot(await invoke("Snapshot"));
  hosted = await invoke("ServerStatus");
  setConnection({ state: "connected" });
  render();
  await invoke("SetSponsorBlockEnabled", preferences.sponsorBlock !== false);
  await invoke("SetAutoOfferEnabled", preferences.autoOffer !== false);
  await invoke("SetChatOverlayEnabled", preferences.chatOverlay !== false);
  await invoke("SetStreamCacheLimit", streamCacheLimit());
  // Indexing can hash thousands of files. The room is already usable, so keep
  // discovery in the background and refresh availability as results arrive.
  void indexSavedDirectories();
  addPendingLaunchPaths();
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
    return true;
  } catch (error) { showError(error); return false; }
}

async function copyInvite(owner = false) {
  try {
    await copyText(await invoke(owner ? "OwnerInvite" : "ParticipantInvite"), owner ? "Owner recovery invite copied" : "Invite link copied");
  } catch (error) { showError(error); }
}

// New items are appended by the backend to the room's latest queue, so a
// friend's edit made while files are being inspected is not overwritten.
async function appendToPlaylist(additions, play = false) {
  const previous = snapshot ? playlistInputs() : null;
  const count = await invoke("AppendPlaylist", additions, play);
  rememberPlaylist(previous);
  return count;
}

async function addMediaPaths(paths, playImmediately = false) {
  if (!snapshot || !canControl() || !paths?.length) return;
  try {
    const expanded = hasBackend ? await invoke("ExpandMediaPaths", paths) : paths;
    if (!expanded.length) {
      showToast("No media files found there", "error");
      return;
    }
    const play = playImmediately && expanded.length === 1;
    const additions = expanded.map((source) => ({ id: "", label: basename(source), source, url: "", media: null }));
    const count = await appendToPlaylist(additions, play);
    showToast(play ? `Opening ${basename(expanded[0])}` : `${count} item${count === 1 ? "" : "s"} added to queue`);
  } catch (error) { showError(error); }
}

// Files opened with Faro from a file manager wait until there is a room.
let pendingLaunchPaths = [];
async function collectLaunchPaths() {
  if (!hasBackend) return;
  try { pendingLaunchPaths.push(...((await invoke("TakeLaunchPaths")) || [])); } catch (_) { return; }
  addPendingLaunchPaths();
}

function addPendingLaunchPaths() {
  if (!pendingLaunchPaths.length) return;
  if (!snapshot) {
    const what = pendingLaunchPaths.length === 1 ? basename(pendingLaunchPaths[0]) : `${pendingLaunchPaths.length} files`;
    showToast(`Join or host a room to add ${what} to the queue`);
    return;
  }
  const paths = pendingLaunchPaths;
  pendingLaunchPaths = [];
  if (!canControl()) {
    showToast("Only moderators can add to this room's queue", "error");
    return;
  }
  void addMediaPaths(paths);
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
  const height = popover.offsetHeight;
  const below = rect.bottom + 4;
  const top = below + height > innerHeight - 8 && rect.top - height - 4 >= 8 ? rect.top - height - 4 : below;
  popover.style.top = `${Math.max(8, Math.min(innerHeight - height - 8, top))}px`;
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

function menuButton(label, action, { checked = false, disabled = false, danger = false, hint = "" } = {}) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = `popover-btn ${danger ? "danger-item" : ""}`;
  button.disabled = disabled;
  button.setAttribute("role", checked ? "menuitemradio" : "menuitem");
  if (checked) button.setAttribute("aria-checked", "true");
  const mark = document.createElement("span");
  mark.className = "menu-check";
  mark.textContent = checked ? "✓" : "";
  const text = document.createElement("span");
  text.className = "menu-label";
  text.textContent = label;
  button.append(mark, text);
  if (hint) {
    const keys = document.createElement("kbd");
    keys.className = "menu-hint";
    keys.textContent = document.body.dataset.platform === "mac" ? hint.replace(/^Ctrl\+/, "⌘") : hint;
    button.append(keys);
  }
  button.onclick = action;
  return button;
}

function menuSeparator() {
  const line = document.createElement("hr");
  line.className = "popover-sep";
  return line;
}

// Drops separators at the edges and doubled ones left by omitted items.
function tidyMenu(items) {
  const result = [];
  for (const item of items.filter(Boolean)) {
    const separator = item.classList.contains("popover-sep");
    if (separator && (!result.length || result.at(-1).classList.contains("popover-sep"))) continue;
    result.push(item);
  }
  while (result.length && result.at(-1).classList.contains("popover-sep")) result.pop();
  return result;
}

function enhanceSelect(select) {
  if (!select || select.dataset.enhanced === "true") return;
  select.dataset.enhanced = "true";
  select.classList.add("native-select-source");
  const wrapper = document.createElement("div");
  wrapper.className = `custom-select ${select.classList.contains("select-compact") || select.classList.contains("participant-role-select") ? "compact" : ""}`;
  if (select.dataset.selectClass) wrapper.classList.add(select.dataset.selectClass);
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
  if (select.dataset.selectClass) menu.classList.add(`${select.dataset.selectClass}-menu`);
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

// remember offers a "Don't ask again" box; its answer is left in
// #confirm-remember for the caller to read once the question resolves.
function askConfirmation(title, message, acceptLabel = "Continue", { remember = false } = {}) {
  const dialog = $("confirm-dialog");
  // Only one question at a time; a newer one answers the older with "no".
  if (dialog.open) dialog.close("cancel");
  $("confirm-remember").checked = false;
  $("confirm-remember-row").classList.toggle("hidden", !remember);
  $("confirm-title").textContent = title;
  $("confirm-message").textContent = message;
  $("confirm-accept").textContent = acceptLabel;
  // Escape closes the dialog without a value, which would otherwise keep the
  // previous answer and count as "accept".
  dialog.returnValue = "";
  dialog.showModal();
  $("confirm-cancel").focus();
  return new Promise((resolve) => dialog.addEventListener("close", () => resolve(dialog.returnValue === "accept"), { once: true }));
}

// Leaving a room this Faro hosts stops its server and disconnects everyone.
async function confirmLeaveRoom() {
  if (!hosted.running) return true;
  return askConfirmation("End the room?", "You are hosting this room. Leaving stops it and disconnects everyone watching.", "End room");
}

async function leaveRoomWithConfirmation() {
  if (await confirmLeaveRoom()) await leaveRoom();
}

async function leaveRoom() {
  if (preferences.pauseOnLeave && canControl() && snapshot && !snapshot.playback.paused) {
    try { await invoke("SetPaused", true); } catch (error) { showError(error); }
  }
  try { await invoke("LeaveRoom"); }
  catch (error) { showError(error); }
  closeRoomView();
}

// The backend has already left the room; a server this participant hosts
// keeps running.
async function leaveRoomAfterKick(message) {
  closeRoomView();
  hosted = await invoke("ServerStatus").catch(() => ({ running: false }));
  renderConnectionInfo();
  showError(message || "You were removed from the room");
}

function closeRoomView() {
  hosted = { running: false };
  snapshot = null; timeline = []; playlistHistory = [];
  playlistRenderKey = ""; participantsRenderKey = ""; availabilityKey = "";
  renderChat();
  streamingAvailable = false;
  streamState = { state: "idle", offerId: "", route: "" };
  syncStatus = { state: "idle", driftSeconds: 0 };
  timelineSegments = []; timelineSegmentKey = ""; timelineSegmentsRetryKey = "";
  isScrubbing = false;
  seekCommandPending = false;
  clearTimeout(seekReleaseTimer);
  $("settings-dialog").close();
  if ($("wheel-dialog").open) $("wheel-dialog").close();
  activeWheel = null;
  dismissedWheelID = null;
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
$("window-titlebar").ondblclick = titlebarDoubleClick;
document.querySelector(".app-command-bar")?.addEventListener("dblclick", titlebarDoubleClick);
document.querySelector(".sidebar-brand")?.addEventListener("dblclick", titlebarDoubleClick);
window.addEventListener("blur", () => document.body.classList.add("window-inactive"));
window.addEventListener("focus", () => document.body.classList.remove("window-inactive"));

// Connection forms wiring
$("join-tab").onclick = () => switchConnectMode("join");
$("host-tab").onclick = () => switchConnectMode("host");
// A connection attempt can take a while (a relayed peer, an unreachable
// server), so the form offers Cancel until it finishes.
let connectAttempt = null;
async function withCancellableConnect(prefix, task) {
  const attempt = { cancelled: false };
  connectAttempt = attempt;
  const cancel = $(`${prefix}-cancel`);
  cancel.classList.remove("hidden");
  try {
    await task(attempt);
    // Cancel pressed just as the room opened: leave it again.
    if (attempt.cancelled && snapshot) await leaveRoom();
  } catch (error) {
    if (!attempt.cancelled) $(`${prefix}-error`).textContent = errorText(error);
  } finally {
    if (connectAttempt === attempt) connectAttempt = null;
    cancel.classList.add("hidden");
  }
}
for (const prefix of ["join", "host"]) {
  $(`${prefix}-cancel`).onclick = () => {
    if (!connectAttempt) return;
    connectAttempt.cancelled = true;
    $(`${prefix}-error`).textContent = "";
    invoke("CancelConnect").catch(() => {});
  };
}

$("join-form").onsubmit = async (event) => {
  event.preventDefault(); $("join-error").textContent = "";
  const button = event.submitter || $("join-form").querySelector("[type=submit]");
  await withButtonLoading(button, () => withCancellableConnect("join", async () => {
    rememberConnectPreferences("join");
    await enterRoom(connectionRequest("join", $("join-invite").value.trim()));
  }), "Connecting");
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
  await withButtonLoading(button, () => withCancellableConnect("host", async () => {
    let startedServer = false;
    try {
      rememberConnectPreferences("host");
      hosted = await invoke("StartServer", {
        mode: $("host-mode").value,
        listenAddress: $("host-listen").value.trim(),
        publicHost: $("host-public").value.trim(),
        room: $("host-room").value.trim(),
        protected: $("host-protected").checked
      });
      startedServer = true;
      await enterRoom(connectionRequest("host", hosted.localInvite));
    } catch (error) {
      if (startedServer) {
        try { await invoke("StopServer"); } catch (_) {}
        hosted = { running: false };
      }
      throw error;
    }
  }), "Starting");
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
    document.querySelectorAll("[data-settings]").forEach((node) => node.classList.toggle("active", node.dataset.settings === "session"));
    document.querySelectorAll("[data-page]").forEach((page) => page.classList.toggle("hidden", page.dataset.page !== "session"));
    openPreferences();
  };
}
$("leave-room").onclick = (event) => { event.stopPropagation(); togglePopover($("session-menu"), $("leave-room")); };
$("disconnect").onclick = () => { closePopovers(); leaveRoomWithConfirmation().catch(showError); };
$("settings-leave").onclick = async () => {
  if (await confirmLeaveRoom()) withButtonLoading($("settings-leave"), leaveRoom, "Leaving").catch(showError);
};
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
$("rate-decrease").onclick = () => nudgePlaybackRate(-1);
$("rate-increase").onclick = () => nudgePlaybackRate(1);
$("playback-rate").onchange = (event) => {
  const rate = Number(event.target.value);
  if (rate > 0 && rate !== Number(event.target.dataset.value)) invoke("SetRate", rate).catch(showError);
};
$("previous-media").onclick = () => playPlaylist(snapshot.playlist.selected - 1);
$("next-media").onclick = () => playPlaylist(snapshot.playlist.selected + 1);

$("copy-invite").onclick = () => hosted.running && hosted.shareInvite ? copyText(hosted.shareInvite, "Invite copied") : copyInvite(false);
$("settings-copy-invite").onclick = () => $("copy-invite").click();
$("owner-invite").onclick = () => copyInvite(true);
$("settings-owner-invite").onclick = () => copyInvite(true);
$("stop-server").onclick = () => { closePopovers(); leaveRoomWithConfirmation().catch(showError); };

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
    await appendToPlaylist([{ id: "", label, source, url: "", media: null, durationSeconds: Number(info?.duration) || 0 }]);
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
  try { await updatePlaylist(previous, { remember: false, basedOnScreen: true }); }
  catch (error) { playlistHistory.push(previous); showError(error); }
};
$("shuffle-playlist").onclick = () => {
  closePopovers();
  const items = playlistInputs(), selected = snapshot.playlist.selected, start = selected >= 0 ? selected + 1 : 0, tail = items.splice(start);
  updatePlaylist([...items, ...shuffled(tail)], { basedOnScreen: true }).catch(showError);
};
$("playlist-more").onclick = (event) => { event.stopPropagation(); togglePopover($("playlist-menu"), $("playlist-more")); };
$("add-stream-focus").onclick = () => {
  closePopovers();
  streamDrawer.classList.remove("hidden");
  $("playlist-source").focus();
};
$("save-playlist").onclick = () => { closePopovers(); copyText(snapshot.playlist.items.map((item) => item.url || item.label).join("\n"), "Playlist copied as text"); };
$("shuffle-all").onclick = () => { closePopovers(); updatePlaylist(shuffled(playlistInputs()), { basedOnScreen: true }).catch(showError); };
$("load-playlist-file").onclick = () => loadPlaylistFromFile();
$("save-playlist-file").onclick = async () => { closePopovers(); try { const path = await invoke("SavePlaylistFile"); if (path) showToast("Playlist saved"); } catch (error) { showError(error); } };
$("spin-wheel").onclick = () => { closePopovers(); openWheelWindow(); };
$("wheel-dialog").addEventListener("cancel", () => {
  if (activeWheel) dismissedWheelID = activeWheel.id;
  cancelAnimationFrame(wheelAnimation);
});
$("wheel-dialog").addEventListener("close", () => {
  if ($("wheel-dialog").open) return;
  if (activeWheel) dismissedWheelID = activeWheel.id;
  cancelAnimationFrame(wheelAnimation);
});
$("close-wheel").onclick = dismissWheelWindow;
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
  appendTimeline({ kind: "chat", value: chat, sentAtUnixMs: chat.sentAtUnixMs });
}

function addActivity(activity) {
  appendTimeline({ kind: "activity", value: activity, sentAtUnixMs: activity.sentAtUnixMs });
}

// New entries are appended instead of rebuilding the feed, and the feed only
// follows new messages while the reader is already at the bottom.
function appendTimeline(entry) {
  timeline.push(entry);
  if (timeline.length > 500) timeline = timeline.slice(-500);
  const chatStream = $("chat");
  if (!chatStream || (chatFilter !== "all" && entry.kind !== chatFilter)) return;
  const following = chatStream.scrollHeight - chatStream.scrollTop - chatStream.clientHeight < 24;
  chatStream.querySelector(".chat-empty")?.remove();
  chatStream.append(entry.kind === "activity" ? renderActivity(entry) : renderChatMessage(entry));
  while (chatStream.childElementCount > 500) chatStream.firstElementChild.remove();
  if (following) chatStream.scrollTop = chatStream.scrollHeight;
  else if (entry.kind === "chat") $("chat-new-messages")?.classList.remove("hidden");
}

// Shown when a message arrives while the reader is scrolled up in the feed.
$("chat").addEventListener("scroll", () => {
  const chatStream = $("chat");
  if (chatStream.scrollHeight - chatStream.scrollTop - chatStream.clientHeight < 24) $("chat-new-messages")?.classList.add("hidden");
});
$("chat-new-messages")?.addEventListener("click", () => {
  const chatStream = $("chat");
  chatStream.scrollTop = chatStream.scrollHeight;
  $("chat-new-messages").classList.add("hidden");
});

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
    case "playback.rate": return `${name} changed speed to ${rateLabel(Number(activity.rate || 1))}`;
    case "playlist.updated": return `${name} updated the queue · ${activity.itemCount} item${activity.itemCount === 1 ? "" : "s"}`;
    case "playlist.played": return `${name} played ${activity.itemLabel || "a queue item"}`;
    case "wheel.started": return `${name} spun the wheel`;
    case "wheel.completed": return `The wheel chose ${activity.itemLabel || "a queue item"}`;
    case "participant.kicked": return `${name} removed ${activity.targetName || "someone"} from the room`;
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
  if (action === "participant.kicked") return '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="10" cy="8" r="3.5"/><path d="M3.5 19a6.5 6.5 0 0 1 13 0M16 9h5"/></svg>';
  return '<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="3"/></svg>';
}

const timelineRows = new WeakMap();

function renderActivity(entry) {
  const activity = entry.value;
  const row = document.createElement("div");
  row.className = "activity-msg-row";
  timelineRows.set(row, entry);
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
  timelineRows.set(row, entry);
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
  $("chat-new-messages")?.classList.add("hidden");
}

// Menus and settings dialog
document.addEventListener("click", (event) => {
  // A click on an element that opens a menu must not close the menu it just
  // opened (the quality menu can open before its click finishes bubbling).
  if (!event.target.closest(".popover, .popover-menu, [aria-haspopup]")) {
    closePopovers();
  }
});
function openPopover() {
  return document.querySelector(".popover:not(.hidden), .popover-menu:not(.hidden), #context-menu:not(.hidden)");
}

document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    // Escape closes one layer at a time, as native menus do: an open menu
    // closes and the dialog under it stays.
    if (openPopover()) {
      event.preventDefault();
      event.stopPropagation();
      closePopovers();
    }
    return;
  }
  // Arrow keys, Home and End move through the open menu, as native menus do.
  if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
  const menu = [...document.querySelectorAll(".popover-menu:not(.hidden), #context-menu:not(.hidden)")].pop();
  if (!menu) return;
  const items = [...menu.querySelectorAll(".popover-btn")].filter((item) => !item.disabled && item.offsetParent !== null);
  if (!items.length) return;
  event.preventDefault();
  const position = items.indexOf(document.activeElement);
  const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1
    : event.key === "ArrowDown" ? (position + 1) % items.length : (position <= 0 ? items.length - 1 : position - 1);
  items[next].focus();
}, true);
$("room-chip").onclick = (event) => { event.stopPropagation(); togglePopover($("room-menu"), $("room-chip")); };
$("copy-room-name").onclick = () => { closePopovers(); copyText(snapshot.room.id, "Room name copied"); };
function openPreferences() {
  renderConnectionInfo();
  void refreshTrayStatus();
  void refreshQuitConfirmation();
  if (!$("settings-dialog").open) $("settings-dialog").showModal();
}
$("open-settings").onclick = openPreferences;
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


// Software updates. The backend owns the whole process (checking, the
// download, verification and the install); the page renders the status it
// reports through faro:update events and asks it to act.
function updateBusy(status = updateStatus) {
  return ["downloading", "installing", "restarting"].includes(status?.phase);
}

function updateVersionLabel(version) {
  return version ? `v${String(version).replace(/^v/i, "")}` : "";
}

function updatePercent(status) {
  if (!status?.downloadSize) return 0;
  return Math.max(0, Math.min(100, Math.floor((status.received / status.downloadSize) * 100)));
}

// Short progress text shared by the notices, the room button and the tray.
function updateProgressText(status) {
  switch (status.phase) {
    case "downloading": return status.downloadSize ? `Downloading · ${updatePercent(status)}%` : "Downloading…";
    case "installing": return "Installing…";
    case "restarting": return "Restarting…";
  }
  return "";
}

function applyUpdateStatus(status) {
  if (!status) return;
  const previous = updateStatus;
  updateStatus = status;
  renderUpdateNotices();
  renderUpdatePreferences();
  if ($("update-dialog").open) {
    if (!status.available && !updateBusy(status)) $("update-dialog").close();
    else renderUpdateDialog();
  }
  // Mention a newly found release once, without interrupting.
  if (status.available && status.phase === "idle" && status.latestVersion !== updateNotifiedVersion && previous && !previous.available
    && !$("update-dialog").open && !$("settings-dialog").open) {
    updateNotifiedVersion = status.latestVersion;
    showToast(`Faro ${updateVersionLabel(status.latestVersion)} is available`);
  }
}

function renderUpdateNotices() {
  const status = updateStatus;
  const visible = Boolean(status?.available);
  const busy = updateBusy(status);
  const failed = Boolean(status?.installError) && !busy;
  const version = updateVersionLabel(status?.latestVersion);
  document.querySelectorAll("[data-update-notice]").forEach((notice) => {
    notice.classList.toggle("hidden", !visible);
    if (!visible) return;
    notice.classList.toggle("busy", busy);
    notice.classList.toggle("failed", failed);
    notice.querySelector("[data-update-title]").textContent = busy ? "Updating Faro" : failed ? "Update didn't finish" : "Update available";
    notice.querySelector("[data-update-detail]").textContent = busy ? updateProgressText(status) : failed ? "Open to try again" : `Faro ${version} is ready`;
    notice.querySelector("[data-update-meter]").style.width = status.phase === "downloading" ? `${updatePercent(status)}%` : status.phase === "idle" ? "0%" : "100%";
    notice.setAttribute("aria-label", busy ? `Updating Faro: ${updateProgressText(status)}` : `Faro ${version} is available. Show the update.`);
  });
  const pill = $("update-pill");
  pill.classList.toggle("hidden", !visible);
  pill.classList.toggle("busy", busy);
  pill.classList.toggle("failed", failed);
  $("update-pill-label").textContent = status?.phase === "downloading" && status.downloadSize ? `${updatePercent(status)}%` : busy ? "Updating…" : "Update";
  pill.title = visible ? (busy ? updateProgressText(status) : `Faro ${version} is available`) : "";
}

function relativeTime(milliseconds) {
  const seconds = Math.max(0, Math.round((Date.now() - milliseconds) / 1000));
  if (seconds < 60) return "just now";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} hour${hours === 1 ? "" : "s"} ago`;
  const days = Math.round(hours / 24);
  return `${days} day${days === 1 ? "" : "s"} ago`;
}

function renderUpdatePreferences() {
  const status = updateStatus;
  if (!status) return;
  const current = updateVersionLabel(status.currentVersion);
  let text = `Faro ${current}`;
  let tone = "";
  if (status.phase === "checking") {
    text = "Checking for updates…";
    tone = "busy";
  } else if (status.available) {
    text = updateBusy(status) ? `Updating to ${updateVersionLabel(status.latestVersion)} · ${updateProgressText(status)}` : `Faro ${updateVersionLabel(status.latestVersion)} is available`;
    // A known release must not hide that the latest check failed.
    if (status.checkError && !updateBusy(status)) text += " · the last check for newer releases failed";
    tone = "available";
  } else if (status.checkError) {
    text = `Couldn't check for updates. ${status.checkError}`;
    tone = "error";
  } else if (status.checkedAt) {
    text = `Faro ${current} is up to date · checked ${relativeTime(status.checkedAt)}`;
    tone = "ok";
  }
  $("update-status-text").textContent = text;
  $("update-status-dot").className = `update-status-dot ${tone}`;
  $("update-check-now").disabled = status.phase !== "idle";
  $("update-check-now").textContent = status.available && !updateBusy(status) && !status.checkError ? "View update" : "Check now";
}

// Release notes are Markdown written by GitHub's release note generator or by
// hand. A small subset is rendered as text nodes: headings, bullets and
// paragraphs, with links reduced to their text and pull request credits to
// their number. Nothing is ever inserted as HTML.
function releaseNoteText(line) {
  return line
    .replace(/\s+by @[\w-]+ in (https:\/\/github\.com\/\S+\/pull\/(\d+))/g, " (#$2)")
    .replace(/https:\/\/github\.com\/\S+\/pull\/(\d+)/g, "#$1")
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/(\*\*|__|`)/g, "")
    .replace(/^\s*\*\s+/, "")
    .trim();
}

function renderReleaseNotes(container, markdown) {
  container.replaceChildren();
  let list = null;
  for (const raw of String(markdown || "").split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || /^\*\*Full Changelog\*\*/i.test(line) || /^<!--/.test(line)) { list = null; continue; }
    const heading = line.match(/^#{1,6}\s+(.*)$/);
    const bullet = line.match(/^[-*+]\s+(.*)$/);
    if (heading) {
      list = null;
      const text = releaseNoteText(heading[1]);
      if (!text || /^what'?s changed$/i.test(text)) continue;
      const element = document.createElement("h4");
      element.textContent = text;
      container.append(element);
    } else if (bullet) {
      const text = releaseNoteText(bullet[1]);
      if (!text) continue;
      if (!list) { list = document.createElement("ul"); container.append(list); }
      const item = document.createElement("li");
      item.textContent = text;
      list.append(item);
    } else {
      list = null;
      const text = releaseNoteText(line);
      if (!text) continue;
      const element = document.createElement("p");
      element.textContent = text;
      container.append(element);
    }
  }
  return container.childElementCount > 0;
}

function roomLeaveWarning() {
  if (!snapshot) return "";
  return hosted.running
    ? "You're hosting this room. Restarting ends it for everyone watching."
    : "Restarting takes you out of this room. You can rejoin with the same invite afterwards.";
}

function renderUpdateDialog() {
  const status = updateStatus;
  if (!status) return;
  const version = updateVersionLabel(status.latestVersion);
  const busy = updateBusy(status);
  $("update-title").textContent = busy
    ? (status.phase === "downloading" ? `Downloading Faro ${version}` : status.phase === "installing" ? `Installing Faro ${version}` : "Restarting Faro")
    : `Faro ${version} is available`;
  const details = [`You have ${updateVersionLabel(status.currentVersion)}`];
  if (status.publishedAt) details.push(`released ${new Date(status.publishedAt).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" })}`);
  if (status.downloadSize && status.canInstall) details.push(formatBytes(status.downloadSize));
  $("update-subtitle").textContent = details.join(" · ");

  const notesKey = `${status.latestVersion}\n${status.releaseNotes || ""}`;
  if ($("update-notes").dataset.key !== notesKey) {
    $("update-notes").dataset.key = notesKey;
    $("update-notes-section").classList.toggle("hidden", !renderReleaseNotes($("update-notes"), status.releaseNotes));
  }

  $("update-progress").classList.toggle("hidden", !busy);
  $("update-progress").classList.toggle("indeterminate", busy && (status.phase !== "downloading" || !status.downloadSize));
  if (busy) {
    const labels = { downloading: "Downloading update", installing: "Installing update", restarting: "Restarting Faro" };
    $("update-progress-label").textContent = labels[status.phase];
    $("update-progress-value").textContent = status.phase === "downloading" && status.downloadSize
      ? `${formatBytes(status.received) || "0 B"} of ${formatBytes(status.downloadSize)}`
      : "";
    $("update-progress-bar").style.width = status.phase === "downloading" && status.downloadSize ? `${updatePercent(status)}%` : "";
  }

  const note = status.phase === "installing" && status.method === "windows-installer"
    ? "Faro will close while the installer runs, then open again."
    : status.note || "";
  $("update-note").textContent = note;
  $("update-note").classList.toggle("hidden", !note || Boolean(status.installError && !busy));
  const warning = status.canInstall && status.phase !== "restarting" ? roomLeaveWarning() : "";
  $("update-warning").textContent = warning;
  $("update-warning").classList.toggle("hidden", !warning);
  const failed = Boolean(status.installError) && !busy;
  $("update-error").textContent = status.installError || "";
  $("update-error").classList.toggle("hidden", !failed);

  const install = $("update-install");
  install.disabled = busy;
  install.textContent = !status.canInstall ? "Download"
    : busy ? (status.phase === "downloading" ? "Downloading…" : status.phase === "installing" ? "Installing…" : "Restarting…")
    : failed ? "Try again"
    : snapshot && hosted.running ? "End room and update" : "Update and restart";
  install.classList.toggle("btn-danger-solid", Boolean(!busy && status.canInstall && snapshot && hosted.running));
  // Installing cannot be interrupted; the dialog can still be closed.
  $("update-later").textContent = status.phase === "downloading" ? "Cancel" : "Later";
  $("update-later").classList.toggle("hidden", status.phase === "installing" || status.phase === "restarting");
  $("update-open-download").classList.toggle("hidden", !(failed && status.downloaded));
}

function openUpdateDialog() {
  if (!updateStatus?.available) return;
  closePopovers();
  renderUpdateDialog();
  const dialog = $("update-dialog");
  if (!dialog.open) dialog.showModal();
  (updateBusy() ? $("update-later") : $("update-install")).focus();
}

function openUpdateRelease() {
  const url = updateStatus?.releaseUrl;
  if (url) wails?.Browser?.OpenURL(url).catch(showError);
}

async function checkForUpdatesNow() {
  if (updateStatus?.available && !updateBusy() && !updateStatus.checkError) { openUpdateDialog(); return; }
  try {
    const status = await invoke("CheckForUpdates");
    applyUpdateStatus(status);
    if (status.available) openUpdateDialog();
  } catch (error) {
    // The status line already explains a failed check; the toast is for a
    // build without an updater at all.
    if (!updateStatus?.checkError) showError(error);
  }
}

async function startUpdates() {
  if (!hasBackend) return;
  try { applyUpdateStatus(await invoke("UpdateStatus")); } catch { return; }
  wails.Events.On("faro:update", (event) => applyUpdateStatus(event.data));
  wails.Events.On("faro:open-update", () => openUpdateDialog());
  invoke("SetUpdateChecks", preferences.checkUpdates !== false).catch(() => {});
  if (updateStatus?.updatedFrom) {
    showToast(`Faro updated to ${updateVersionLabel(updateStatus.currentVersion)}`);
  }
  // Keep "checked 5 min ago" honest while Preferences is open.
  setInterval(() => { if ($("settings-dialog").open) renderUpdatePreferences(); }, 30000);
}

document.querySelectorAll("[data-update-notice], [data-update-open]").forEach((button) => {
  button.onclick = () => openUpdateDialog();
});
$("update-install").onclick = () => {
  if (!updateStatus?.canInstall) { openUpdateRelease(); return; }
  // The backend pauses the room itself right before restarting, when this
  // participant is still in one and allowed to.
  invoke("InstallUpdate", Boolean(preferences.pauseOnLeave)).catch(showError);
};
$("update-later").onclick = () => {
  if (updateStatus?.phase === "downloading") invoke("CancelUpdate").catch(showError);
  else $("update-dialog").close();
};
$("update-close").onclick = () => $("update-dialog").close();
$("update-release").onclick = openUpdateRelease;
$("update-open-download").onclick = () => invoke("OpenUpdateDownload").catch(showError);
$("update-dialog").addEventListener("click", (event) => { if (event.target === $("update-dialog")) $("update-dialog").close(); });
$("update-check-now").onclick = () => { void checkForUpdatesNow(); };
$("check-updates").onchange = (event) => {
  savePreferences({ checkUpdates: event.target.checked });
  invoke("SetUpdateChecks", event.target.checked).catch(() => {});
};

function cycleTheme() {
  // data-theme is already resolved, so the toggle never needs the system query.
  const order = ["dark", "midnight", "pine", "light", "sage"];
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
$("stream-cache-limit").onchange = (event) => {
  savePreferences({ streamCacheLimit: Number(event.target.value) || 0 });
  invoke("SetStreamCacheLimit", streamCacheLimit()).catch(showError);
};
$("chat-overlay-enabled").onchange = (event) => {
  savePreferences({ chatOverlay: event.target.checked });
  invoke("SetChatOverlayEnabled", event.target.checked).catch(showError);
};
$("auto-offer-enabled").onchange = (event) => {
  savePreferences({ autoOffer: event.target.checked });
  if (snapshot) invoke("SetAutoOfferEnabled", event.target.checked).catch(showError);
};
const systemThemeQuery = matchMedia("(prefers-color-scheme: dark)");
const systemThemeChanged = () => {
  if (preferences.theme === "system") applyPreferences();
};
// The media query reports real system theme changes. Re-applying the theme on
// every focus or visibility change restyled the whole page exactly when the
// compositor was redrawing the window, which showed up as flashing.
if (systemThemeQuery.addEventListener) systemThemeQuery.addEventListener("change", systemThemeChanged);
else systemThemeQuery.addListener?.(systemThemeChanged);

function showContextMenu(event, items) {
  const menu = $("context-menu");
  const container = event.target?.closest?.("dialog[open]") || document.body;
  if (menu.parentElement !== container) container.append(menu);
  menu.replaceChildren(...items);
  closePopovers();
  menu.classList.remove("hidden");
  const width = menu.offsetWidth;
  const height = menu.offsetHeight;
  menu.style.left = `${Math.max(8, Math.min(innerWidth - width - 8, event.clientX))}px`;
  menu.style.top = `${Math.max(8, Math.min(innerHeight - height - 8, event.clientY))}px`;
}

async function pasteIntoControl(control, start, end, originalValue) {
  try {
    const text = wails?.Clipboard ? await wails.Clipboard.Text() : await navigator.clipboard.readText();
    if (control.readOnly || control.disabled) return;
    if (control.value !== originalValue) {
      start = control.selectionStart ?? control.value.length;
      end = control.selectionEnd ?? start;
    }
    control.setRangeText(text, start, end, "end");
    control.dispatchEvent(new Event("input", { bubbles: true }));
  } catch (error) { showError(error); }
}

document.addEventListener("contextmenu", (event) => {
  const control = event.target.closest("input, textarea");
  const editableText = control instanceof HTMLTextAreaElement || control instanceof HTMLInputElement && ["text", "search", "tel", "url", "password"].includes(control.type);
  if (editableText) {
    event.preventDefault();
    const start = control.selectionStart ?? control.value.length;
    const end = control.selectionEnd ?? start;
    const selected = end > start;
    const writable = !control.readOnly && !control.disabled;
    const originalValue = control.value;
    showContextMenu(event, [
      menuButton("Cut", async () => { closePopovers(); if (await copyText(originalValue.slice(start, end), "Copied") && control.value === originalValue) { control.setRangeText("", start, end, "start"); control.dispatchEvent(new Event("input", { bubbles: true })); } }, { disabled: !selected || !writable }),
      menuButton("Copy", () => { closePopovers(); copyText(originalValue.slice(start, end), "Copied"); }, { disabled: !selected }),
      menuButton("Paste", () => { closePopovers(); pasteIntoControl(control, start, end, originalValue); }, { disabled: !writable }),
      menuButton("Select All", () => { control.focus(); control.select(); closePopovers(); })
    ]);
    return;
  }

  const queueRow = event.target.closest(".queue-item");
  if (queueRow && snapshot) {
    event.preventDefault();
    const index = Number(queueRow.dataset.index);
    const item = snapshot.playlist.items[index];
    if (!item) return;
    const selectedHere = index === snapshot.playlist.selected;
    // The queue can change while the menu is open, so actions find their item
    // by ID when chosen instead of trusting the row index captured now.
    const at = (task) => () => {
      closePopovers();
      const current = snapshot?.playlist?.items?.findIndex((entry) => entry.id === item.id) ?? -1;
      if (current < 0) { showToast("That queue item was removed", "error"); return; }
      task(current);
    };
    const controls = [selectedHere
      ? menuButton("Resume", () => { closePopovers(); invoke("SetPaused", false).catch(showError); }, { disabled: !canControl() || !snapshot.playback.paused || !isPlaylistItemPlayable(item) })
      : menuButton("Play now", at((current) => playPlaylist(current)), { disabled: !canControl() || !isPlaylistItemPlayable(item) })];
    if (!item.url && item.media && !availability[item.id]) controls.push(menuButton("Locate matching file…", () => { closePopovers(); locateItem(item.id); }));
    const ownOffer = (snapshot.streamOffers || []).find((offer) => offer.providerId === snapshot.selfId);
    if (ownOffer && item.media && ownOffer.media?.fingerprint === item.media.fingerprint) controls.push(menuButton("Stop sharing file", () => { closePopovers(); invoke("StopOfferingStream").then(() => showToast("File sharing stopped")).catch(showError); }));
    else if (!ownOffer && !item.url && availability[item.id] && streamingAvailable && friendsMissing(item.media)) controls.push(menuButton("Share file", () => { closePopovers(); invoke("OfferPlaylistStream", item.id).then(() => showToast("File sharing started")).catch(showError); }));
    controls.push(
      menuButton("Move up", at((current) => current > 0 && movePlaylistItem(item.id, snapshot.playlist.items[current - 1].id)), { disabled: !canControl() || index === 0 }),
      menuButton("Move down", at((current) => current < snapshot.playlist.items.length - 1 && movePlaylistItem(item.id, snapshot.playlist.items[current + 1].id)), { disabled: !canControl() || index === snapshot.playlist.items.length - 1 }),
      menuButton("Copy title", () => { closePopovers(); copyText(item.label, "Title copied"); }),
      menuButton("Remove from queue", at(() => removePlaylistItem(item.id)), { disabled: !canControl(), danger: true })
    );
    if (item.url) controls.splice(controls.length - 1, 0, menuButton("Copy video URL", () => { closePopovers(); copyText(item.url, "URL copied"); }));
    showContextMenu(event, controls);
    return;
  }

  const participantRow = event.target.closest(".participant-row");
  if (participantRow && snapshot) {
    const person = snapshot.participants.find((entry) => entry.id === participantRow.dataset.id);
    if (!person) return;
    event.preventDefault();
    const items = [menuButton("Copy name", () => { closePopovers(); copyText(person.name, "Name copied"); })];
    if (person.media?.title) items.push(menuButton("Copy media title", () => { closePopovers(); copyText(person.media.title, "Title copied"); }));
    if (self()?.role === "owner" && person.role !== "owner") {
      const role = person.role === "moderator" ? "member" : "moderator";
      items.push(menuButton(role === "moderator" ? "Make moderator" : "Make member", () => { closePopovers(); invoke("SetRole", person.id, role).catch(showError); }));
    }
    if (canKick(person)) items.push(menuSeparator(), menuButton("Remove from room…", () => { closePopovers(); kickParticipant(person); }, { danger: true }));
    showContextMenu(event, items);
    return;
  }

  // Every other area gets a menu for what it shows. Items mirror the visible
  // controls, so their enabled state follows the same permissions.
  const act = (task) => () => { closePopovers(); task(); };
  const open = (items) => {
    const menu = tidyMenu(items);
    if (!menu.length) return;
    event.preventDefault();
    showContextMenu(event, menu);
  };
  const selectedText = String(getSelection()?.toString() || "").trim();
  const copySelection = selectedText ? [menuButton("Copy", act(() => copyText(selectedText, "Copied")), { hint: "Ctrl+C" }), menuSeparator()] : [];
  const windowItems = () => document.body.dataset.platform === "mac" ? [] : [
    menuSeparator(),
    menuButton("Minimize", act(() => wails?.Window?.Minimise())),
    menuButton($("window-maximise").dataset.maximised === "true" ? "Restore" : "Maximize", act(() => wails?.Window?.ToggleMaximise())),
    menuButton("Close window", act(() => wails?.Window?.Close()))
  ];
  const chatFilterItems = () => [
    ["all", "Show everything"], ["chat", "Chat only"], ["activity", "Activity only"]
  ].map(([value, label]) => menuButton(label, act(() => document.querySelector(`[data-chat-filter="${value}"]`)?.click()), { checked: chatFilter === value }));
  const clearChatItem = () => menuButton("Clear feed", act(() => $("clear-chat").click()), { disabled: !timeline.length });

  if (event.target.closest("#wheel-dialog")) {
    open([
      ...copySelection,
      menuButton("Spin again", act(() => $("wheel-spin-again").click()), { disabled: $("wheel-spin-again").disabled }),
      menuButton("Close", act(dismissWheelWindow), { hint: "Esc" })
    ]);
    return;
  }

  if (event.target.closest("dialog[open]")) {
    open([...copySelection, menuButton("Close", act(() => event.target.closest("dialog").close()), { hint: "Esc" })]);
    return;
  }

  const timelineRow = event.target.closest(".chat-msg-row, .activity-msg-row");
  if (timelineRow && snapshot) {
    const entry = timelineRows.get(timelineRow);
    const isChat = entry?.kind === "chat";
    const text = isChat ? entry.value.message : entry ? activityDescription(entry.value) : timelineRow.textContent;
    open([
      ...copySelection,
      menuButton(isChat ? "Copy message" : "Copy text", act(() => copyText(text, "Copied"))),
      isChat ? menuButton("Copy author name", act(() => copyText(entry.value.participantName, "Name copied"))) : null,
      menuSeparator(), ...chatFilterItems(), menuSeparator(), clearChatItem()
    ]);
    return;
  }

  if (event.target.closest("#inspector-chat-panel") && snapshot) {
    open([...copySelection, ...chatFilterItems(), menuSeparator(), clearChatItem()]);
    return;
  }

  if (event.target.closest("#playlist-panel") && snapshot) {
    const allowed = canControl(), count = snapshot.playlist.items.length;
    open([
      ...copySelection,
      menuButton("Add media files…", act(() => chooseFiles()), { disabled: !allowed, hint: "Ctrl+O" }),
      menuButton("Add folder…", act(() => $("add-folder").click()), { disabled: !allowed }),
      menuButton("Add URL…", act(() => { streamDrawer.classList.remove("hidden"); $("playlist-source").focus(); }), { disabled: !allowed }),
      menuSeparator(),
      menuButton("Spin the wheel", act(openWheelWindow), { disabled: !allowed || count < 2 }),
      menuButton("Shuffle upcoming", act(() => $("shuffle-playlist").click()), { disabled: !allowed || count < 2 }),
      menuButton("Shuffle entire queue", act(() => $("shuffle-all").click()), { disabled: !allowed || count < 2 }),
      menuSeparator(),
      menuButton("Load playlist file…", act(loadPlaylistFromFile), { disabled: !allowed }),
      menuButton("Save queue to file…", act(() => $("save-playlist-file").click()), { disabled: !count }),
      menuButton("Copy queue as text", act(() => $("save-playlist").click()), { disabled: !count }),
      menuSeparator(),
      menuButton("Undo last queue change", act(() => $("undo-playlist").click()), { disabled: !allowed || !playlistHistory.length }),
      menuButton("Clear queue…", act(() => $("clear-playlist").click()), { disabled: !allowed || !count, danger: true })
    ]);
    return;
  }

  if (event.target.closest("#inspector-participants-panel") && snapshot) {
    const owner = self()?.role === "owner";
    const setMode = (mode) => act(() => invoke("SetRoomMode", mode).catch(showError));
    open([
      ...copySelection,
      menuButton("Copy invite link", act(() => $("copy-invite").click())),
      owner ? menuButton("Copy owner recovery invite", act(() => copyInvite(true))) : null,
      owner ? menuSeparator() : null,
      owner ? menuButton("Everyone controls playback", setMode("collaborative"), { checked: snapshot.room.mode === "collaborative" }) : null,
      owner ? menuButton("Moderators control playback", setMode("moderated"), { checked: snapshot.room.mode === "moderated" }) : null
    ]);
    return;
  }

  if (event.target.closest("#now-playing-drop") && snapshot) {
    const allowed = canControl(), media = referenceMedia(), paused = snapshot.playback.paused;
    const rate = Number(snapshot.playback.rate || 1), skip = Number(preferences.skipSeconds || 10);
    const title = $("media-title").textContent, source = selectedSourceURL();
    const speedAllowed = allowed && rateRange.supported;
    open([
      ...copySelection,
      menuButton(paused ? "Play" : "Pause", act(() => $("pause").click()), { disabled: !allowed, hint: "Space" }),
      menuButton(`Back ${skip} seconds`, act(() => $("back-ten").click()), { disabled: !allowed || !media, hint: "←" }),
      menuButton(`Forward ${skip} seconds`, act(() => $("forward-ten").click()), { disabled: !allowed || !media, hint: "→" }),
      menuSeparator(),
      menuButton("Slower", act(() => stepPlaybackRate(-1)), { disabled: !speedAllowed || rate <= rateRange.min, hint: "<" }),
      menuButton("Faster", act(() => stepPlaybackRate(1)), { disabled: !speedAllowed || rate >= rateRange.max, hint: ">" }),
      menuButton("Normal speed", act(() => invoke("SetRate", 1).catch(showError)), { disabled: !speedAllowed || rate === 1 }),
      menuSeparator(),
      menuButton("Copy timestamp", act(() => copyText(formatTime(projectedPlaybackPosition()), "Timestamp copied")), { disabled: !media }),
      menuButton("Copy media title", act(() => copyText(title, "Title copied")), { disabled: !media }),
      /^https?:\/\//i.test(source) ? menuButton("Copy video URL", act(() => copyText(source, "URL copied"))) : null
    ]);
    return;
  }

  if (event.target.closest(".app-command-bar, .window-titlebar") && snapshot) {
    open([
      ...copySelection,
      menuButton("Copy invite link", act(() => $("copy-invite").click())),
      menuButton("Copy room name", act(() => $("copy-room-name").click())),
      menuSeparator(),
      menuButton("Server & session info…", act(() => $("stats-open-session").click())),
      menuButton("Preferences…", act(openPreferences), { hint: "Ctrl+," }),
      menuSeparator(),
      menuButton("Leave room", act(() => leaveRoomWithConfirmation().catch(showError)), { danger: true }),
      ...windowItems()
    ]);
    return;
  }

  if (event.target.closest("#connect-view, .window-titlebar") && !snapshot) {
    const joining = !$("join-form").classList.contains("hidden");
    open([
      ...copySelection,
      menuButton("Join a room", act(() => switchConnectMode("join")), { checked: joining }),
      menuButton("Host a room", act(() => switchConnectMode("host")), { checked: !joining }),
      menuSeparator(),
      menuButton("Paste invite link", act(() => {
        switchConnectMode("join");
        const input = $("join-invite");
        input.focus();
        pasteIntoControl(input, 0, input.value.length, input.value);
      })),
      menuButton("Switch theme", act(cycleTheme)),
      ...windowItems()
    ]);
    return;
  }

  if (!snapshot && !selectedText) return;
  open([
    ...copySelection,
    snapshot ? menuButton("Copy invite link", act(() => $("copy-invite").click())) : null,
    snapshot ? menuButton("Preferences…", act(openPreferences), { hint: "Ctrl+," }) : null
  ]);
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
      await appendToPlaylist([{ id: "", label: info?.title || playlistSourceLabel(source), source, url: "", media: null, durationSeconds: Number(info?.duration) || 0 }]);
    } catch (error) { showError(error); }
  }
});

function receiveFileDrop(paths, target) {
  dragDepth = 0;
  document.body.classList.remove("drag-active");
  addMediaPaths(paths || [], target?.id === "now-playing-drop");
}

// Availability and streaming support change with the playlist or the
// connection, not with every room event.
let connectionEpoch = 0;
let availabilityKey = "";
let streamingCheckEpoch = -1;
function receive(event) {
  if (event.connection) {
    setConnection(event.connection);
    if (event.connection.state === "connected") {
      connectionEpoch++;
      refreshStreamingSupport();
    }
    if (event.connection.state === "disconnected") {
      streamingAvailable = false;
      streamState = { state: "idle", offerId: "", route: "" };
    }
  }
  if (event.snapshot) {
    snapshot = normalizeSnapshot(event.snapshot);
    render();
    const nextAvailabilityKey = `${connectionEpoch}|${snapshot.playlist.revision}|${snapshot.playlist.items.length}`;
    if (nextAvailabilityKey !== availabilityKey) {
      availabilityKey = nextAvailabilityKey;
      refreshAvailability();
    }
    if (!streamingAvailable && streamingCheckEpoch !== connectionEpoch) {
      streamingCheckEpoch = connectionEpoch;
      refreshStreamingSupport();
    }
    if (event.snapshot.playlistWheel) showWheel(event.snapshot.playlistWheel, event.serverNowUnixMs);
  }
  if (event.stream) {
    const next = event.stream;
    if (next.totalBytes) {
      // Progress ticks only refresh the badge, and a tick that arrives after
      // the stream ended is ignored.
      if (streamState.state === "active" && streamState.offerId === next.offerId) {
        Object.assign(streamState, {
          cachedBytes: next.cachedBytes || 0, totalBytes: next.totalBytes, bytesPerSecond: next.bytesPerSecond || 0,
          cachedRanges: next.cachedRanges || [],
        });
        if (snapshot) renderMediaKind(referenceMedia());
        renderStreamCache();
      }
    } else {
      streamState = { state: next.state || "idle", offerId: next.offerId || "", route: next.route || "" };
      if (snapshot) render();
    }
  }
  if (event.sync) {
    syncStatus = { state: event.sync.state || "idle", driftSeconds: Number(event.sync.driftSeconds) || 0 };
    if (snapshot) renderSyncBadge();
  }
  if (event.wheel) showWheel(event.wheel, event.serverNowUnixMs);
  if (event.chat) addChat(event.chat);
  if (event.activity) addActivity(event.activity);
  if (event.error?.code === "kicked") {
    void leaveRoomAfterKick(event.error.message);
    return;
  }
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
    // Only the topmost dialog closes; the confirmation sits above the others.
    event.preventDefault();
    if ($("confirm-dialog").open) $("confirm-dialog").close("cancel");
    else if ($("update-dialog").open) $("update-dialog").close();
    else if ($("wheel-dialog").open) dismissWheelWindow();
    else if ($("settings-dialog").open) $("settings-dialog").close();
    return;
  }
  // Playback shortcuts belong to the room view, not to an open dialog or menu,
  // and Space or Enter on a focused button activates that button.
  if (!snapshot || event.target.matches("input, select, textarea") || event.target.isContentEditable) return;
  if (document.querySelector("dialog[open]") || openPopover()) return;
  if (event.code === "Space" && event.target.closest("button, a, [role=button], [role=menuitem], [tabindex]:not(body)")) return;
  if (event.code === "Space" && canControl()) { event.preventDefault(); if (!event.repeat) $("pause").click(); }
  if (event.key === "ArrowLeft" && canControl()) { event.preventDefault(); $("back-ten").click(); }
  if (event.key === "ArrowRight" && canControl()) { event.preventDefault(); $("forward-ten").click(); }
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "o" && canControl()) { event.preventDefault(); chooseFiles(); }
  if ((event.ctrlKey || event.metaKey) && event.key === ",") { event.preventDefault(); openPreferences(); }
  if (!event.ctrlKey && !event.metaKey && !event.altKey && (event.key === ">" || event.key === "<")) { event.preventDefault(); stepPlaybackRate(event.key === ">" ? 1 : -1); }
});

if (wails?.Events) {
  wails.Events.On("faro:event", (event) => receive(event.data));
  wails.Events.On("faro:file-drop", (event) => receiveFileDrop(event.data?.paths, event.data?.target));
  wails.Events.On("faro:window-state", (event) => setWindowState(event.data));
  wails.Events.On("faro:window-chrome", (event) => {
    if (!event.data) return;
    windowChrome = { ...windowChrome, ...event.data };
    applyWindowChrome();
  });
  wails.Events.On("faro:open-files", () => { void collectLaunchPaths(); });
  // The backend asks the page to finish a quit that leaves a room; ask is
  // false once the user has turned the question off.
  wails.Events.On("faro:confirm-quit", async (event) => {
    const hosting = Boolean(event.data?.hosting);
    if (event.data?.ask !== false) {
      const quit = await askConfirmation("Quit Faro?", hosting
        ? "You are hosting this room. Quitting ends it for everyone watching."
        : "Quitting takes you out of the room.", "Quit", { remember: true });
      if (!quit) return;
      if ($("confirm-remember").checked) await setConfirmQuit(false).catch(showError);
    }
    if (preferences.pauseOnLeave && canControl() && snapshot && !snapshot.playback.paused) {
      try { await invoke("SetPaused", true); } catch (_) {}
    }
    invoke("Quit").catch(showError);
  });
}

setInterval(() => {
  if (document.hidden || !snapshot || snapshot.playback.paused || connectionState !== "connected" || isScrubbing) return;
  const projected = projectedPlaybackPosition();
  const duration = playbackDuration();
  if (duration) $("position").value = String(projected);
  updateScrubberProgress(projected, duration);
  $("current-time").textContent = formatTime(projected);
}, 250);

// Startup. On Linux the window stays hidden until WindowReady, so everything
// the first frame shows is settled before it is revealed.
applyPreferences();
await Promise.all([
  configurePlayerOptions(),
  loadWindowChrome(),
  hasBackend ? invoke("Version").then((value) => { appVersion = value; }).catch(() => {}) : null,
  hasBackend ? wails?.Window?.IsMaximised?.().then((maximised) => setWindowState({ maximised })).catch(() => {}) : null
]);
$("version").textContent = appVersion;
applyWindowChrome();
hydrateConnectForms();
enhanceAllSelects();
applyPreferences();
renderMediaDirectories();
renderConnectionInfo();
if (hasBackend) invoke("WindowReady").catch(() => {});
void collectLaunchPaths();
// Only WebKitGTK offers a choice of rendering path.
if (hasBackend && document.body.dataset.platform === "linux") {
  invoke("HardwareAcceleration").then((enabled) => {
    $("hardware-acceleration").checked = Boolean(enabled);
    $("hardware-acceleration-row").classList.remove("hidden");
  }).catch(() => {});
}
async function refreshQuitConfirmation() {
  if (!hasBackend) return;
  try {
    $("confirm-quit").checked = Boolean(await invoke("ConfirmQuit"));
    $("confirm-quit-row").classList.remove("hidden");
  } catch {}
}
async function setConfirmQuit(enabled) {
  await invoke("SetConfirmQuit", enabled);
  $("confirm-quit").checked = enabled;
}
void refreshQuitConfirmation();
$("confirm-quit").onchange = (event) => {
  setConfirmQuit(event.target.checked)
    .catch((error) => { event.target.checked = !event.target.checked; showError(error); });
};
async function refreshTrayStatus() {
  if (!hasBackend) return;
  try {
    const status = await invoke("TrayStatus");
    $("close-to-tray").checked = Boolean(status.closeToTray);
    $("close-to-tray-hint").textContent = status.available
      ? "Closing the window keeps rooms, hosting and shared files running. Quit from the tray icon."
      : "No system tray found, so closing the window quits Faro. On GNOME, enable the AppIndicator extension.";
    $("close-to-tray-row").classList.remove("hidden");
  } catch {}
}
void refreshTrayStatus();
$("close-to-tray").onchange = (event) => {
  invoke("SetCloseToTray", event.target.checked)
    .catch((error) => { event.target.checked = !event.target.checked; showError(error); });
};
$("hardware-acceleration").onchange = (event) => {
  invoke("SetHardwareAcceleration", event.target.checked)
    .then(() => showToast("Restart Faro to apply the rendering change"))
    .catch((error) => { event.target.checked = !event.target.checked; showError(error); });
};
void startUpdates();
