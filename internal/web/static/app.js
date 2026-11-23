// The page fetches one JSON view per scrub position and redraws four panes.
// All rendering is textContent-based; nothing from the run is treated as HTML.

const $ = (id) => document.getElementById(id);
const state = { view: null, storm: "", scrub: 100, playing: false, timer: null };

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
}
function hhmmss(iso, loc) {
  const d = new Date(iso);
  const pad = (x) => String(x).padStart(2, "0");
  return pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
}
function chip(a, cls) {
  const c = el("span", "chip" + (cls ? " " + cls : ""));
  c.textContent = a.text;
  c.title = "告警 " + a.id + "\n状态 " + a.status +
    (a.start ? "\n开始 " + hhmmss(a.start) : "") + (a.end ? "\n结束 " + hhmmss(a.end) : "");
  if ((a.labels || {}).severity === "critical") c.classList.add("crit");
  return c;
}

async function load() {
  const q = new URLSearchParams({ storm: state.storm, t: String(state.scrub) });
  const resp = await fetch("/api/view?" + q.toString());
  if (!resp.ok) { $("banner").hidden = false; $("banner").textContent = "加载失败：" + resp.status; return; }
  state.view = await resp.json();
  state.storm = state.view.storm;
  render();
}

function render() {
  const v = state.view;
  if (!v) return;
  $("storm").value = v.storm;
  const scrub = $("scrub");
  scrub.max = String(v.length);
  scrub.value = String(v.scrub);
  $("clock").textContent = hhmmss(v.now) + " / " + v.length / 60 + " 分钟";
  renderSummary(v);
  renderBanner(v);
  renderTree(v);
  renderGroups(v);
  renderSilences(v);
  renderNotifs(v);
  renderEvents(v);
}

function renderSummary(v) {
  const s = v.stats, ver = v.verify;
  const box = $("summary");
  box.replaceChildren();
  const add = (label, val) => { const b = el("span"); b.append(label + " "); b.append(el("b", null, val)); box.append(b); };
  add("源事件", s.sourceEvents);
  add("告警实例", s.alertInstances);
  add("通知", s.notifications);
  add("压缩比", s.compression.toFixed(1) + "×");
  add("静默挡下", s.suppressedSilence);
  add("规则挡下", s.suppressedInhibit);
  add("重复送达", s.duplicates);
  add("被重复的通知", s.twiceDelivered);
  add("重试", s.retries);
  add("核对", ver.matched + "/" + ver.expected);
}

function renderBanner(v) {
  const b = $("banner");
  b.hidden = false;
  if (v.verify.ok) {
    b.className = "banner ok";
    b.textContent = "核对通过：应发 " + v.verify.expected + " 条，逐条对上 " + v.verify.matched +
      " 条；送达 " + v.verify.delivered + " 次，未送达 " + v.verify.undelivered +
      "，分区期间重复 " + v.verify.duplicateOk + " 次，分区外重复 " + v.verify.badDuplicates + " 次。";
    return;
  }
  b.className = "banner";
  b.textContent = "核对未通过（" + v.verify.problems.length + " 条）：\n" + v.verify.problems.slice(0, 12).join("\n");
}

function renderTree(v) {
  const root = $("tree");
  root.replaceChildren();
  const byParent = new Map();
  for (const n of v.tree) {
    const k = n.parent || "";
    if (!byParent.has(k)) byParent.set(k, []);
    byParent.get(k).push(n);
  }
  const build = (parent) => {
    const kids = byParent.get(parent) || [];
    const frag = document.createDocumentFragment();
    for (const n of kids) {
      const node = el("div", "node");
      if (n.windows && n.windows.length && !n.windowsActive) node.classList.add("gated");
      if (n.groups && n.groups.length) node.classList.add("matched");
      const head = el("div", "node-head");
      head.append(el("span", "name", n.name));
      head.append(el("span", "cond", n.match));
      head.append(el("span", "tag ch", "渠道 " + n.channelNow));
      if (n.groupBy) head.append(el("span", "tag", "按 " + n.groupBy));
      if (n.continueSiblings) head.append(el("span", "tag cont", "继续匹配兄弟"));
      for (const w of n.windows || []) {
        head.append(el("span", "tag " + (n.windowsActive ? "on" : "off"), w + (n.windowsActive ? " ✓" : " ×")));
      }
      for (const r of n.rules || []) head.append(el("span", "tag", "抑制 " + r));
      node.append(head);
      if (n.groups && n.groups.length) {
        const wrap = el("div", "groups-inline");
        for (const g of n.groups) wrap.append(miniGroup(g));
        node.append(wrap);
      }
      if (kids.length) node.append(build(n.id));
      frag.append(node);
    }
    return frag;
  };
  root.append(build(""));
}

