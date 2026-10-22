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
  const PAGE = 25;
  const MAX_SERIES = 8;

  const $ = (id) => document.getElementById(id);
  const statusEl = $("status");
  const main = $("main");
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
  const startOfDay = (ms) => { const d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime(); };
  const isoDay = (ms) => { const d = new Date(ms); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`; };
  const playTitle = (p) => p.series ? `${p.series} · S${String(p.season).padStart(2, "0")}E${String(p.episode).padStart(2, "0")} · ${p.title}` : p.title;
  const who = (p) => (p.profile && p.profile.toLowerCase() !== p.user.toLowerCase() ? `${p.user} · ${p.profile}` : p.user);

  // ---------- colors ----------
  // Settings colors override the stylesheet's defaults (both themes).
  function applyColors(colors, palette) {
    const root = document.documentElement.style;
    const vars = { direct_play: "--c-direct", direct_stream: "--c-remux", transcode: "--c-transcode", movies: "--c-movies", tv: "--c-tv", audiobooks: "--c-audiobooks" };
    for (const [k, v] of Object.entries(colors || {})) if (vars[k] && /^#[0-9a-f]{6}$/i.test(v)) root.setProperty(vars[k], v);
    const list = (palette || []).filter((c) => /^#[0-9a-f]{6}$/i.test(c));
    list.forEach((c, i) => root.setProperty(`--p${i}`, c));
    state.palette = list.length;
  }
  const PALETTE_DEFAULT = 8;
  const seriesColor = (i) => `var(--p${i % (state.palette || PALETTE_DEFAULT)})`;
  const streamColor = (s) => `var(--c-${s === "direct" ? "direct" : s === "unknown" ? "other" : s})`;
  const catColor = (c) => (c === "other" ? "var(--c-other)" : `var(--c-${c})`);

  // ---------- loading ----------
  async function fetchJSON(url) {
    const res = await fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" } });
    let body = null;
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
      return {
        ended: r[col.ended] * 1000,
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
    state.plays = chunks.flatMap(decode).sort((a, b) => b.ended - a.ended);
    state.features = first.features || { clients: false };
    state.status = first.status;
    applyColors(state.features.colors, state.features.palette);
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
    s.value = options.some(([v]) => v === cur) ? cur : fallback;
    s.addEventListener("change", () => { setSetting(tab, key, s.value); onchange(); });
    return el("label", { class: "field" }, el("span", { text: label }), s);
  }

  function segmented(label, tab, key, options, fallback, onchange) {
    let cur = setting(tab, key, fallback);
    if (!options.some(([v]) => v === cur)) cur = fallback;
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
  function legend(series) {
    return el("ul", { class: "legend" }, series.map((s) => el("li", { style: `--c:${s.color}` }, el("span", { class: "sw" }), s.name)));
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
    return { unit, list, start };
  }

  function columnLabel(unit, ms) { return unit === "month" ? monthFmt.format(ms) : shortDate.format(ms); }

  // stackedColumns draws one column per bucket with a stack per series.
  function stackedColumns(b, series, fmt, what) {
    const W = 760, H = 260, L = 52, B = 24, T = 10;
    const totals = b.list.map((_, i) => series.reduce((s, x) => s + x.values[i], 0));
    const maxV = Math.max(1e-9, ...totals);
    const step = (W - L) / b.list.length;
    const bw = Math.max(1, Math.min(32, step * 0.72));
    const y = (v) => H - B - (v / maxV) * (H - B - T);
    const unitName = { day: "day", week: "week", month: "month" }[b.unit];
    const root = svg("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": `${what} per ${unitName}` });
    const grid = svg("g", { class: "grid" });
    for (let i = 0; i <= 4; i++) {
      const v = (maxV * i) / 4, yy = y(v);
      grid.append(svg("line", { x1: L, x2: W, y1: yy, y2: yy }), svg("text", { x: L - 6, y: yy + 4, "text-anchor": "end" }, fmt(v)));
    }
    root.append(grid);
    const labelEvery = Math.ceil(b.list.length / 9);
    b.list.forEach((t, i) => {
      let base = 0;
      const x = L + i * step + (step - bw) / 2;
      const col = svg("g", { class: "col" });
      const tip = [`${b.unit === "week" ? "Week of " : ""}${b.unit === "month" ? monthFmt.format(t) : dateFmt.format(t)}: ${fmt(totals[i])}`];
      series.forEach((s) => {
        const v = s.values[i];
        if (v > 0) {
          col.append(svg("rect", { x, y: y(base + v), width: bw, height: Math.max(0.5, y(base) - y(base + v)), fill: s.color }));
          tip.push(`${s.name}: ${fmt(v)}`);
        }
        base += v;
      });
      // A full-height hit area so short columns are easy to hover.
      col.append(svg("rect", { x: L + i * step, y: T, width: step, height: H - B - T, fill: "transparent" }, svg("title", {}, tip.join("\n"))));
      root.append(col);
      if (i % labelEvery === 0) root.append(svg("text", { x: x + bw / 2, y: H - 6, "text-anchor": "middle" }, columnLabel(b.unit, t)));
    });
    return el("div", { class: "chart" }, root);
  }

  // donut shows shares of a whole; slices are [{name, value, color, sub}].
  function donut(slices, center, centerSub, fmt) {
    const total = slices.reduce((s, x) => s + x.value, 0);
    const R = 70, r = 46, C = 80;
    const root = svg("svg", { viewBox: "0 0 160 160", class: "donut", role: "img", "aria-label": slices.map((s) => `${s.name} ${pct(s.value, total)}`).join(", ") });
    if (total <= 0) {
      root.append(svg("circle", { cx: C, cy: C, r: (R + r) / 2, fill: "none", stroke: "var(--line)", "stroke-width": R - r }));
    } else {
      let a0 = -Math.PI / 2;
      const shown = slices.filter((s) => s.value > 0);
      for (const s of shown) {
        const frac = s.value / total;
        const a1 = a0 + frac * Math.PI * 2;
        let node;
        if (shown.length === 1) {
          node = svg("circle", { cx: C, cy: C, r: (R + r) / 2, fill: "none", stroke: s.color, "stroke-width": R - r });
        } else {
          const gap = Math.min(0.012, frac * 0.2);
          const p = (rad, a) => `${C + rad * Math.cos(a)} ${C + rad * Math.sin(a)}`;
          const b0 = a0 + gap, b1 = a1 - gap, large = b1 - b0 > Math.PI ? 1 : 0;
          node = svg("path", { fill: s.color, d: `M ${p(R, b0)} A ${R} ${R} 0 ${large} 1 ${p(R, b1)} L ${p(r, b1)} A ${r} ${r} 0 ${large} 0 ${p(r, b0)} Z` });
        }
        node.append(svg("title", {}, `${s.name}: ${fmt(s.value)} (${pct(s.value, total)})`));
        root.append(node);
        a0 = a1;
      }
    }
    root.append(svg("text", { x: C, y: C - 2, "text-anchor": "middle", class: "donut-value" }, center),
      svg("text", { x: C, y: C + 16, "text-anchor": "middle", class: "donut-sub" }, centerSub));
    const keys = el("ul", { class: "donut-keys" }, slices.map((s) => el("li", { style: `--c:${s.color}` },
      el("span", { class: "sw" }), el("span", { class: "k-name", text: s.name }),
      el("span", { class: "k-pct", text: pct(s.value, total) }), el("span", { class: "k-sub", text: s.sub || fmt(s.value) }))));
    return el("div", { class: "donut-wrap" }, root, keys);
  }

  // barList is a ranked list with proportional bars.
  function barList(groups, label, valueText, subText, color, limit) {
    if (!groups.length) return empty();
    const top = Math.max(1e-9, groups[0].value);
    const list = el("ol", { class: "bars" });
    const draw = (n) => list.replaceChildren(...groups.slice(0, n).map((g, i) => el("li", { class: "bar" },
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

  const CAT_FILTER = [["all", "All"], ["movies", "Movies"], ["tv", "TV shows"], ["audiobooks", "Audiobooks"]];
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
      const index = new Map(b.list.map((t, i) => [t, i]));
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
      for (const s of series) for (const p of s.items) { const i = index.get(b.start(p.ended)); if (i !== undefined) s.values[i] += f(p); }
      const what = { time: "Watch time", plays: "Plays", smart: "Smart plays" }[measure];
      const people = groupBy(plays, split ? (p) => p.profileKey : (p) => p.userId, measure);
      const colorOf = new Map(shown.map((g, i) => [g.key, seriesColor(i)]));
      content.replaceChildren(
        el("div", { class: "tiles" },
          tile("Watch time", hours(sum(plays, (p) => p.watched)), plays.some((p) => p.estimated) ? "≈ includes estimated older plays" : null),
          tile("Plays", num(plays.length)),
          tile("Smart plays", dec(sum(plays, (p) => p.smart)), "watch time ÷ length"),
          tile(split ? "Active profiles" : "Active users", num(people.length)),
          tile("Transcoded", pct(plays.filter((p) => p.stream === "transcode").length, plays.length), "of plays")),
        panel(`${what} per ${b.unit}`, stackedColumns(b, series, fmt, what), legend(series)),
        el("div", { class: "grid2" },
          panel(split ? "By profile" : "By user", barList(people, (g) => (split ? who(g.items[0]) : g.items[0].user), (g) => fmt(g.value),
            (g) => `${num(g.plays)} plays · ${hours(g.time)}`, stack === "user" ? (g) => colorOf.get(g.key) || "var(--c-other)" : null, 15)),
          panel(`${what} by weekday and hour`, heatmap(plays, measure), el("p", { class: "note", text: "Your local time. Watch time is spread over the hours a play ran." }))));
    };
    view.append(toolbar(
      rangeControl(T, D, draw),
      segmented("Show", T, "cat", CAT_FILTER, "all", draw),
      segmented("Measure", T, "measure", [["time", "Time"], ["plays", "Plays"], ["smart", "Smart plays"]], "time", draw),
      selectControl("Stack by", T, "stack", [["user", "User"], ["cat", "Media type"], ["stream", "Stream type"]], "user", draw),
      checkbox("Separate profiles", T, "profiles", draw)), content);
    draw();
  }

  function heatmap(plays, measure) {
    const grid = Array.from({ length: 7 }, () => new Array(24).fill(0));
    for (const p of plays) {
      const start = p.started || p.ended - p.watched * 1000;
      if (measure !== "time") {
        const d = new Date(start); grid[(d.getDay() + 6) % 7][d.getHours()] += MEASURES[measure](p); continue;
      }
      // Spread watch time over the hours the play ran.
      let left = p.watched * 1000, t = Math.max(start, p.ended - left);
      while (left > 0) {
        const d = new Date(t);
        const next = new Date(d); next.setMinutes(60, 0, 0);
        const span = Math.min(left, next.getTime() - t);
        grid[(d.getDay() + 6) % 7][d.getHours()] += span / 1000;
        left -= span; t += span;
      }
    }
    const maxV = Math.max(1e-9, ...grid.flat());
    const fmt = fmtBy(measure);
    return el("div", { class: "heat", role: "img", "aria-label": "Activity by weekday and hour" },
      el("span"), Array.from({ length: 24 }, (_, h) => el("span", { class: "hour", text: h % 3 === 0 ? String(h) : "" })),
      grid.map((row, d) => [el("span", { class: "wd", text: WEEKDAYS[d] }), row.map((v, h) => el("span", {
        class: "cell", style: v > 0 ? `--a:${(0.15 + 0.85 * (v / maxV)).toFixed(3)}` : null,
        title: `${WEEKDAYS[d]} ${String(h).padStart(2, "0")}:00 · ${fmt(v)}` }))]));
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
      profileSel.replaceChildren(el("option", { value: "", text: "All profiles" }), profiles.map((p) => el("option", { value: p, text: p || "(no profile)" })));
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
          catTile(cats[0], "movies watched"), catTile(cats[1], "episodes watched"), catTile(cats[2], "books listened"),
          cats[3].items.length ? catTile(cats[3], "plays") : null),
        el("div", { class: "grid2" },
          panel("How they stream", donut(streams, pct(streams[0].value, total), "direct play", hours), el("p", { class: "note", text: "Share of watch time. Remux copies the video into a new container without re-encoding it." })),
          panel("What they watch", donut(cats.filter((x) => x.items.length || x.c !== "other").map((x) => ({ name: x.label, value: x.time, color: catColor(x.c), sub: `${hours(x.time)} · ${num(x.items.length)} plays` })), hours(total), "watched", hours))),
        sessions(plays, T)].filter(Boolean));
    };
    userSel.addEventListener("change", () => { setSetting(T, "user", userSel.value); setSetting(T, "profile", ""); state.page.users = 0; fillProfiles(); draw(); });
    profileSel.addEventListener("change", () => { setSetting(T, "profile", profileSel.value); state.page.users = 0; draw(); });
    fillProfiles();
    view.append(toolbar(
      el("label", { class: "field" }, el("span", { text: "User" }), userSel),
      el("label", { class: "field" }, el("span", { text: "Profile" }), profileSel),
      rangeControl(T, D, () => { state.page.users = 0; draw(); })), content);
    draw();
  }

  function sessions(plays, pageKey) {
    plays = plays.slice().sort((a, b) => (b.started || b.ended) - (a.started || a.ended));
    const box = el("div");
    const draw = () => {
      const pages = Math.max(1, Math.ceil(plays.length / PAGE));
      state.page[pageKey] = Math.min(state.page[pageKey] || 0, pages - 1);
      const at = state.page[pageKey];
      const go = (d) => { state.page[pageKey] = at + d; draw(); };
      box.replaceChildren(...[plays.length ? playTable(plays.slice(at * PAGE, (at + 1) * PAGE), pageKey !== "users") : empty(),
        pages > 1 ? el("div", { class: "pager" },
          el("button", { type: "button", class: "small", disabled: at === 0, onclick: () => go(-1), text: "Newer" }),
          el("span", { text: `Page ${at + 1} of ${pages}` }),
          el("button", { type: "button", class: "small", disabled: at >= pages - 1, onclick: () => go(1), text: "Older" })) : null,
        plays.some((p) => p.estimated) ? el("p", { class: "note", text: "≈ marks plays recorded before Silo measured watch time; their watch time is an estimate." }) : null].filter(Boolean));
    };
    draw();
    return panel([`Playback sessions (${num(plays.length)})`, el("button", { type: "button", class: "small", onclick: () => csv(plays), disabled: !plays.length, text: "Export CSV" })], box);
  }

  function playTable(plays, showUser) {
    const showClient = state.features.clients;
    const head = ["Started", showUser ? "User" : "Profile", "Title", "Type", "Stream", "Watched", "Done", showClient ? "Client" : null].filter(Boolean);
    return el("div", { class: "table-wrap" }, el("table", {},
      el("thead", {}, el("tr", {}, head.map((h) => el("th", { class: h === "Watched" ? "num" : null, text: h })))),
      el("tbody", {}, plays.map((p) => el("tr", {},
        el("td", { text: dateTimeFmt.format(new Date(p.started || p.ended)) }),
        el("td", { text: showUser ? who(p) : p.profile }),
        el("td", { class: "title", text: playTitle(p) }),
        el("td", {}, el("span", { class: "dot", style: `--c:${catColor(p.cat)}` }), CAT_LABEL[p.cat]),
        el("td", {}, el("span", { class: "pill", style: `--c:${streamColor(p.stream)}`, text: STREAM_LABEL[p.stream] })),
        el("td", { class: "num" + (p.estimated ? " est" : ""), title: p.estimated ? "Estimated: recorded before Silo measured watch time" : null,
          text: (p.estimated ? "≈ " : "") + hours(p.watched) + (p.duration ? ` of ${hours(p.duration)}` : "") }),
        el("td", { text: p.completed ? "Yes" : "" }),
        showClient ? el("td", { text: [p.client, { local: "Local", remote: "Remote" }[p.location]].filter(Boolean).join(" · ") }) : null)))));
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
        measure !== "plays" ? `${num(g.plays)} plays` : null,
        measure !== "time" ? hours(g.time) : null,
        measure !== "smart" ? `${dec(g.smart)}${smartUnit[c]}` : null,
        `${num(viewers(g))} ${viewers(g) === 1 ? "user" : "users"}`,
      ].filter(Boolean).join(" · ");
      const section = (c, title, key, label, note) => {
        const items = plays.filter((p) => p.cat === c);
        const groups = groupBy(items, key, measure);
        const total = sum(items, MEASURES[measure]);
        return panel([title, el("span", { class: "total", text: items.length ? `${measure === "smart" ? dec(total) + smartUnit[c] : fmt(total)} total` : "" })],
          items.length ? barList(groups, label, valueText(c), subText(c), () => catColor(c), 20) : empty(note || `No ${title.toLowerCase()} in this range.`));
      };
      const sections = [
        section("movies", "Movies", (p) => p.itemId, (g) => g.items[0].title),
        section("tv", "TV shows", (p) => p.series || p.itemId, (g) => g.items[0].series || g.items[0].title),
        section("audiobooks", "Audiobooks", (p) => p.itemId, (g) => g.items[0].title,
          "No audiobook plays in this range. Silo's playback history covers audiobooks played in Silo's own apps; Audiobookshelf-app listening isn't recorded there."),
      ];
      if (plays.some((p) => p.cat === "other")) sections.push(section("other", "Other", (p) => p.itemId, (g) => g.items[0].title));
      content.replaceChildren(...[
        measure === "smart" ? el("p", { class: "note range-note", text: "Smart plays divide watch time by the length of the movie, episode or book: two half-watched episodes count as one episode watched." }) : null,
        el("div", { class: "grid3" }, sections)].filter(Boolean));
    };
    view.append(toolbar(
      rangeControl(T, D, draw),
      segmented("View", T, "measure", [["plays", "Plays"], ["time", "Time"], ["smart", "Smart plays"]], "time", draw),
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
    view.append(toolbar(rangeControl(T, D, draw), segmented("Show", T, "cat", CAT_FILTER, "all", draw)), content);
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
  load().then(() => {
    $("tab-clients").hidden = !state.features.clients;
    showStatus();
    render();
  }).catch((err) => {
    statusEl.textContent = err.message || String(err);
    statusEl.classList.add("error");
    main.replaceChildren(el("section", { class: "panel" }, el("p", { text: err.message || String(err) })));
  });
})();
