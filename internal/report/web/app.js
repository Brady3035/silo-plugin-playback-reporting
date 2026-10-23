// Playback Reporting page. Loads every play once (the plugin's /data chunks),
// then computes all reports here: the host's plugin launch cookie expires five
// minutes after the page opens, so the page does not go back for more.
(() => {
  "use strict";

  const DAY = 86400000;
  const STREAM_OF = { direct: "direct", remux: "remux", direct_stream: "remux", transcode: "transcode" };
  const STREAMS = [["direct", "Direct Play"], ["remux", "Remux"], ["transcode", "Transcode"], ["unknown", "Unknown"]];
  const CATS = [["movies", "Movies"], ["tv", "TV shows"], ["audiobooks", "Audiobooks"], ["other", "Other"]];
  const STREAM_LABEL = Object.fromEntries(STREAMS);
  const CAT_LABEL = Object.fromEntries(CATS);
  const WEEKDAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
  const FULL_DAYS = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"];
  const PAGE = 25;
  const MAX_SERIES = 8;

  const $ = (id) => document.getElementById(id);
  const statusEl = $("status");
  const main = $("main");
  // The back link returns to the admin page the report was opened from, or
  // to the admin home. Plugin pages live under /api/v2/plugin-content/, so
  // Silo's root is whatever comes before that.
  (function wireBack() {
    const back = $("back");
    const marker = location.pathname.indexOf("/api/v2/plugin-content/");
    const root = marker > 0 ? location.pathname.slice(0, marker).replace(/\/+$/, "") : "";
    let href = root + "/admin";
    try {
      const ref = new URL(document.referrer);
      if (ref.origin === location.origin && ref.pathname.startsWith(root + "/admin")) href = ref.pathname + ref.search;
    } catch (_) { /* no referrer */ }
    if (back) back.href = href;
    if (back) warmOnIntent(back);
  })();

  // Start loading the admin page as soon as the pointer or keyboard focus
  // lands on the back link, so the click lands on a page that is already
  // (or nearly) ready. Chromium gets a full prerender via speculation rules;
  // other browsers get a plain prefetch of the document. Done once per page.
  function warmOnIntent(link) {
    let warmed = false;
    const warm = () => {
      if (warmed) return;
      warmed = true;
      const url = link.href;
      try {
        if (HTMLScriptElement.supports && HTMLScriptElement.supports("speculationrules")) {
          const rules = document.createElement("script");
          rules.type = "speculationrules";
          rules.textContent = JSON.stringify({ prerender: [{ source: "list", urls: [url] }] });
          document.head.append(rules);
          return;
        }
      } catch (_) { /* fall through to prefetch */ }
      const hint = document.createElement("link");
      hint.rel = "prefetch";
      hint.href = url;
      document.head.append(hint);
    };
    let timer = 0;
    // A short delay skips pointers that are just passing over the link.
    link.addEventListener("pointerenter", () => { timer = setTimeout(warm, 65); });
    link.addEventListener("pointerleave", () => clearTimeout(timer));
    link.addEventListener("pointerdown", warm);
    link.addEventListener("touchstart", warm, { passive: true });
    link.addEventListener("focus", warm);
  }
  if (!main) return; // setup page

  const state = {
    plays: [],
    features: { clients: false },
    status: null,
    tab: remember("tab") || "activity",
    page: { users: 0, history: 0 },
    palette: 0,
  };

  // ---------- small DOM helpers (text only, never innerHTML) ----------
  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === undefined || v === null || v === false) continue;
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
      else if (k === "style") node.setAttribute("style", v);
      else node.setAttribute(k, v === true ? "" : v);
    }
    for (const c of children.flat(Infinity)) if (c !== null && c !== undefined && c !== false) node.append(c instanceof Node ? c : document.createTextNode(String(c)));
    return node;
  }
  const svgNS = "http://www.w3.org/2000/svg";
  function svg(tag, attrs, ...children) {
    const node = document.createElementNS(svgNS, tag);
    for (const [k, v] of Object.entries(attrs || {})) if (v !== undefined && v !== null) node.setAttribute(k, v);
    for (const c of children.flat()) if (c !== null && c !== undefined && c !== false) node.append(c instanceof Node ? c : document.createTextNode(String(c)));
    return node;
  }
  function remember(key, value) {
    try {
      if (value === undefined) return localStorage.getItem("playback-reporting:" + key);
      localStorage.setItem("playback-reporting:" + key, value);
    } catch (_) { /* storage can be unavailable */ }
    return null;
  }
  const empty = (text) => el("p", { class: "empty", text: text || "No plays match these filters." });

  // ---------- formatting ----------
  function hours(seconds) {
    const m = Math.round(seconds / 60);
    if (m < 60) return m + "m";
    const h = Math.floor(m / 60);
    if (h < 100) return h + "h " + (m % 60) + "m";
    return h.toLocaleString() + "h";
  }
  const num = (n) => Math.round(n).toLocaleString();
  const dec = (n) => (n >= 100 ? num(n) : n.toLocaleString(undefined, { maximumFractionDigits: 1, minimumFractionDigits: n > 0 && n < 10 ? 1 : 0 }));
  const pct = (n, d) => (d > 0 ? Math.round((n / d) * 100) + "%" : "—");
  const dateFmt = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" });
  const shortDate = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" });
  const monthFmt = new Intl.DateTimeFormat(undefined, { month: "short", year: "2-digit" });
  const dateTimeFmt = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  // Local time without Date objects: the zone offset is looked up once per
  // UTC hour and the rest is arithmetic. Reports over hundreds of thousands
  // of plays would otherwise spend most of their time creating Dates.
  const HOUR = 3600000;
  const offsets = new Map();
  function offsetAt(t) {
    const k = Math.floor(t / HOUR);
    let o = offsets.get(k);
    if (o === undefined) { o = -new Date(k * HOUR).getTimezoneOffset() * 60000; offsets.set(k, o); }
    return o;
  }
  const localMs = (t) => t + offsetAt(t);
  // startOfDay is local midnight, correct across daylight-saving changes.
  function startOfDay(t) {
    const o = offsetAt(t), mid = Math.floor((t + o) / DAY) * DAY;
    const o2 = offsetAt(mid - o);
    return mid - (o2 === o ? o : o2);
  }
  // Monday = 0 (1 Jan 1970 was a Thursday).
  const weekdayOfLocal = (local) => (Math.floor(local / DAY) + 3) % 7;
  const isoDay = (ms) => { const d = new Date(ms); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`; };
  const playTitle = (p) => p.series ? `${p.series} · S${String(p.season).padStart(2, "0")}E${String(p.episode).padStart(2, "0")} · ${p.title}` : p.title;
  const who = (p) => (p.profile && p.profile.toLowerCase() !== p.user.toLowerCase() ? `${p.user} · ${p.profile}` : p.user);

  // ---------- colors ----------
  // Colors are picked under Customize and saved by the plugin; the page gets
  // the saved set inline. Defaults match app.css.
  const COLOR_ROLES = [
    ["Stream types", [["direct_play", "Direct Play", "#12a594"], ["direct_stream", "Remux", "#3d63dd"], ["transcode", "Transcode", "#e5484d"]]],
    ["Media types", [["movies", "Movies", "#3d63dd"], ["tv", "TV shows", "#8e4ec6"], ["audiobooks", "Audiobooks", "#f5a524"]]],
  ];
  const ROLE_VAR = { direct_play: "--c-direct", direct_stream: "--c-remux", transcode: "--c-transcode", movies: "--c-movies", tv: "--c-tv", audiobooks: "--c-audiobooks" };
  const ROLE_DEFAULT = Object.fromEntries(COLOR_ROLES.flatMap(([, roles]) => roles.map(([k, , d]) => [k, d])));
  const DEFAULT_PALETTE = ["#3d63dd", "#12a594", "#e5484d", "#f5a524", "#8e4ec6", "#0091ff", "#d6409f", "#46a758", "#e54666", "#00a2c7", "#978365", "#6e56cf"];
  const MAX_PALETTE = 16;
  const HEX = /^#[0-9a-f]{6}$/i;

  function readSavedColors() {
    try {
      const raw = JSON.parse(($("saved-colors") || {}).textContent || "{}");
      return { colors: raw.colors || {}, palette: Array.isArray(raw.palette) ? raw.palette : [], show_audiobooks: raw.show_audiobooks === true };
    } catch (_) {
      return { colors: {}, palette: [], show_audiobooks: false };
    }
  }
  // fullColors fills a saved set with defaults so every role has a value.
  function fullColors(c) {
    const colors = {};
    for (const k of Object.keys(ROLE_DEFAULT)) colors[k] = HEX.test(c.colors[k] || "") ? c.colors[k].toLowerCase() : ROLE_DEFAULT[k];
    const palette = (c.palette || []).filter((x) => HEX.test(x)).slice(0, MAX_PALETTE);
    return { colors, palette: palette.length ? palette : DEFAULT_PALETTE.slice(), show_audiobooks: c.show_audiobooks === true };
  }
  function applyColors(c) {
    const root = document.documentElement.style;
    for (const [k, v] of Object.entries(c.colors)) root.setProperty(ROLE_VAR[k], v);
    for (let i = 0; i < MAX_PALETTE; i++) root.removeProperty(`--p${i}`);
    c.palette.forEach((v, i) => root.setProperty(`--p${i}`, v));
    state.palette = c.palette.length;
  }
  state.colors = fullColors(readSavedColors());
  applyColors(state.colors);
  const seriesColor = (i) => `var(--p${i % (state.palette || DEFAULT_PALETTE.length)})`;
  const streamColor = (s) => `var(--c-${s === "direct" ? "direct" : s === "unknown" ? "other" : s})`;
  const catColor = (c) => (c === "other" ? "var(--c-other)" : `var(--c-${c})`);

  // ---------- loading ----------
  // fetchJSON reads a /data chunk. Chunks arrive gzipped (Silo doesn't
  // compress plugin responses) and are unzipped here; errors are plain JSON.
  async function fetchJSON(url) {
    const res = await fetch(url, { credentials: "same-origin", headers: { Accept: "application/gzip, application/json" } });
    let body = null;
    if (res.ok && (res.headers.get("Content-Type") || "").startsWith("application/gzip")) {
      if (typeof DecompressionStream === "undefined") {
        throw new Error("This browser is too old for Playback Reporting. Use a current version of Chrome, Edge, Firefox or Safari.");
      }
      return new Response(res.body.pipeThrough(new DecompressionStream("gzip"))).json();
    }
    try { body = await res.json(); } catch (_) { /* not JSON */ }
    if (!res.ok) {
      if (res.status === 401 || res.status === 403) throw new Error("Your access to this page expired. Open Playback Reporting again from the admin menu.");
      throw new Error((body && body.error) || `The plugin answered ${res.status}.`);
    }
    return body;
  }

  function category(type, series) {
    if (type === "movie") return "movies";
    if (type === "episode" || series) return "tv";
    if (type === "audiobook") return "audiobooks";
    return "other";
  }

  function decode(chunk) {
    const col = Object.fromEntries(chunk.columns.map((c, i) => [c, i]));
    const d = chunk.dict;
    return chunk.plays.map((r) => {
      const user = d.users[r[col.user]] || ["", ""];
      const title = d.titles[r[col.title]] || ["", ""];
      const method = d.methods[r[col.method]] || "";
      const live = r[col.live_method] >= 0 ? d.methods[r[col.live_method]] : "";
      const series = d.series[r[col.series]] || "";
      const type = d.types[r[col.type]] || "";
      const watched = r[col.watched], duration = r[col.duration], completed = (r[col.flags] & 1) !== 0;
      const profile = d.profiles[r[col.profile]] || "";
      const ended = r[col.ended] * 1000;
      return {
        ended, day: startOfDay(ended),
        started: r[col.started] * 1000,
        userId: user[0], user: user[1], profile,
        profileKey: user[0] + "\u0000" + profile,
        itemId: title[0], title: title[1], series,
        season: r[col.season], episode: r[col.episode],
        type, cat: category(type, series),
        stream: STREAM_OF[live] || STREAM_OF[method] || "unknown",
        watched, duration, completed,
        // Smart plays: how many times the title was watched, from watch time.
        smart: duration > 0 ? watched / duration : completed ? 1 : 0,
        estimated: (r[col.flags] & 2) !== 0,
        client: r[col.client] >= 0 ? d.clients[r[col.client]] : "",
        location: r[col.location] >= 0 ? d.locations[r[col.location]] : "",
      };
    });
  }

  async function load() {
    const first = await fetchJSON("data?chunk=0");
    const rest = [];
    for (let i = 1; i < first.chunks; i++) rest.push(i);
    const chunks = [first];
    // Fetch the rest a few at a time, all from the same snapshot.
    while (rest.length) {
      const batch = rest.splice(0, 3).map((i) => fetchJSON(`data?snapshot=${encodeURIComponent(first.snapshot)}&chunk=${i}`));
      chunks.push(...(await Promise.all(batch)));
    }
    state.allPlays = chunks.flatMap(decode).sort((a, b) => b.ended - a.ended);
    setAudiobooks(state.colors.show_audiobooks);
    state.features = first.features || { clients: false };
    state.status = first.status;
  }

  function showStatus() {
    const st = state.status || {}, f = state.features;
    if (st.last_error) {
      statusEl.textContent = st.last_error;
      statusEl.classList.add("error");
      return;
    }
    const parts = [];
    if (!st.ready) parts.push("Still reading Silo's history — reload in a moment for the full picture.");
    parts.push(`${num(state.plays.length)} plays`);
    if (f.hidden) {
      const why = [f.min_seconds ? `shorter than ${num(f.min_seconds)}s` : "", f.ignore_rules ? "on the ignore list" : ""].filter(Boolean).join(" or ");
      parts.push(`${num(f.hidden)} hidden (${why})`);
    }
    if (st.last_sync) parts.push("synced " + dateTimeFmt.format(new Date(st.last_sync)));
    if (f.clients && f.clients_error) parts.push("client tracking: " + f.clients_error);
    statusEl.textContent = parts.join(" · ");
  }

  // ---------- controls ----------
  // Every tab keeps its own settings, remembered per browser.
  function setting(tab, key, fallback) {
    const k = `${tab}.${key}`;
    if (!(k in settings)) settings[k] = remember(k) ?? fallback;
    return settings[k];
  }
  const settings = {};
  function setSetting(tab, key, value) { settings[`${tab}.${key}`] = value; remember(`${tab}.${key}`, value); }

  function selectControl(label, tab, key, options, fallback, onchange) {
    const s = el("select", {}, options.map(([v, t]) => el("option", { value: v, text: t })));
    const cur = setting(tab, key, fallback);
    if (!options.some(([v]) => v === cur)) setSetting(tab, key, fallback);
    s.value = setting(tab, key, fallback);
    s.addEventListener("change", () => { setSetting(tab, key, s.value); onchange(); });
    return el("label", { class: "field" }, el("span", { text: label }), s);
  }

  function segmented(label, tab, key, options, fallback, onchange) {
    let cur = setting(tab, key, fallback);
    // A saved choice that is no longer offered falls back to the default.
    if (!options.some(([v]) => v === cur)) { cur = fallback; setSetting(tab, key, fallback); }
    const group = el("div", { class: "seg", role: "radiogroup", "aria-label": label });
    const buttons = options.map(([v, t]) => el("button", { type: "button", role: "radio", "aria-checked": String(v === cur), "data-v": v, text: t }));
    group.append(...buttons);
    group.addEventListener("click", (e) => {
      const b = e.target.closest("button[data-v]");
      if (!b) return;
      for (const x of buttons) x.setAttribute("aria-checked", String(x === b));
      setSetting(tab, key, b.dataset.v);
      onchange();
    });
    group.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      const i = buttons.findIndex((b) => b.getAttribute("aria-checked") === "true");
      const next = buttons[(i + (e.key === "ArrowRight" ? 1 : buttons.length - 1)) % buttons.length];
      next.focus(); next.click();
    });
    return el("div", { class: "field" }, el("span", { text: label }), group);
  }

  function checkbox(label, tab, key, onchange) {
    const box = el("input", { type: "checkbox" });
    box.checked = setting(tab, key, "0") === "1";
    box.addEventListener("change", () => { setSetting(tab, key, box.checked ? "1" : "0"); onchange(); });
    return el("label", { class: "check" }, box, el("span", { text: label }));
  }

  const RANGES = [["7", "Last 7 days"], ["30", "Last 30 days"], ["90", "Last 90 days"], ["365", "Last 12 months"], ["year", "This year"], ["all", "All time"], ["custom", "Custom…"]];

  // rangeControl is the date selector: presets, or a custom from/to.
  function rangeControl(tab, fallback, onchange) {
    const sel = el("select", {}, RANGES.map(([v, t]) => el("option", { value: v, text: t })));
    sel.value = setting(tab, "range", fallback);
    if (!sel.value) sel.value = fallback;
    const from = el("input", { type: "date", "aria-label": "From" }), to = el("input", { type: "date", "aria-label": "To" });
    from.value = setting(tab, "from", ""); to.value = setting(tab, "to", "");
    const custom = el("span", { class: "custom" }, from, el("span", { text: "–" }), to);
    const sync = () => { custom.hidden = sel.value !== "custom"; };
    sel.addEventListener("change", () => { setSetting(tab, "range", sel.value); sync(); onchange(); });
    from.addEventListener("change", () => { setSetting(tab, "from", from.value); onchange(); });
    to.addEventListener("change", () => { setSetting(tab, "to", to.value); onchange(); });
    sync();
    return el("div", { class: "field" }, el("span", { text: "Dates" }), el("div", { class: "range" }, sel, custom));
  }

  // bounds returns [from, to) in ms for a tab's range. "All time" is
  // [0, Infinity); charts then start at the first play they show.
  function bounds(tab, fallback) {
    const r = setting(tab, "range", fallback);
    const today = startOfDay(Date.now());
    if (r === "all") return [0, Infinity];
    if (r === "year") return [new Date(new Date().getFullYear(), 0, 1).getTime(), Infinity];
    if (r === "custom") {
      const f = setting(tab, "from", ""), t = setting(tab, "to", "");
      return [f ? new Date(f + "T00:00").getTime() : 0, t ? new Date(t + "T00:00").getTime() + DAY : Infinity];
    }
    const n = Number(r) || 30;
    return [today - (n - 1) * DAY, Infinity];
  }
  const inRange = (plays, [from, to]) => plays.filter((p) => p.ended >= from && p.ended < to);

  function toolbar(...items) { return el("div", { class: "toolbar" }, items); }

  // ---------- aggregation ----------
  const MEASURES = { plays: () => 1, time: (p) => p.watched, smart: (p) => p.smart };
  function groupBy(plays, key, measure) {
    const m = new Map(), f = MEASURES[measure || "time"];
    for (const p of plays) {
      const k = key(p);
      let g = m.get(k);
      if (!g) { g = { key: k, plays: 0, time: 0, smart: 0, value: 0, items: [] }; m.set(k, g); }
      g.plays += 1; g.time += p.watched; g.smart += p.smart; g.value += f(p); g.items.push(p);
    }
    return [...m.values()].sort((a, b) => b.value - a.value);
  }
  const sum = (plays, f) => plays.reduce((s, p) => s + f(p), 0);
  const fmtBy = (measure) => (measure === "plays" ? num : measure === "smart" ? dec : hours);

  // ---------- charts ----------
  // One floating tooltip for every chart. It follows the pointer, stays
  // inside the window, and on touch appears where the finger lands.
  const tipEl = el("div", { class: "tip", role: "tooltip", hidden: true });
  document.body.append(tipEl);
  let tipOwner = null;
  function placeTip(x, y) {
    const pad = 12, w = tipEl.offsetWidth, h = tipEl.offsetHeight;
    let left = x + pad, top = y + pad;
    if (left + w > window.innerWidth - 8) left = x - w - pad;
    if (top + h > window.innerHeight - 8) top = y - h - pad;
    tipEl.style.left = Math.max(8, left) + "px";
    tipEl.style.top = Math.max(8, top) + "px";
  }
  function showTip(owner, content, e) {
    if (tipOwner !== owner) { tipEl.replaceChildren(...[].concat(content())); tipOwner = owner; }
    tipEl.hidden = false;
    placeTip(e.clientX, e.clientY);
  }
  function hideTip(owner) { if (!owner || tipOwner === owner) { tipEl.hidden = true; tipOwner = null; } }
  // hover wires a node to the tooltip plus optional enter/leave effects.
  function hover(node, content, on, off) {
    node.addEventListener("pointerenter", (e) => { if (on) on(); showTip(node, content, e); });
    node.addEventListener("pointermove", (e) => showTip(node, content, e));
    node.addEventListener("pointerleave", () => { if (off) off(); hideTip(node); });
    node.addEventListener("pointerdown", (e) => { if (e.pointerType !== "mouse") { if (on) on(); showTip(node, content, e); } });
  }
  window.addEventListener("scroll", () => hideTip(), { passive: true });
  document.addEventListener("pointerdown", (e) => { if (tipOwner && !tipOwner.contains(e.target)) hideTip(); });

  function tipRows(title, rows, foot) {
    return [el("div", { class: "tip-title", text: title }),
      el("table", { class: "tip-rows" }, rows.map((r) => el("tr", { class: r.strong ? "strong" : null },
        el("td", {}, r.color ? el("span", { class: "sw", style: `--c:${r.color}` }) : null, r.name),
        el("td", { class: "v", text: r.value }), r.extra !== undefined ? el("td", { class: "x", text: r.extra }) : null))),
      foot ? el("div", { class: "tip-foot", text: foot }) : null].filter(Boolean);
  }

  // legend items highlight their series in the chart they belong to.
  function legend(series, focus) {
    return el("ul", { class: "legend" }, series.map((s, i) => {
      const li = el("li", { style: `--c:${s.color}`, tabindex: focus ? "0" : null }, el("span", { class: "sw" }), s.name);
      if (focus) {
        li.addEventListener("pointerenter", () => focus(i));
        li.addEventListener("pointerleave", () => focus(-1));
        li.addEventListener("focus", () => focus(i));
        li.addEventListener("blur", () => focus(-1));
      }
      return li;
    }));
  }

  // buckets picks days, weeks or months so a chart keeps a readable number
  // of columns, and returns each column's start.
  function buckets(from, to) {
    const span = (to - from) / DAY;
    const unit = span <= 120 ? "day" : span <= 730 ? "week" : "month";
    const start = (ms) => {
      const d = new Date(startOfDay(ms));
      if (unit === "week") d.setDate(d.getDate() - ((d.getDay() + 6) % 7));
      if (unit === "month") d.setDate(1);
      return d.getTime();
    };
    const list = [];
    const d = new Date(start(from));
    while (d.getTime() < to && list.length < 800) {
      list.push(d.getTime());
      if (unit === "day") d.setDate(d.getDate() + 1);
      else if (unit === "week") d.setDate(d.getDate() + 7);
      else d.setMonth(d.getMonth() + 1);
    }
    // column maps a play's local day to its column, or -1 outside the
    // chart. Days and weeks are arithmetic (rounding absorbs daylight-saving
    // hours); months are looked up once per day.
    const first = list[0], memo = new Map();
    const column = (day) => {
      if (day < first) return -1;
      let i;
      if (unit === "day") i = Math.round((day - first) / DAY);
      else if (unit === "week") i = Math.floor(Math.round((day - first) / DAY) / 7);
      else {
        i = memo.get(day);
        if (i === undefined) { i = list.indexOf(start(day)); memo.set(day, i); }
      }
      return i < list.length ? i : -1;
    };
    return { unit, list, start, column };
  }

  function columnLabel(unit, ms) { return unit === "month" ? monthFmt.format(ms) : shortDate.format(ms); }
  function bucketTitle(unit, ms) {
    if (unit === "month") return new Intl.DateTimeFormat(undefined, { month: "long", year: "numeric" }).format(ms);
    if (unit === "week") return `Week of ${dateFmt.format(ms)}`;
    return new Intl.DateTimeFormat(undefined, { weekday: "short", year: "numeric", month: "short", day: "numeric" }).format(ms);
  }

  // stackedColumns draws one column per bucket with a stack per series.
  // Hovering a column dims the others and lists its series; hovering a
  // legend item dims the other series. Returns [chart, legend].
  // The column chart is drawn at its real pixel width (and redrawn when that
  // changes) so text and bars stay their designed size on any screen,
  // instead of stretching a fixed viewBox.
  let lastChartWidth = 760;
  function stackedColumns(b, series, fmt, what) {
    const totals = b.list.map((_, i) => series.reduce((s, x) => s + x.values[i], 0));
    // Gridlines land on round values: whole minutes/hours for watch time,
    // 1-2-2.5-5 steps for counts.
    const rawMax = Math.max(1e-9, ...totals);
    const tickStep = (() => {
      const target = rawMax / 4;
      if (fmt === hours) {
        const steps = [60, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];
        return steps.find((x) => x >= target) || Math.ceil(target / 86400) * 86400;
      }
      const mag = Math.pow(10, Math.floor(Math.log10(target)));
      const mults = fmt === num && mag < 10 ? [1, 2, 5, 10] : [1, 2, 2.5, 5, 10];
      const step = mults.map((m) => m * mag).find((x) => x >= target);
      return fmt === num ? Math.max(1, step) : step;
    })();
    const ticks = Math.max(1, Math.ceil(rawMax / tickStep - 1e-9));
    const maxV = ticks * tickStep;
    const wrap = el("div", { class: "chart" });
    let root = null, focused = -1, drawnW = 0;
    const draw = (W) => {
      W = Math.max(280, Math.round(W));
      if (W === drawnW) return;
      drawnW = W;
      // Left margin fits the widest axis label (12px text ≈ 7px per character).
      const longest = Math.max(...Array.from({ length: ticks + 1 }, (_, i) => fmt(tickStep * i).length));
      const H = Math.round(Math.min(380, Math.max(220, W * 0.26))), L = Math.max(40, longest * 7 + 14), B = 26, T = 10;
      const step = (W - L) / b.list.length;
      const bw = Math.max(1, Math.min(56, step * 0.72));
      const y = (v) => H - B - (v / maxV) * (H - B - T);
      const svgEl = svg("svg", { viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: "img", "aria-label": `${what} per ${b.unit}` });
      const grid = svg("g", { class: "grid" });
      for (let i = 0; i <= ticks; i++) {
        const v = tickStep * i, yy = y(v);
        grid.append(svg("line", { x1: L, x2: W, y1: yy, y2: yy }), svg("text", { x: L - 8, y: yy + 4, "text-anchor": "end" }, fmt(v)));
      }
      svgEl.append(grid);
      const band = svg("rect", { class: "band", x: 0, y: T, width: step, height: H - B - T, visibility: "hidden" });
      svgEl.append(band);
      // Roughly one date label per 80px keeps them from colliding.
      const labelEvery = Math.max(1, Math.ceil(b.list.length / Math.max(2, Math.floor((W - L) / 80))));
      b.list.forEach((t, i) => {
        let base = 0;
        const x = L + i * step + (step - bw) / 2;
        const col = svg("g", { class: "col" });
        series.forEach((s, si) => {
          const v = s.values[i];
          if (v > 0) {
            const top = y(base + v), h = Math.max(0.5, y(base) - top);
            col.append(svg("rect", { x, y: top, width: bw, height: h, fill: s.color, "data-s": si, rx: Math.min(3, bw / 4) }));
          }
          base += v;
        });
        // A full-height hit area so short columns are easy to hover.
        const hit = svg("rect", { class: "hit", x: L + i * step, y: T, width: step, height: H - B - T, fill: "transparent" });
        col.append(hit);
        hover(hit, () => {
          const rows = series.map((s) => ({ name: s.name, v: s.values[i], color: s.color })).filter((r) => r.v > 0).sort((a, c) => c.v - a.v);
          return tipRows(bucketTitle(b.unit, t), [
            { name: "Total", value: fmt(totals[i]), strong: true },
            ...rows.slice(0, 12).map((r) => ({ name: r.name, value: fmt(r.v), color: r.color, extra: pct(r.v, totals[i]) })),
          ], rows.length > 12 ? `+${rows.length - 12} more` : rows.length ? null : "Nothing played");
        }, () => {
          svgEl.classList.add("hovering"); col.classList.add("active");
          band.setAttribute("x", L + i * step); band.setAttribute("visibility", "visible");
        }, () => {
          svgEl.classList.remove("hovering"); col.classList.remove("active"); band.setAttribute("visibility", "hidden");
        });
        svgEl.append(col);
        if (i % labelEvery === 0) svgEl.append(svg("text", { x: x + bw / 2, y: H - 7, "text-anchor": "middle" }, columnLabel(b.unit, t)));
      });
      if (root) hideTip();
      root = svgEl;
      wrap.replaceChildren(root);
      applyFocus();
    };
    const applyFocus = () => {
      if (!root) return;
      root.classList.toggle("focusing", focused >= 0);
      for (const r of root.querySelectorAll("rect[data-s]")) r.classList.toggle("on", Number(r.dataset.s) === focused);
    };
    const focus = (si) => { focused = si; applyFocus(); };
    draw(lastChartWidth);
    if (typeof ResizeObserver === "function") {
      let raf = 0;
      const ro = new ResizeObserver((entries) => {
        if (!wrap.isConnected) { ro.disconnect(); return; }
        const w = entries[0].contentRect.width;
        if (!w) return;
        lastChartWidth = w;
        cancelAnimationFrame(raf);
        raf = requestAnimationFrame(() => draw(w));
      });
      ro.observe(wrap);
    }
    return [wrap, legend(series, focus)];
  }

  // donut shows shares of a whole; slices are [{name, value, color, sub}].
  // Slices and their legend rows highlight each other on hover.
  function donut(slices, center, centerSub, fmt, opts) {
    const o = opts || {};
    const total = slices.reduce((s, x) => s + x.value, 0);
    const size = o.size || 160, C = size / 2, R = C - 6, r = R * (o.pie ? 0 : 0.64);
    const root = svg("svg", { viewBox: `0 0 ${size} ${size}`, class: "donut" + (o.pie ? " pie" : ""), style: `width:${size}px;height:${size}px`,
      role: "img", "aria-label": slices.map((s) => `${s.name} ${pct(s.value, total)}`).join(", ") });
    const nodes = [];
    const keysEl = el("ul", { class: "donut-keys" + (slices.length > 8 ? " long" : "") });
    const focus = (i) => {
      root.classList.toggle("focusing", i >= 0);
      nodes.forEach((n, j) => n && n.classList.toggle("on", j === i));
      [...keysEl.children].forEach((li, j) => li.classList.toggle("on", j === i));
    };
    const tipFor = (s) => () => tipRows(s.name, [{ name: "Share", value: pct(s.value, total), strong: true }], s.sub || fmt(s.value));
    if (total <= 0) {
      root.append(svg("circle", { cx: C, cy: C, r: (R + r) / 2, fill: "none", stroke: "var(--line)", "stroke-width": R - r }));
    } else {
      let a0 = -Math.PI / 2;
      const shown = slices.filter((x) => x.value > 0).length;
      slices.forEach((s, i) => {
        if (s.value <= 0) { nodes.push(null); return; }
        const frac = s.value / total;
        const a1 = a0 + frac * Math.PI * 2;
        let node;
        if (shown === 1) {
          node = o.pie ? svg("circle", { cx: C, cy: C, r: R, fill: s.color })
            : svg("circle", { cx: C, cy: C, r: (R + r) / 2, fill: "none", stroke: s.color, "stroke-width": R - r });
        } else {
          const gap = Math.min(0.012, frac * 0.2);
          const p = (rad, a) => `${(C + rad * Math.cos(a)).toFixed(2)} ${(C + rad * Math.sin(a)).toFixed(2)}`;
          const b0 = a0 + gap, b1 = a1 - gap, large = b1 - b0 > Math.PI ? 1 : 0;
          node = svg("path", { fill: s.color, d: r > 0
            ? `M ${p(R, b0)} A ${R} ${R} 0 ${large} 1 ${p(R, b1)} L ${p(r, b1)} A ${r} ${r} 0 ${large} 0 ${p(r, b0)} Z`
            : `M ${C} ${C} L ${p(R, b0)} A ${R} ${R} 0 ${large} 1 ${p(R, b1)} Z` });
          // Hover pushes the slice out a little along its middle angle.
          const mid = (a0 + a1) / 2;
          node.style.setProperty("--dx", (Math.cos(mid) * 4).toFixed(2) + "px");
          node.style.setProperty("--dy", (Math.sin(mid) * 4).toFixed(2) + "px");
        }
        node.classList.add("slice");
        hover(node, tipFor(s), () => focus(i), () => focus(-1));
        nodes.push(node);
        root.append(node);
        a0 = a1;
      });
    }
    if (!o.pie) {
      root.append(svg("text", { x: C, y: C - 2, "text-anchor": "middle", class: "donut-value" }, center),
        svg("text", { x: C, y: C + 16, "text-anchor": "middle", class: "donut-sub" }, centerSub));
    }
    slices.forEach((s, i) => {
      const li = el("li", { style: `--c:${s.color}`, tabindex: "0" },
        el("span", { class: "sw" }), el("span", { class: "k-name", title: s.name, text: s.name }),
        el("span", { class: "k-pct", text: pct(s.value, total) }), el("span", { class: "k-sub", text: s.sub || fmt(s.value) }));
      li.addEventListener("pointerenter", () => focus(i));
      li.addEventListener("pointerleave", () => focus(-1));
      li.addEventListener("focus", () => focus(i));
      li.addEventListener("blur", () => focus(-1));
      keysEl.append(li);
    });
    return el("div", { class: "donut-wrap" }, root, keysEl);
  }

  // barList is a ranked list with proportional bars.
  // barList is a ranked list with proportional bars. pick, when given,
  // makes each row open something (with its hint as the tooltip).
  function barList(groups, label, valueText, subText, color, limit, pick) {
    if (!groups.length) return empty();
    const top = Math.max(1e-9, groups[0].value);
    const list = el("ol", { class: "bars" });
    const row = (g) => {
      if (!pick) return { class: "bar" };
      const go = () => pick.open(g);
      return { class: "bar pickable", tabindex: "0", role: "link", title: pick.hint(g), onclick: go,
        onkeydown: (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); go(); } } };
    };
    const draw = (n) => list.replaceChildren(...groups.slice(0, n).map((g, i) => el("li", row(g),
      el("span", { class: "rank", text: String(i + 1) }),
      el("span", { class: "name", title: label(g), text: label(g) }),
      el("span", { class: "num", text: valueText(g) }),
      subText ? el("span", { class: "sub", text: subText(g) }) : null,
      el("div", { class: "track" }, el("div", { class: "fill", style: `width:${(g.value / top) * 100}%${color ? `;background:${color(g)}` : ""}` })))));
    const n = limit || groups.length;
    draw(n);
    if (groups.length <= n) return list;
    const more = el("button", { type: "button", class: "small link", text: `Show all ${num(groups.length)}` });
    more.addEventListener("click", () => { draw(groups.length); more.remove(); });
    return el("div", {}, list, more);
  }

  function tile(label, value, sub, color) {
    return el("div", { class: "tile", style: color ? `--c:${color}` : null }, el("div", { class: "label", text: label }),
      el("div", { class: "value", text: value }), sub ? el("div", { class: "sub", text: sub }) : null);
  }

  function panel(title, ...body) {
    const head = Array.isArray(title) ? el("div", { class: "panel-head" }, el("h2", { text: title[0] }), title.slice(1)) : el("h2", { text: title });
    return el("section", { class: "panel" }, head, body);
  }

  const MEASURE_OPTIONS = [["time", "Time"], ["smart", "Smart plays"], ["plays", "Plays"]];
  // Audiobooks only appear where there has been audiobook activity.
  // Audiobooks are part of the reports only when turned on under Customize;
  // otherwise their plays are left out everywhere.
  let showAudiobooks = false;
  function setAudiobooks(on) {
    showAudiobooks = on;
    const all = state.allPlays || [];
    state.plays = on ? all : all.filter((p) => p.cat !== "audiobooks");
    if (state.status) showStatus();
  }
  const catFilter = () => [["all", "All"], ["movies", "Movies"], ["tv", "TV shows"], ...(showAudiobooks ? [["audiobooks", "Audiobooks"]] : [])];
  const byCat = (plays, c) => (c === "all" ? plays : plays.filter((p) => p.cat === c));

  // ---------- Activity tab ----------
  function renderActivity(view) {
    const T = "activity", D = "30";
    const content = el("div", { class: "stack" });
    const draw = () => {
      const measure = setting(T, "measure", "time");
      const fmt = fmtBy(measure);
      const split = setting(T, "profiles", "0") === "1";
      const stack = setting(T, "stack", "user");
      const range = bounds(T, D);
      const plays = byCat(inRange(state.plays, range), setting(T, "cat", "all"));
      if (!plays.length) { content.replaceChildren(panel("Activity", empty())); return; }
      const from = range[0] > 0 ? range[0] : startOfDay(plays[plays.length - 1].ended);
      const to = Math.min(range[1], startOfDay(Date.now()) + DAY);
      const b = buckets(from, to);
      const key = stack === "user" ? (split ? (p) => p.profileKey : (p) => p.userId) : stack === "stream" ? (p) => p.stream : (p) => p.cat;
      const name = stack === "user" ? (g) => (split ? who(g.items[0]) : g.items[0].user) : stack === "stream" ? (g) => STREAM_LABEL[g.key] : (g) => CAT_LABEL[g.key];
      const groups = groupBy(plays, key, measure);
      const orderFixed = stack === "stream" ? STREAMS.map((s) => s[0]) : stack === "cat" ? CATS.map((c) => c[0]) : null;
      if (orderFixed) groups.sort((a, b2) => orderFixed.indexOf(a.key) - orderFixed.indexOf(b2.key));
      const shown = stack === "user" ? groups.slice(0, MAX_SERIES) : groups;
      const series = shown.map((g, i) => ({
        name: name(g), items: g.items, values: new Array(b.list.length).fill(0),
        color: stack === "user" ? seriesColor(i) : stack === "stream" ? streamColor(g.key) : catColor(g.key),
      }));
      const rest = groups.slice(shown.length);
      if (rest.length) series.push({ name: `${rest.length} more`, items: rest.flatMap((g) => g.items), values: new Array(b.list.length).fill(0), color: "var(--c-other)" });
      const f = MEASURES[measure];
      for (const s of series) {
        const values = s.values;
        for (const p of s.items) { const i = b.column(p.day); if (i >= 0) values[i] += f(p); }
      }
      const what = { time: "Watch time", plays: "Plays", smart: "Smart plays" }[measure];
      const people = stack === "user" ? groups : groupBy(plays, split ? (p) => p.profileKey : (p) => p.userId, measure);
      const colorOf = new Map(shown.map((g, i) => [g.key, seriesColor(i)]));
      content.replaceChildren(
        el("div", { class: "tiles" },
          tile("Watch time", hours(sum(plays, (p) => p.watched)), plays.some((p) => p.estimated) ? "≈ includes estimated older plays" : null),
          tile("Smart plays", dec(sum(plays, (p) => p.smart)), "watch time ÷ length"),
          tile("Plays", num(plays.length)),
          tile(split ? "Active profiles" : "Active users", num(people.length)),
          tile("Transcoded", pct(plays.filter((p) => p.stream === "transcode").length, plays.length), "of plays")),
        panel(`${what} per ${b.unit}`, ...stackedColumns(b, series, fmt, what)),
        el("div", { class: "grid2" },
          panel(split ? "By profile" : "By user", barList(people, (g) => (split ? who(g.items[0]) : g.items[0].user), (g) => fmt(g.value),
            (g) => `${num(g.plays)} plays · ${hours(g.time)}`, stack === "user" ? (g) => colorOf.get(g.key) || "var(--c-other)" : null, 15,
            { hint: (g) => `Open ${split ? who(g.items[0]) : g.items[0].user} in Users`, open: (g) => openUser(g.items[0], split) })),
          panel(`${what} by weekday and hour`, heatmap(plays, measure), el("p", { class: "note", text: "Your local time. Watch time is spread over the hours a play ran." }))));
    };
    view.append(toolbar(
      rangeControl(T, D, draw),
      segmented("Show", T, "cat", catFilter(), "all", draw),
      segmented("Measure", T, "measure", MEASURE_OPTIONS, "time", draw),
      selectControl("Stack by", T, "stack", [["user", "User"], ["cat", "Media type"], ["stream", "Stream type"]], "user", draw),
      checkbox("Separate profiles", T, "profiles", draw)), content);
    draw();
  }

  function heatmap(plays, measure) {
    const grid = Array.from({ length: 7 }, () => new Array(24).fill(0));
    const f = MEASURES[measure];
    for (const p of plays) {
      const start = p.started || p.ended - p.watched * 1000;
      if (measure !== "time") {
        const l = localMs(start);
        grid[weekdayOfLocal(l)][Math.floor((l % DAY) / HOUR)] += f(p);
        continue;
      }
      // Spread watch time over the hours the play ran.
      let left = p.watched * 1000, l = localMs(Math.max(start, p.ended - left));
      while (left > 0) {
        const dayIdx = Math.floor(l / DAY), within = l - dayIdx * DAY, h = Math.floor(within / HOUR);
        const span = Math.min(left, (h + 1) * HOUR - within);
        grid[(dayIdx + 3) % 7][h] += span / 1000;
        left -= span; l += span;
      }
    }
    const maxV = Math.max(1e-9, ...grid.flat());
    const fmt = fmtBy(measure);
    return el("div", { class: "heat", role: "img", "aria-label": "Activity by weekday and hour" },
      el("span"), Array.from({ length: 24 }, (_, h) => el("span", { class: "hour", text: h % 3 === 0 ? String(h) : "" })),
      grid.map((row, d) => [el("span", { class: "wd", text: WEEKDAYS[d] }), row.map((v, h) => {
        const cell = el("span", { class: "cell", style: v > 0 ? `--a:${(0.15 + 0.85 * (v / maxV)).toFixed(3)}` : null });
        hover(cell, () => tipRows(`${FULL_DAYS[d]}, ${String(h).padStart(2, "0")}:00–${String((h + 1) % 24).padStart(2, "0")}:00`,
          [{ name: { time: "Watch time", plays: "Plays", smart: "Smart plays" }[measure], value: fmt(v), strong: true }]));
        return cell;
      })]));
  }

  // openUser shows one user (or one profile) on the Users tab over the same
  // dates the Activity tab is showing.
  function openUser(p, withProfile) {
    setSetting("users", "user", p.userId);
    setSetting("users", "profile", withProfile ? p.profile : "");
    for (const k of ["range", "from", "to"]) setSetting("users", k, setting("activity", k, k === "range" ? "30" : ""));
    if (state.tableFilters) delete state.tableFilters.users;
    state.page.users = 0;
    state.tab = "users"; remember("tab", "users");
    render();
    window.scrollTo(0, 0);
    const tab = $("tab-users");
    if (tab) tab.focus({ preventScroll: true });
  }

  // ---------- Users tab ----------
  function renderUsers(view) {
    const T = "users", D = "all";
    const users = groupBy(state.plays, (p) => p.userId, "time");
    const userSel = el("select", {}, users.map((g) => el("option", { value: g.key, text: g.items[0].user })).sort((a, b) => a.textContent.localeCompare(b.textContent)));
    const saved = setting(T, "user", "");
    userSel.value = users.some((g) => g.key === saved) ? saved : users[0] ? users[0].key : "";
    const profileSel = el("select", {});
    const fillProfiles = () => {
      const profiles = [...new Set(state.plays.filter((p) => p.userId === userSel.value).map((p) => p.profile))].sort();
      profileSel.replaceChildren(el("option", { value: "", text: "All profiles" }), ...profiles.map((p) => el("option", { value: p, text: p || "(no profile)" })));
      const cur = setting(T, "profile", "");
      profileSel.value = profiles.includes(cur) ? cur : "";
      profileSel.disabled = profiles.length < 2;
    };
    const content = el("div", { class: "stack" });
    const draw = () => {
      const all = state.plays.filter((p) => p.userId === userSel.value && (!profileSel.value || p.profile === profileSel.value));
      const range = bounds(T, D);
      const plays = inRange(all, range);
      if (!users.length) { content.replaceChildren(panel("Users", empty("No plays yet."))); return; }
      const name = profileSel.value ? `${all[0] ? all[0].user : ""} · ${profileSel.value}` : (all[0] ? all[0].user : "");
      // "All time" is smart: it spans this user's own first to last play.
      const span = all.length ? `${dateFmt.format(all[all.length - 1].ended)} – ${dateFmt.format(all[0].ended)}` : "";
      const rangeNote = setting(T, "range", D) === "all" && span ? `All time for ${name}: ${span}` : null;
      const total = sum(plays, (p) => p.watched);
      const cats = CATS.map(([c, label]) => {
        const items = plays.filter((p) => p.cat === c);
        return { c, label, items, time: sum(items, (p) => p.watched), smart: sum(items, (p) => p.smart) };
      });
      const catTile = (x, unit) => tile(x.label, hours(x.time), x.items.length ? `${dec(x.smart)} ${unit} · ${num(x.items.length)} plays` : "none", catColor(x.c));
      const streams = STREAMS.map(([s, label]) => {
        const items = plays.filter((p) => p.stream === s);
        return { name: label, value: sum(items, (p) => p.watched), color: streamColor(s), sub: `${hours(sum(items, (p) => p.watched))} · ${num(items.length)} plays`, n: items.length };
      }).filter((s) => s.n > 0 || s.name !== "Unknown");
      content.replaceChildren(...[
        rangeNote ? el("p", { class: "note range-note", text: rangeNote }) : null,
        el("div", { class: "tiles" },
          tile("Total watch time", hours(total), plays.some((p) => p.estimated) ? "≈ includes estimated older plays" : `${num(plays.length)} plays`),
          catTile(cats[0], "movies watched"), catTile(cats[1], "episodes watched"),
          showAudiobooks ? catTile(cats[2], "books listened") : null,
          cats[3].items.length ? catTile(cats[3], "plays") : null),
        el("div", { class: "grid2" },
          panel("How they stream", donut(streams, pct(streams[0].value, total), "direct play", hours), el("p", { class: "note", text: "Share of watch time. Remux copies the video into a new container without re-encoding it." })),
          panel("What they watch", donut(cats.filter((x) => x.c === "movies" || x.c === "tv" || (x.c === "audiobooks" && showAudiobooks) || (x.c === "other" && x.items.length)).map((x) => ({ name: x.label, value: x.time, color: catColor(x.c), sub: `${hours(x.time)} · ${num(x.items.length)} plays` })), hours(total), "watched", hours))),
        sessions(plays, T)].filter(Boolean));
    };
    userSel.addEventListener("change", () => {
      setSetting(T, "user", userSel.value); setSetting(T, "profile", ""); state.page.users = 0;
      if (state.tableFilters) delete state.tableFilters.users; // another user's values differ
      fillProfiles(); draw();
    });
    profileSel.addEventListener("change", () => { setSetting(T, "profile", profileSel.value); state.page.users = 0; draw(); });
    fillProfiles();
    view.append(toolbar(
      el("label", { class: "field" }, el("span", { text: "User" }), userSel),
      el("label", { class: "field" }, el("span", { text: "Profile" }), profileSel),
      rangeControl(T, D, () => { state.page.users = 0; draw(); })), content);
    draw();
  }

  // ---------- session tables (Users and History) ----------
  // Each column can filter the table: value columns by ticking values in
  // the header menu or by clicking a cell, Title by text, Watched by a
  // minimum. Filters last while the page is open, per table.
  const FUNNEL = () => {
    const icon = svg("svg", { viewBox: "0 0 16 16", width: 13, height: 13, "aria-hidden": "true" });
    icon.append(svg("path", { d: "M2 3h12l-4.6 5.4V13l-2.8-1.4V8.4z", fill: "currentColor" }));
    return icon;
  };
  function sessionColumns(showUser) {
    const cols = [
      { key: "started", label: "Started", cell: (p) => dateTimeFmt.format(new Date(p.started || p.ended)) },
      { key: "who", label: showUser ? "User" : "Profile", kind: "values", value: (p) => (showUser ? who(p) : p.profile) || "(none)" },
      { key: "title", label: "Title", kind: "text", cls: "title", cell: (p) => playTitle(p) },
      { key: "type", label: "Type", kind: "values", value: (p) => CAT_LABEL[p.cat], mark: (p) => el("span", { class: "dot", style: `--c:${catColor(p.cat)}` }) },
      { key: "stream", label: "Stream", kind: "values", value: (p) => STREAM_LABEL[p.stream], pill: (p) => streamColor(p.stream) },
      { key: "watched", label: "Watched", kind: "min", cls: "num",
        cell: (p) => (p.estimated ? "≈ " : "") + hours(p.watched) + (p.duration ? ` of ${hours(p.duration)}` : "") },
      { key: "done", label: "Done", kind: "values", value: (p) => (p.completed ? "Yes" : "No") },
    ];
    if (state.features.clients) {
      cols.push({ key: "client", label: "Client", kind: "values",
        value: (p) => [p.client, { local: "Local", remote: "Remote" }[p.location]].filter(Boolean).join(" · ") || "Unknown" });
    }
    return cols;
  }
  const titleText = (p) => p._title || (p._title = playTitle(p).toLowerCase());

  function filterPlays(plays, cols, f, skip) {
    const text = f.text.trim().toLowerCase();
    const active = cols.filter((c) => c.kind === "values" && c.key !== skip && f.values[c.key]);
    const minSec = skip === "watched" ? 0 : f.min * 60;
    if (!text && !active.length && !minSec) return plays;
    return plays.filter((p) => (!text || titleText(p).includes(text)) && (!minSec || p.watched >= minSec) &&
      active.every((c) => { const r = f.values[c.key]; return r.not !== r.set.has(c.value(p)); }));
  }

  // popover opens a small menu under an anchor; it closes on Escape, on a
  // click elsewhere, or when another opens.
  let openPop = null;
  function popover(anchor, content) {
    if (openPop) openPop.close();
    const pop = el("div", { class: "popover", role: "dialog" }, content);
    document.body.append(pop);
    const r = anchor.getBoundingClientRect();
    pop.style.top = `${r.bottom + window.scrollY + 4}px`;
    pop.style.left = `${Math.max(8, Math.min(r.left + window.scrollX, window.scrollX + document.documentElement.clientWidth - pop.offsetWidth - 8))}px`;
    const onDown = (e) => { if (!pop.contains(e.target) && !anchor.contains(e.target)) close(); };
    const onKey = (e) => { if (e.key === "Escape") { close(); anchor.focus(); } };
    const close = () => {
      pop.remove(); anchor.setAttribute("aria-expanded", "false");
      document.removeEventListener("pointerdown", onDown, true); document.removeEventListener("keydown", onKey);
      if (openPop && openPop.pop === pop) openPop = null;
    };
    document.addEventListener("pointerdown", onDown, true); document.addEventListener("keydown", onKey);
    anchor.setAttribute("aria-expanded", "true");
    openPop = { pop, close };
    const first = pop.querySelector("input, button");
    if (first) first.focus();
    return close;
  }

  function sessions(plays, pageKey) {
    plays = plays.slice().sort((a, b) => (b.started || b.ended) - (a.started || a.ended));
    const showUser = pageKey !== "users";
    const cols = sessionColumns(showUser);
    if (!state.tableFilters) state.tableFilters = {};
    const f = state.tableFilters[pageKey] || (state.tableFilters[pageKey] = { text: "", values: {}, min: 0 });
    let rows = plays;

    const count = el("h2");
    const exportBtn = el("button", { type: "button", class: "small", text: "Export CSV", onclick: () => csv(rows) });
    const search = el("input", { type: "search", class: "search", placeholder: "Filter titles…", "aria-label": "Filter by title", value: f.text });
    let timer = 0;
    search.addEventListener("input", () => { clearTimeout(timer); timer = setTimeout(() => { f.text = search.value; changed(); }, 150); });
    const chips = el("div", { class: "chips" });
    const box = el("div");

    const changed = () => { state.page[pageKey] = 0; draw(); };
    // A value filter is either "only these" or "all but these" (unticking
    // from the full list), so values hidden by other filters aren't lost.
    const setValues = (key, rule) => { if (!rule || (rule.not && !rule.set.size)) delete f.values[key]; else f.values[key] = rule; changed(); };
    const only = (v) => ({ not: false, set: new Set([v]) });

    const valuesMenu = (col, anchor) => {
      // Counts reflect the other filters, so the menu shows what's left.
      const counts = new Map();
      for (const p of filterPlays(plays, cols, f, col.key)) { const v = col.value(p); counts.set(v, (counts.get(v) || 0) + 1); }
      const values = [...counts.keys()].sort((a, b) => counts.get(b) - counts.get(a) || a.localeCompare(b));
      const list = el("div", { class: "pop-list" });
      const shown = (v) => { const r = f.values[col.key]; return !r || r.not !== r.set.has(v); };
      const drawList = (q) => list.replaceChildren(...values.filter((v) => !q || v.toLowerCase().includes(q)).map((v) => {
        const cb = el("input", { type: "checkbox" });
        cb.checked = shown(v);
        cb.addEventListener("change", () => {
          const r = f.values[col.key] || { not: true, set: new Set() };
          // In "all but" mode the set lists what's unticked; otherwise what's ticked.
          if (r.not !== cb.checked) r.set.add(v); else r.set.delete(v);
          setValues(col.key, r);
        });
        const onlyBtn = el("button", { type: "button", class: "only", text: "only", onclick: () => { setValues(col.key, only(v)); close(); } });
        return el("label", { class: "pop-item" }, cb, el("span", { class: "v", title: v, text: v }), el("span", { class: "n", text: num(counts.get(v)) }), onlyBtn);
      }));
      const find = values.length > 8 ? el("input", { type: "search", class: "search", placeholder: `Find ${col.label.toLowerCase()}…`, "aria-label": `Find ${col.label}` }) : null;
      if (find) find.addEventListener("input", () => drawList(find.value.trim().toLowerCase()));
      drawList("");
      const close = popover(anchor, [el("div", { class: "pop-title", text: `Filter ${col.label}` }), find, list,
        el("div", { class: "pop-foot" },
          el("button", { type: "button", class: "small link", text: "Show all", onclick: () => { setValues(col.key, null); close(); } }),
          el("button", { type: "button", class: "small", text: "Done", onclick: () => close() }))].filter(Boolean));
    };
    const minMenu = (anchor) => {
      const input = el("input", { type: "number", min: "0", step: "1", value: f.min || "", placeholder: "0", "aria-label": "Minimum minutes watched" });
      const apply = (v) => { f.min = Math.max(0, Number(v) || 0); changed(); };
      input.addEventListener("input", () => apply(input.value));
      const close = popover(anchor, [el("div", { class: "pop-title", text: "Watched at least" }),
        el("label", { class: "pop-min" }, input, "minutes"),
        el("div", { class: "pop-presets" }, [1, 5, 10, 30, 60].map((m) => el("button", { type: "button", class: "small", text: `${m}m`,
          onclick: () => { input.value = m; apply(m); } }))),
        el("div", { class: "pop-foot" },
          el("button", { type: "button", class: "small link", text: "Clear", onclick: () => { apply(0); close(); } }),
          el("button", { type: "button", class: "small", text: "Done", onclick: () => close() }))]);
    };

    const drawChips = () => {
      const list = [];
      for (const c of cols) {
        const r = f.values[c.key];
        if (!r) continue;
        const vals = [...r.set];
        const shownVals = vals.length ? `${vals.slice(0, 3).join(", ")}${vals.length > 3 ? ` +${vals.length - 3}` : ""}` : "none";
        list.push(el("span", { class: "chip" }, `${c.label}: ${r.not ? "not " : ""}${shownVals}`,
          el("button", { type: "button", "aria-label": `Remove ${c.label} filter`, onclick: () => setValues(c.key, null) }, "×")));
      }
      if (f.min) list.push(el("span", { class: "chip" }, `Watched ≥ ${num(f.min)}m`,
        el("button", { type: "button", "aria-label": "Remove watched filter", onclick: () => { f.min = 0; changed(); } }, "×")));
      if (list.length || f.text) {
        list.push(el("button", { type: "button", class: "small link", text: "Clear filters",
          onclick: () => { f.text = ""; search.value = ""; f.values = {}; f.min = 0; changed(); } }));
      }
      chips.replaceChildren(...list);
    };

    const table = (page) => el("div", { class: "table-wrap" }, el("table", {},
      el("thead", {}, el("tr", {}, cols.map((c) => {
        const th = el("th", { class: c.cls === "num" ? "num" : null }, c.label);
        if (c.kind === "values" || c.kind === "min") {
          const on = c.kind === "min" ? f.min > 0 : Boolean(f.values[c.key]);
          const b = el("button", { type: "button", class: "th-filter" + (on ? " on" : ""), "aria-label": `Filter ${c.label}`, "aria-haspopup": "dialog", "aria-expanded": "false" }, FUNNEL());
          b.addEventListener("click", () => (c.kind === "min" ? minMenu(b) : valuesMenu(c, b)));
          th.append(b);
        }
        return th;
      }))),
      el("tbody", {}, page.map((p) => el("tr", {}, cols.map((c) => {
        if (c.kind !== "values") {
          return el("td", { class: [c.cls, c.key === "watched" && p.estimated ? "est" : null].filter(Boolean).join(" ") || null,
            title: c.key === "watched" && p.estimated ? "Estimated: recorded before Silo measured watch time" : null, text: c.cell(p) });
        }
        // Clicking a value shows only rows with that value; again clears it.
        const v = c.value(p);
        const r = f.values[c.key];
        const isOnly = r && !r.not && r.set.size === 1 && r.set.has(v);
        const inner = c.pill ? el("span", { class: "pill", style: `--c:${c.pill(p)}`, text: v }) : [c.mark ? c.mark(p) : null, v];
        return el("td", {}, el("button", { type: "button", class: "cell-filter" + (c.key === "done" && v === "No" ? " muted" : ""),
          title: isOnly ? `Show every ${c.label.toLowerCase()}` : `Show only ${v}`,
          onclick: () => setValues(c.key, isOnly ? null : only(v)) }, inner));
      }))))));

    const draw = () => {
      rows = filterPlays(plays, cols, f);
      const filtered = rows.length !== plays.length;
      count.textContent = filtered ? `Playback sessions (${num(rows.length)} of ${num(plays.length)})` : `Playback sessions (${num(plays.length)})`;
      exportBtn.disabled = !rows.length;
      exportBtn.textContent = filtered ? "Export filtered CSV" : "Export CSV";
      drawChips();
      const pages = Math.max(1, Math.ceil(rows.length / PAGE));
      state.page[pageKey] = Math.min(state.page[pageKey] || 0, pages - 1);
      const at = state.page[pageKey];
      const go = (d) => { state.page[pageKey] = at + d; draw(); };
      box.replaceChildren(...[
        rows.length ? table(rows.slice(at * PAGE, (at + 1) * PAGE)) : empty(plays.length ? "No sessions match these filters." : undefined),
        pages > 1 ? el("div", { class: "pager" },
          el("button", { type: "button", class: "small", disabled: at === 0, onclick: () => go(-1), text: "Newer" }),
          el("span", { text: `Page ${at + 1} of ${num(pages)}` }),
          el("button", { type: "button", class: "small", disabled: at >= pages - 1, onclick: () => go(1), text: "Older" })) : null,
        rows.some((p) => p.estimated) ? el("p", { class: "note", text: "≈ marks plays recorded before Silo measured watch time; their watch time is an estimate." }) : null].filter(Boolean));
    };
    draw();
    return el("section", { class: "panel" },
      el("div", { class: "panel-head" }, count, exportBtn),
      el("div", { class: "table-tools" }, search, chips),
      box);
  }

  // ---------- Breakdown tab ----------
  function renderBreakdown(view) {
    const T = "breakdown", D = "30";
    const users = groupBy(state.plays, (p) => p.userId, "time");
    const userOpts = [["", "Everyone"], ...users.map((g) => [g.key, g.items[0].user]).sort((a, b) => a[1].localeCompare(b[1]))];
    const content = el("div", { class: "stack" });
    const draw = () => {
      const measure = setting(T, "measure", "time");
      const fmt = fmtBy(measure);
      const user = setting(T, "user", "");
      const plays = inRange(state.plays, bounds(T, D)).filter((p) => !user || p.userId === user);
      const viewers = (g) => new Set(g.items.map((p) => p.userId)).size;
      const smartUnit = { movies: "× watched", tv: " episodes", audiobooks: "× listened", other: "×" };
      const valueText = (c) => (g) => (measure === "smart" ? dec(g.value) + smartUnit[c] : fmt(g.value));
      const subText = (c) => (g) => [
        measure !== "time" ? hours(g.time) : null,
        measure !== "smart" ? `${dec(g.smart)}${smartUnit[c]}` : null,
        measure !== "plays" ? `${num(g.plays)} plays` : null,
        `${num(viewers(g))} ${viewers(g) === 1 ? "user" : "users"}`,
      ].filter(Boolean).join(" · ");
      const section = (c, title, key, label, note) => {
        const items = plays.filter((p) => p.cat === c);
        const groups = groupBy(items, key, measure);
        const total = sum(items, MEASURES[measure]);
        const totalText = measure === "smart" ? dec(total) + smartUnit[c] : fmt(total);
        const head = [title, el("span", { class: "total", text: items.length ? `${totalText} total · ${num(groups.length)} ${groups.length === 1 ? "title" : "titles"}` : "" })];
        if (!items.length) return panel(head, empty(note || `No ${title.toLowerCase()} in this range.`));
        if (chart === "list") return panel(head, barList(groups, label, valueText(c), subText(c), () => catColor(c), limit));
        // Pie: the top items, then everything else as one slice.
        const top = groups.slice(0, limit), rest = groups.slice(limit);
        const slices = top.map((g, i) => ({ name: label(g), value: g.value, color: seriesColor(i), sub: subText(c)(g) ? `${valueText(c)(g)} · ${subText(c)(g)}` : valueText(c)(g) }));
        if (rest.length) {
          const v = rest.reduce((a, g) => a + g.value, 0);
          slices.push({ name: `Other (${num(rest.length)} ${rest.length === 1 ? "title" : "titles"})`, value: v, color: "var(--c-other)",
            sub: measure === "smart" ? dec(v) + smartUnit[c] : fmt(v) });
        }
        return panel(head, donut(slices, "", "", valueText(c), { pie: true, size: 200 }));
      };
      const chart = setting(T, "chart", "list");
      const limitSetting = setting(T, "limit", "10");
      const limit = limitSetting === "all" ? Infinity : Number(limitSetting) || 10;
      const sections = [
        section("movies", "Movies", (p) => p.itemId, (g) => g.items[0].title),
        section("tv", "TV shows", (p) => p.series || p.itemId, (g) => g.items[0].series || g.items[0].title),
      ];
      if (showAudiobooks) {
        sections.push(section("audiobooks", "Audiobooks", (p) => p.itemId, (g) => g.items[0].title,
          "No audiobook plays in this range. Silo's playback history covers audiobooks played in Silo's own apps; Audiobookshelf-app listening isn't recorded there."));
      }
      if (plays.some((p) => p.cat === "other")) sections.push(section("other", "Other", (p) => p.itemId, (g) => g.items[0].title));
      content.replaceChildren(...[
        measure === "smart" ? el("p", { class: "note range-note", text: "Smart plays divide watch time by the length of the movie, episode or book: two half-watched episodes count as one episode watched." }) : null,
        el("div", { class: "grid3" }, sections)].filter(Boolean));
    };
    view.append(toolbar(
      rangeControl(T, D, draw),
      segmented("View", T, "measure", MEASURE_OPTIONS, "time", draw),
      segmented("Chart", T, "chart", [["list", "List"], ["pie", "Pie"]], "list", draw),
      selectControl("Show", T, "limit", [["5", "Top 5"], ["10", "Top 10"], ["20", "Top 20"], ["50", "Top 50"], ["all", "All"]], "10", draw),
      selectControl("User", T, "user", userOpts, "", draw)), content);
    draw();
  }

  // ---------- Clients tab (optional feature) ----------
  function renderClients(view) {
    const T = "clients", D = "30";
    const content = el("div", { class: "stack" });
    const draw = () => {
      const plays = inRange(state.plays, bounds(T, D));
      const tracked = plays.filter((p) => p.client || p.location);
      const loc = (p) => ({ local: "Local network", remote: "Remote" }[p.location] || "Unknown");
      const list = (groups) => barList(groups, (g) => g.key, (g) => hours(g.value), (g) => `${num(g.plays)} plays`, null, 15);
      content.replaceChildren(
        el("p", { class: "note range-note", text: `${num(tracked.length)} of ${num(plays.length)} plays in this range have client details. Only plays that started while client tracking was on are covered.` }),
        el("div", { class: "grid2" },
          panel("By client", list(groupBy(tracked, (p) => p.client || "Unknown"))),
          panel("Local or remote", list(groupBy(tracked, loc))),
          panel("Transcodes by client", list(groupBy(tracked.filter((p) => p.stream === "transcode"), (p) => p.client || "Unknown")))));
    };
    view.append(toolbar(rangeControl(T, D, draw)), content);
    draw();
  }

  // ---------- History tab ----------
  function renderHistory(view) {
    const T = "history", D = "30";
    const content = el("div", { class: "stack" });
    const draw = () => {
      state.page.history = 0;
      content.replaceChildren(sessions(byCat(inRange(state.plays, bounds(T, D)), setting(T, "cat", "all")), "history"));
    };
    view.append(toolbar(rangeControl(T, D, draw), segmented("Show", T, "cat", catFilter(), "all", draw)), content);
    draw();
  }

  function csv(plays) {
    const q = (s) => `"${String(s).replace(/"/g, '""')}"`;
    const lines = [["ended_at", "started_at", "user", "profile", "title", "series", "season", "episode", "type", "stream", "watched_seconds", "watched_is_estimate", "duration_seconds", "smart_plays", "completed", "client", "location"].join(",")];
    for (const p of plays) lines.push([new Date(p.ended).toISOString(), new Date(p.started).toISOString(), p.user, p.profile, p.title, p.series,
      p.series ? p.season : "", p.series ? p.episode : "", p.type, p.stream, p.watched, p.estimated, p.duration || "", p.smart.toFixed(3), p.completed, p.client, p.location].map(q).join(","));
    const blob = new Blob([lines.join("\n") + "\n"], { type: "text/csv" });
    const a = el("a", { href: URL.createObjectURL(blob), download: `playback-history-${isoDay(Date.now())}.csv` });
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }

  // ---------- Customize (colors) ----------
  const DRAFT_KEY = "colors-draft";
  // spreadColor suggests a new color once the defaults are used: hues a
  // golden angle apart, so neighbours never look alike.
  function spreadColor(i) {
    const h = (i * 137.508) % 360, s = 0.62, l = 0.52;
    const f = (n) => {
      const k = (n + h / 30) % 12, a = s * Math.min(l, 1 - l);
      return Math.round(255 * (l - a * Math.max(-1, Math.min(k - 3, 9 - k, 1)))).toString(16).padStart(2, "0");
    };
    return `#${f(0)}${f(8)}${f(4)}`;
  }
  const copy = (c) => JSON.parse(JSON.stringify(c));

  // openCustomize shows the page settings: audiobooks and colors. Changes
  // preview live; Cancel returns to what the page had, Save stores them in
  // the plugin for everyone.
  function openCustomize(start, note) {
    const original = copy(state.colors);
    let work = copy(start || state.colors);
    const preview = () => { applyColors(work); setAudiobooks(work.show_audiobooks); render(); };
    const msg = el("p", { class: "dlg-msg" + (note ? " error" : ""), role: "status", "aria-live": "polite", text: note || "" });

    const picker = (value, label, onInput) => {
      const input = el("input", { type: "color", value, "aria-label": label });
      const code = el("code", { text: value });
      input.addEventListener("input", () => { code.textContent = input.value; onInput(input.value); preview(); });
      return [input, code];
    };
    const body = el("div", { class: "dlg-body" });
    const addBtn = el("button", { type: "button", class: "add-color", "aria-label": "Add color", title: "Add color" }, "+");
    const paletteList = el("div", { class: "palette" });
    const drawPalette = () => {
      paletteList.replaceChildren(...work.palette.map((c, i) => {
        const input = el("input", { type: "color", value: c, "aria-label": `User color ${i + 1}`, title: `Color ${i + 1}: ${c}` });
        input.addEventListener("input", () => { work.palette[i] = input.value; input.title = `Color ${i + 1}: ${input.value}`; preview(); });
        return el("div", { class: "swatch-edit" }, input,
          work.palette.length > 1 ? el("button", { type: "button", class: "remove", "aria-label": `Remove color ${i + 1}`, title: "Remove",
            onclick: () => { work.palette.splice(i, 1); drawPalette(); preview(); } }, "×") : null);
      }), addBtn);
      addBtn.disabled = work.palette.length >= MAX_PALETTE;
      addBtn.title = addBtn.disabled ? `Up to ${MAX_PALETTE} colors` : "Add color";
    };
    const drawBody = () => {
      const toggle = el("input", { type: "checkbox", role: "switch", class: "switch" });
      toggle.checked = work.show_audiobooks;
      toggle.addEventListener("change", () => { work.show_audiobooks = toggle.checked; drawBody(); preview(); body.querySelector(".switch").focus(); });
      body.replaceChildren(
        el("fieldset", { class: "colors" }, el("legend", { text: "Reports" }),
          el("label", { class: "color-row" }, el("span", { class: "c-label" }, "Show audiobooks",
            el("span", { class: "note block", text: "Adds audiobooks to the filters, the Users tab and Breakdown. When off, audiobook plays are left out of every report." })), toggle)),
        ...COLOR_ROLES.map(([group, roles]) => el("fieldset", { class: "colors" },
          el("legend", { text: group }),
          roles.filter(([k]) => k !== "audiobooks" || work.show_audiobooks).map(([k, label]) => el("label", { class: "color-row" },
            el("span", { class: "c-label", text: label }),
            picker(work.colors[k], `${label} color`, (v) => { work.colors[k] = v; }))))),
        el("fieldset", { class: "colors" }, el("legend", { text: "User colors" }),
          el("p", { class: "note", text: "Used in order for users in the Activity chart and for titles in pie charts. Click a color to change it, + to add one, × to remove it." }),
          paletteList));
      drawPalette();
    };
    addBtn.addEventListener("click", () => {
      // Suggest a default color that isn't in use yet.
      work.palette.push(DEFAULT_PALETTE.find((c) => !work.palette.includes(c)) || spreadColor(work.palette.length));
      drawPalette(); preview();
      const inputs = paletteList.querySelectorAll('input[type="color"]');
      inputs[inputs.length - 1].focus();
      inputs[inputs.length - 1].click();
    });
    drawBody();

    const close = (saved) => {
      if (!saved) { state.colors = original; applyColors(original); setAudiobooks(original.show_audiobooks); render(); }
      dlg.close(); dlg.remove();
    };
    const saveBtn = el("button", { type: "button", class: "small primary", text: "Save" });
    saveBtn.addEventListener("click", async () => {
      saveBtn.disabled = true; msg.textContent = "Saving…"; msg.classList.remove("error");
      try {
        const res = await fetch("settings", { method: "POST", credentials: "same-origin",
          headers: { "Content-Type": "application/json", Accept: "application/json" }, body: JSON.stringify(work) });
        if (res.status === 401 || res.status === 403) throw Object.assign(new Error("expired"), { expired: true });
        const saved = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(saved.error || `The plugin answered ${res.status}.`);
        state.colors = fullColors(saved);
        remember(DRAFT_KEY, "");
        close(true);
        applyColors(state.colors); setAudiobooks(state.colors.show_audiobooks); render();
      } catch (err) {
        saveBtn.disabled = false;
        msg.classList.add("error");
        if (err.expired) {
          // Access to this page lasts five minutes; keep the picks for next time.
          remember(DRAFT_KEY, JSON.stringify(work));
          msg.textContent = "Your access to this page expired, so these settings weren't saved. Open Playback Reporting again from the admin menu and your picks will be waiting.";
        } else {
          msg.textContent = err.message || "Saving failed.";
        }
      }
    });
    const dlg = el("dialog", { class: "dlg", "aria-labelledby": "dlg-title" },
      el("div", { class: "dlg-head" }, el("h2", { id: "dlg-title", text: "Customize" }),
        el("button", { type: "button", class: "icon", "aria-label": "Close", onclick: () => close(false) }, "×")),
      body, msg,
      el("div", { class: "dlg-foot" },
        el("button", { type: "button", class: "small link", text: "Reset colors",
          onclick: () => { work = { ...fullColors({ colors: {}, palette: [] }), show_audiobooks: work.show_audiobooks }; drawBody(); preview(); } }),
        el("span", { class: "grow" }),
        el("button", { type: "button", class: "small", text: "Cancel", onclick: () => close(false) }),
        saveBtn));
    dlg.addEventListener("cancel", (e) => { e.preventDefault(); close(false); });
    document.body.append(dlg);
    dlg.showModal();
    preview();
  }

  // A save that failed because access expired reopens with those picks.
  function resumeDraft() {
    const raw = remember(DRAFT_KEY);
    if (!raw) return;
    try {
      openCustomize(fullColors(JSON.parse(raw)), "These are the settings you picked before your access expired. Save to keep them.");
    } catch (_) { remember(DRAFT_KEY, ""); }
  }

  function wireHeader() {
    $("conn").addEventListener("click", () => openCheck());
    showCheck();
    // The startup check may still be running when the page opens.
    if (!state.check) setTimeout(() => runCheck().catch(() => {}), 1500);
    $("customize").addEventListener("click", () => openCustomize());
  }

  // ---------- Connection check ----------
  // The plugin checks Silo and the API key when it starts; the header shows
  // the result and the dialog can run it again.
  const STEP_ICON = { ok: "✓", fail: "✕", warn: "!", skip: "–" };
  const STEP_WORD = { ok: "Passed", fail: "Failed", warn: "Warning", skip: "Skipped" };
  state.check = (() => { try { return JSON.parse(($("last-check") || {}).textContent || "null"); } catch (_) { return null; } })();

  function showCheck() {
    const btn = $("conn"), label = $("conn-label");
    if (!btn) return;
    const c = state.check;
    const st = !c ? "checking" : c.ok ? (c.steps.some((x) => x.state === "warn") ? "warn" : "ok") : "fail";
    btn.dataset.state = st;
    label.textContent = { checking: "Checking connection…", ok: "Connected", warn: "Connected, with a warning", fail: "Connection problem" }[st];
    btn.title = c && c.account ? `Signed in to ${c.server} with ${c.account}'s key` : "";
    // A failed check also gets a banner above the reports.
    const old = $("conn-banner");
    if (old) old.remove();
    if (c && !c.ok && main) {
      const first = c.steps.find((x) => x.state === "fail");
      main.before(el("div", { class: "banner", id: "conn-banner", role: "alert" },
        el("strong", { text: "Playback Reporting can't fully reach Silo. " }),
        first ? `${first.name}: ${first.detail || "failed"}` : "",
        el("button", { type: "button", class: "small link", text: "Details", onclick: () => openCheck() })));
    }
  }

  async function runCheck() {
    const res = await fetch("check", { credentials: "same-origin", headers: { Accept: "application/json" } });
    if (res.status === 401 || res.status === 403) throw new Error("Your access to this page expired. Open Playback Reporting again from the admin menu to check again.");
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `The plugin answered ${res.status}.`);
    state.check = body;
    showCheck();
    return body;
  }

  function openCheck() {
    const list = el("ol", { class: "check-steps" });
    const meta = el("p", { class: "note" });
    const msg = el("p", { class: "dlg-msg", role: "status", "aria-live": "polite" });
    const draw = () => {
      const c = state.check;
      if (!c) { list.replaceChildren(el("li", { class: "step skip" }, "The first check is still running.")); meta.textContent = ""; return; }
      list.replaceChildren(...c.steps.map((x) => el("li", { class: `step ${x.state}` },
        el("span", { class: "icon", "aria-hidden": "true", text: STEP_ICON[x.state] || "?" }),
        el("span", { class: "sr", text: STEP_WORD[x.state] + ": " }),
        el("span", { class: "s-name", text: x.name }),
        x.detail ? el("span", { class: "s-detail", text: x.detail }) : null)));
      meta.textContent = [c.server ? `Server: ${c.server}` : null, c.account ? `Key: ${c.account}` : null,
        `Checked ${dateTimeFmt.format(new Date(c.at))}`].filter(Boolean).join(" · ");
    };
    draw();
    const again = el("button", { type: "button", class: "small primary", text: "Check again" });
    again.addEventListener("click", async () => {
      again.disabled = true; msg.classList.remove("error"); msg.textContent = "Checking…";
      try { await runCheck(); msg.textContent = state.check.ok ? "Everything works." : "Some steps failed; see below."; draw(); }
      catch (err) { msg.classList.add("error"); msg.textContent = err.message; }
      again.disabled = false;
    });
    const close = () => { dlg.close(); dlg.remove(); };
    const dlg = el("dialog", { class: "dlg", "aria-labelledby": "check-title" },
      el("div", { class: "dlg-head" }, el("h2", { id: "check-title", text: "Connection to Silo" }),
        el("button", { type: "button", class: "icon", "aria-label": "Close", onclick: close }, "×")),
      el("div", { class: "dlg-body" }, list, meta,
        el("p", { class: "note", text: "The plugin reads Silo's playback history with the API key in its settings. Change the key or server address there; the plugin restarts and checks again." })),
      msg,
      el("div", { class: "dlg-foot" }, el("span", { class: "grow" }), el("button", { type: "button", class: "small", text: "Close", onclick: close }), again));
    dlg.addEventListener("cancel", (e) => { e.preventDefault(); close(); });
    document.body.append(dlg);
    dlg.showModal();
  }

  // ---------- tabs ----------
  const RENDER = { activity: renderActivity, users: renderUsers, breakdown: renderBreakdown, clients: renderClients, history: renderHistory };

  function render() {
    if (!RENDER[state.tab] || (state.tab === "clients" && !state.features.clients)) state.tab = "activity";
    for (const b of document.querySelectorAll("#tabs button")) {
      const on = b.dataset.tab === state.tab;
      b.setAttribute("aria-selected", String(on));
      b.tabIndex = on ? 0 : -1;
    }
    main.setAttribute("aria-labelledby", "tab-" + state.tab);
    const view = el("div", { class: "stack" });
    RENDER[state.tab](view);
    main.replaceChildren(view);
  }

  function wireTabs() {
    const tabs = $("tabs");
    tabs.addEventListener("click", (e) => {
      const b = e.target.closest("button[data-tab]");
      if (!b || b.dataset.tab === state.tab) return;
      state.tab = b.dataset.tab; remember("tab", state.tab); render();
    });
    tabs.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
      const list = [...tabs.querySelectorAll("button[data-tab]")].filter((b) => !b.hidden);
      const i = list.findIndex((b) => b.dataset.tab === state.tab);
      const next = list[(i + (e.key === "ArrowRight" ? 1 : list.length - 1)) % list.length];
      next.focus(); next.click();
    });
  }

  wireTabs();
  wireHeader();
  load().then(() => {
    $("tab-clients").hidden = !state.features.clients;
    showStatus();
    render();
    $("customize").disabled = false;
    resumeDraft();
  }).catch((err) => {
    statusEl.textContent = err.message || String(err);
    statusEl.classList.add("error");
    main.replaceChildren(el("section", { class: "panel" }, el("p", { text: err.message || String(err) })));
  });
})();