function miniGroup(g) {
  const box = el("div", "mini-group");
  const head = el("div", "ghead");
  head.append(el("span", "values", g.values));
  head.append(el("span", "tag ch", g.channel));
  head.append(el("span", "tag", g.firing.length + " 条在发"));
  if (g.suppressed.length) head.append(el("span", "tag off", g.suppressed.length + " 条被挡"));
  if (g.pending) head.append(el("span", "tag off", "待发送"));
  box.append(head);
  if (g.firing.length) {
    const chips = el("div", "chips");
    for (const a of g.firing) chips.append(chip(a));
    box.append(chips);
  }
  return box;
}

function renderGroups(v) {
  const root = $("groups");
  root.replaceChildren();
  if (!v.groups.length) { root.append(el("div", "empty", "此刻没有分组。")); return; }
  for (const g of v.groups) {
    const card = el("div", "group-card");
    const head = el("div", "ghead");
    head.append(el("span", "path", g.nodeName));
    head.append(el("span", "values", g.values));
    head.append(el("span", "tag ch", g.channel));
    card.append(head);
    const meta = ["按 " + g.labelSet];
    if (g.lastSent) meta.push("上次发送 " + g.lastSent);
    if (g.holding) meta.push(g.holding + " 条待随下次通知发出");
    else if (g.pending) meta.push("有待发送的变化");
    card.append(el("div", "meta", meta.join(" · ")));
    if (g.firing.length) {
      const chips = el("div", "chips");
      for (const a of g.firing) chips.append(chip(a));
      card.append(chips);
    }
    if (g.suppressed.length) {
      const s = el("div", "supp");
      s.textContent = "被挡下：" + g.suppressed.map((x) => x.text + "（" + x.reason + "）").join("；");
      card.append(s);
    }
    root.append(card);
  }
}

function renderSilences(v) {
  const root = $("silences");
  root.replaceChildren();
  if (!v.silences.length) root.append(el("div", "empty", "本风暴没有静默。"));
  for (const s of v.silences) {
    const card = el("div", "silence-card" + (s.active ? " active" : ""));
    const head = el("div", "shead");
    head.append(el("span", "tag " + (s.active ? "on" : "off"), s.state));
    head.append(el("span", "cond", s.text));
    card.append(head);
    card.append(el("div", "times", hhmmss(s.starts) + " → " + hhmmss(s.ends) +
      (s.cancelled && s.cancelled !== "0001-01-01T00:00:00Z" ? "，已于 " + hhmmss(s.cancelled) + " 取消" : "")));
    if (s.reason) card.append(el("div", "reason", s.reason));
    if (s.alerts.length) {
      const chips = el("div", "chips");
      for (const a of s.alerts) chips.append(chip(a, "sup"));
      card.append(chips);
    }
    root.append(card);
  }

  const sup = $("suppressed");
  sup.replaceChildren();
  const rows = [];
  for (const g of v.groups) for (const s of g.suppressed) rows.push({ g, s });
  if (!rows.length) { sup.append(el("div", "empty", "此刻没有被抑制的告警。")); return; }
  for (const { g, s } of rows) {
    const row = el("div", "supp-row");
    row.append(el("span", "why", s.reason));
    row.append(el("span", "text", s.text));
    const why = s.reason === "静默" ? "静默 " + s.silenceId : "规则 " + s.rule + "（" + s.ruleText + "）";
    row.append(el("span", "rule", why));
    if (s.sources && s.sources.length) row.append(el("span", "rule", "源自 " + s.sources.length + " 条源告警"));
    row.append(el("span", "rule", g.nodeName + " / " + g.values));
    sup.append(row);
  }
}

function renderNotifs(v) {
  const root = $("notifications");
  root.replaceChildren();
  if (!v.notifs.length) { root.append(el("div", "empty", "此刻还没有发出通知。")); return; }
  const list = v.notifs.slice().reverse();
  for (const n of list) {
    const box = el("div", "notif");
    const head = el("div", "nhead");
    head.append(el("span", "time", hhmmss(n.at)));
    head.append(el("span", "node", n.nodeName));
    head.append(el("span", "values", n.group || "（全部）"));
    head.append(el("span", "tag ch", n.channel));
    head.append(el("span", "tag", "第 " + (n.ordinal + 1) + " 条"));
    if (n.baseline) head.append(el("span", "tag", "首条基线"));
    box.append(head);

    const delta = el("div", "delta");
    if (n.added.length) {
      const wrap = el("div");
      wrap.append(el("span", "lbl", "新增 " + n.added.length + "："));
      const chips = el("div", "chips");
      for (const a of n.added) chips.append(chip(a));
      wrap.append(chips);
      delta.append(wrap);
    }
    if (n.resolved.length) {
      const wrap = el("div");
      wrap.append(el("span", "lbl", "解除 " + n.resolved.length + "："));
      const chips = el("div", "chips");
      for (const a of n.resolved) chips.append(chip(a, "res"));
      wrap.append(chips);
      delta.append(wrap);
    }
    if (!n.added.length && !n.resolved.length) delta.append(el("span", "lbl", "无变化（仅状态通报）"));
    box.append(delta);

    if (n.suppressed.length) {
      const s = el("div", "delta");
      s.append(el("span", "lbl", "同时挡下 " + n.suppressed.length + " 条："));
      for (const x of n.suppressed) s.append(el("span", "chip sup", x.reason + " · " + x.text));
      box.append(s);
    }

    const d = el("div", "deliv");
    if (n.delivered) {
      d.append(el("span", "ok", "已送达"));
      if (n.replica) d.append(document.createTextNode(" 由 " + n.replica));
    } else {
      d.append(el("span", "", "尚未送达"));
    }
    d.append(document.createTextNode(" · 尝试 " + n.attempts + " 次"));
    if (n.duplicates) d.append(el("span", "dup", " · 分区期间重复 " + n.duplicates + " 次"));
    box.append(d);
    root.append(box);
  }
}

function renderEvents(v) {
  const root = $("events");
  root.replaceChildren();
  const evs = v.events.slice(-200).reverse();
  if (!evs.length) { root.append(el("div", "empty", "此刻没有事件。")); return; }
  for (const e of evs) {
    const row = el("div", "ev");
    row.append(el("span", "t", hhmmss(e.at)));
    row.append(el("span", "k " + e.kind, e.kind));
    row.append(el("span", "txt", e.text));
    root.append(row);
  }
}

function bind() {
  $("storm").addEventListener("change", async (e) => {
    state.storm = e.target.value;
    state.scrub = 1e9;
    await load();
  });
  $("scrub").addEventListener("input", async (e) => {
    state.scrub = Number(e.target.value);
    await load();
  });
  $("play").addEventListener("click", () => {
    state.playing = !state.playing;
    $("play").textContent = state.playing ? "暂停" : "播放";
    if (!state.playing) { clearInterval(state.timer); return; }
    state.timer = setInterval(async () => {
      const v = state.view;
      if (!v) return;
      state.scrub += Math.max(1, Math.round(v.length / 120));
      if (state.scrub > v.length) { state.scrub = 0; }
      await load();
    }, 120);
  });
}

bind();
load();
