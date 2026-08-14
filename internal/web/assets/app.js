// hivewire web UI: one SSE stream in, four live panes out.
'use strict';

const state = {
  agents: new Map(),   // id -> agent
  buffers: new Map(),  // id -> events[]
  slots: [],
  pending: [],
  expanded: new Set(), // event seqs the user opened
};

const COLLAPSE_LINES = 40; // bodies longer than this start folded

// ---------------------------------------------------------------- utilities

const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
};

const clock = (iso) => {
  const d = new Date(iso);
  return isNaN(d) ? '' : d.toTimeString().slice(0, 8);
};

const tokens = (t) => {
  if (!t || !t.total) return '';
  const k = (n) => (n >= 1000 ? (n / 1000).toFixed(1) + 'k' : String(n));
  let s = k(t.total) + ' tok';
  // Only Codex reports the model's context window; say so rather than implying
  // a full window or an empty one.
  s += t.context_window
    ? ` (${Math.round((t.total / t.context_window) * 100)}% ctx)`
    : ' (ctx --)';
  return s;
};

// nesting describes where an agent sits in the spawn tree: depth alone for a
// subagent of a session, and the spawning agent's name when it was nested by
// another subagent.
const nesting = (a) => {
  if (!a.depth) return '';
  return a.parentLabel ? `d${a.depth} · nested in ${a.parentLabel}` : 'd' + a.depth;
};

const elapsed = (a) => {
  const start = new Date(a.started).getTime();
  const end = a.status === 'live' ? Date.now() : new Date(a.updated).getTime();
  if (!start || !end || end < start) return '';
  const s = Math.round((end - start) / 1000);
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${String(s % 60).padStart(2, '0')}s`;
};

// ------------------------------------------------------------------ layout
// Three gutters give VS Code-style resizing: one vertical split between the
// columns, and an independent horizontal split inside each column.

const LAYOUT_KEY = 'hivewire.layout';
const DEFAULT_LAYOUT = { 'col-left': 50, 'row-left': 50, 'row-right': 50 };

function applyLayout(l) {
  const root = document.documentElement.style;
  root.setProperty('--col-left', l['col-left'] + '%');
  root.setProperty('--row-left', l['row-left'] + '%');
  root.setProperty('--row-right', l['row-right'] + '%');
}

function loadLayout() {
  try {
    return Object.assign({}, DEFAULT_LAYOUT, JSON.parse(localStorage.getItem(LAYOUT_KEY) || '{}'));
  } catch {
    return Object.assign({}, DEFAULT_LAYOUT);
  }
}

let layout = loadLayout();
applyLayout(layout);

function initGutters() {
  const grid = document.getElementById('grid');
  document.querySelectorAll('.gutter').forEach((g) => {
    g.addEventListener('pointerdown', (ev) => {
      ev.preventDefault();
      g.setPointerCapture(ev.pointerId);
      g.classList.add('dragging');

      const which = g.dataset.gutter;
      const move = (e) => {
        let pct;
        if (which === 'main') {
          const r = grid.getBoundingClientRect();
          pct = ((e.clientX - r.left) / r.width) * 100;
          layout['col-left'] = clamp(pct);
        } else {
          const col = g.parentElement.getBoundingClientRect();
          pct = ((e.clientY - col.top) / col.height) * 100;
          layout[which === 'left' ? 'row-left' : 'row-right'] = clamp(pct);
        }
        applyLayout(layout);
      };
      const up = () => {
        g.classList.remove('dragging');
        g.removeEventListener('pointermove', move);
        g.removeEventListener('pointerup', up);
        localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout));
      };
      g.addEventListener('pointermove', move);
      g.addEventListener('pointerup', up);
    });
  });
}

const clamp = (pct) => Math.min(88, Math.max(12, pct));

// -------------------------------------------------------------------- ansi
// Tool and shell output is often colour-coded (make, kubectl, colored test
// runners, gspcli, …). Bodies keep those raw SGR escape codes verbatim, like
// everything else hivewire never touches, so this turns them into <span>s
// instead of the browser showing the escape bytes as literal text.

const hasANSI = (s) => !!s && s.indexOf('\x1b[') !== -1;

// A GitHub Dark terminal palette, so ANSI colours read consistently with the
// rest of hivewire's own (identically GitHub-Dark-derived) accent colours.
const ANSI_FG = ['#484f58', '#ff7b72', '#3fb950', '#d29922', '#58a6ff', '#bc8cff', '#39c5cf', '#b1bac4'];
const ANSI_BRIGHT = ['#6e7681', '#ffa198', '#56d364', '#e3b341', '#79c0ff', '#d2a8ff', '#56d4dd', '#f0f6fc'];

function ansi256(n) {
  if (n < 8) return ANSI_FG[n];
  if (n < 16) return ANSI_BRIGHT[n - 8];
  if (n < 232) {
    n -= 16;
    const lvl = (v) => (v === 0 ? 0 : v * 40 + 55);
    return `rgb(${lvl(Math.floor(n / 36))},${lvl(Math.floor((n / 6) % 6))},${lvl(n % 6)})`;
  }
  const v = (n - 232) * 10 + 8;
  return `rgb(${v},${v},${v})`;
}

// extendedColor reads the 256-colour (38;5;N) or truecolor (38;2;R;G;B) form
// that can follow a 38/48 code, returning the CSS colour and how many extra
// codes it consumed.
function extendedColor(codes, i) {
  if (codes[i] === 5 && codes[i + 1] !== undefined) return [ansi256(codes[i + 1]), 2];
  if (codes[i] === 2 && codes[i + 3] !== undefined) return [`rgb(${codes[i + 1]},${codes[i + 2]},${codes[i + 3]})`, 4];
  return [null, 0];
}

function applySGR(s, codes) {
  for (let i = 0; i < codes.length; i++) {
    const c = codes[i];
    if (c === 0) Object.keys(s).forEach((k) => delete s[k]);
    else if (c === 1) s.bold = true;
    else if (c === 2) s.dim = true;
    else if (c === 3) s.italic = true;
    else if (c === 4) s.underline = true;
    else if (c === 7) s.inverse = true;
    else if (c === 9) s.strike = true;
    else if (c === 22) { delete s.bold; delete s.dim; }
    else if (c === 23) delete s.italic;
    else if (c === 24) delete s.underline;
    else if (c === 27) delete s.inverse;
    else if (c === 29) delete s.strike;
    else if (c >= 30 && c <= 37) s.fg = ANSI_FG[c - 30];
    else if (c === 38) { const [color, n] = extendedColor(codes, i + 1); if (color) s.fg = color; i += n; }
    else if (c === 39) delete s.fg;
    else if (c >= 40 && c <= 47) s.bg = ANSI_FG[c - 40];
    else if (c === 48) { const [color, n] = extendedColor(codes, i + 1); if (color) s.bg = color; i += n; }
    else if (c === 49) delete s.bg;
    else if (c >= 90 && c <= 97) s.fg = ANSI_BRIGHT[c - 90];
    else if (c >= 100 && c <= 107) s.bg = ANSI_BRIGHT[c - 100];
  }
}

function sgrStyle(s) {
  let fg = s.fg, bg = s.bg;
  if (s.inverse) [fg, bg] = [bg || 'var(--bg)', fg || 'var(--fg)'];
  const decl = [];
  if (fg) decl.push(`color:${fg}`);
  if (bg) decl.push(`background:${bg}`);
  if (s.bold) decl.push('font-weight:700');
  if (s.dim) decl.push('opacity:.65');
  if (s.italic) decl.push('font-style:italic');
  if (s.underline && s.strike) decl.push('text-decoration:underline line-through');
  else if (s.underline) decl.push('text-decoration:underline');
  else if (s.strike) decl.push('text-decoration:line-through');
  return decl.join(';');
}

const escapeHTML = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');

// ANSI_RE matches an SGR sequence (captured) or any other CSI/OSC escape,
// which is dropped rather than shown — cursor moves and the like mean
// nothing once stdout has already been captured into a static transcript.
const ANSI_RE = /\x1b\[([0-9;]*)m|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-9;?]*[A-Za-z]/g;

// ansiToHTML converts a string carrying raw SGR escape codes into HTML with
// equivalent <span> runs. Only used when hasANSI already found an escape
// byte, so plain output keeps the cheaper textContent path.
function ansiToHTML(text) {
  let out = '';
  let cur = {};
  let last = 0;
  const flush = (chunk) => {
    if (!chunk) return;
    const style = sgrStyle(cur);
    out += style ? `<span style="${style}">${escapeHTML(chunk)}</span>` : escapeHTML(chunk);
  };

  ANSI_RE.lastIndex = 0;
  let m;
  while ((m = ANSI_RE.exec(text))) {
    flush(text.slice(last, m.index));
    last = ANSI_RE.lastIndex;
    if (m[1] !== undefined) applySGR(cur, m[1] === '' ? [0] : m[1].split(';').map(Number));
  }
  flush(text.slice(last));
  return out;
}

// setBody fills a <pre> with text, rendering ANSI colour codes as HTML when
// present instead of the safe-but-plain textContent path.
function setBody(pre, text) {
  if (hasANSI(text)) pre.innerHTML = ansiToHTML(text);
  else pre.textContent = text;
}

// ------------------------------------------------------------------ render

function renderBar() {
  const agents = [...state.agents.values()];
  const live = agents.filter((a) => a.status === 'live').length;
  document.getElementById('counts').textContent =
    `${live} live · ${agents.length} seen`;

  const p = document.getElementById('pending');
  if (state.pending.length) {
    p.classList.remove('hidden');
    p.textContent = `${state.pending.length} waiting for a slot`;
  } else {
    p.classList.add('hidden');
  }
}

function renderPane(slotIdx) {
  const pane = document.querySelector(`.pane[data-slot="${slotIdx}"]`);
  if (!pane) return;
  const id = state.slots[slotIdx];
  const agent = id ? state.agents.get(id) : null;

  const body = pane.querySelector('.pane-body');
  const atBottom = !body || body.scrollHeight - body.scrollTop - body.clientHeight < 40;

  pane.className = 'pane ' + (agent ? agent.status : '');
  pane.innerHTML = '';

  const head = el('div', 'pane-head');
  if (!agent) {
    head.appendChild(el('span', 'muted', `slot ${slotIdx + 1} — idle`));
    pane.appendChild(head);
    pane.appendChild(el('div', 'pane-empty', 'waiting for a subagent…'));
    return;
  }

  head.appendChild(el('span', 'status-chip', (agent.status || 'idle').toUpperCase()));
  head.appendChild(el('span', 'head-provider', `${agent.provider} · ${agent.model || '?'}`));
  head.appendChild(el('span', 'head-title', agentLabel(agent)));
  if (agent.title) head.appendChild(el('span', 'head-meta', `"${agent.title}"`));

  const meta = [];
  const nest = nesting(agent);
  if (nest) meta.push(nest);
  const tk = tokens(agent.tokens);
  if (tk) meta.push(tk);
  if (agent.toolCount) meta.push(agent.toolCount + ' tools');
  const el2 = elapsed(agent);
  if (el2) meta.push(el2);
  if (agent.sandbox) meta.push(agent.sandbox);
  if (agent.approval) meta.push('approval:' + agent.approval);
  if (agent.effort) meta.push('effort:' + agent.effort);
  if (agent.gitBranch) meta.push('⎇ ' + agent.gitBranch);
  if (agent.cwd) meta.push(agent.cwd);
  head.appendChild(el('span', 'head-meta', meta.join(' · ')));

  if (agent.dropped) {
    head.appendChild(el('span', 'head-warn',
      `⚠ ring buffer wrapped — ${agent.dropped} events dropped`));
  }
  pane.appendChild(head);

  const out = el('div', 'pane-body');
  const events = state.buffers.get(id) || [];
  if (!events.length) out.appendChild(el('div', 'pane-empty', 'no output yet…'));
  for (const e of events) out.appendChild(renderEvent(e));
  pane.appendChild(out);

  if (atBottom) out.scrollTop = out.scrollHeight;
}

function agentLabel(a) {
  if (a.nickname && a.name) return `${a.name} (${a.nickname})`;
  return a.name || a.id;
}

function renderEvent(e) {
  const wrap = el('div', `ev ${e.kind}${e.err ? ' err' : ''}`);

  const collapsible = (e.lines || 0) > COLLAPSE_LINES || e.kind === 'tool_use';
  const open = state.expanded.has(e.seq) || (!collapsible && !!e.body);

  const head = el('div', 'ev-head');
  head.appendChild(el('span', 'ts', clock(e.ts) + ' '));
  if (collapsible) head.appendChild(el('span', 'chev', (open ? '▾ ' : '▸ ')));
  head.appendChild(el('span', 'kindtag', tag(e) + ' '));
  if (hasANSI(e.header)) {
    const h = el('span');
    h.innerHTML = ansiToHTML(e.header);
    head.appendChild(h);
  } else {
    head.appendChild(document.createTextNode(e.header || ''));
  }
  if ((e.lines || 0) > 1) head.appendChild(el('span', 'lines', `  (${e.lines} lines)`));
  head.addEventListener('click', () => {
    if (state.expanded.has(e.seq)) state.expanded.delete(e.seq);
    else state.expanded.add(e.seq);
    const next = renderEvent(e);
    wrap.replaceWith(next);
  });
  wrap.appendChild(head);

  if (e.overflow) {
    const note = el('div', 'overflow-note');
    note.appendChild(el('span', null, `⚠ ${e.overflow.note}`));
    const btn = el('button', null, 'load full output');
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      btn.textContent = 'loading…';
      try {
        const r = await fetch('/api/overflow?path=' + encodeURIComponent(e.overflow.path));
        const text = await r.text();
        const pre = el('pre', 'body');
        setBody(pre, text);
        note.after(pre);
        btn.remove();
      } catch (err) {
        btn.textContent = 'failed: ' + err;
      }
    });
    note.appendChild(btn);
    wrap.appendChild(note);
  }

  if (open && e.body) {
    const pre = el('pre', 'body');
    setBody(pre, e.body);
    wrap.appendChild(pre);
  }
  return wrap;
}

const TAGS = {
  tool_use: '⚙',
  tool_result: '⮑',
  reasoning: '✻',
  user: '›',
  text: '·',
  status: '◆',
  notice: '!',
};
const tag = (e) => (e.tool ? TAGS[e.kind] + ' ' : TAGS[e.kind] || '·');

function renderAll() {
  renderBar();
  for (let i = 0; i < 4; i++) renderPane(i);
}

// ------------------------------------------------------------------ stream

function connect() {
  const conn = document.getElementById('conn');
  const es = new EventSource('/api/stream');

  es.onopen = () => {
    conn.textContent = 'live';
    conn.className = 'pill ok';
  };
  es.onerror = () => {
    conn.textContent = 'reconnecting…';
    conn.className = 'pill bad';
  };
  es.onmessage = (msg) => {
    let f;
    try {
      f = JSON.parse(msg.data);
    } catch {
      return;
    }
    switch (f.type) {
      case 'snapshot':
        state.agents.clear();
        state.buffers.clear();
        (f.agents || []).forEach((a) => state.agents.set(a.id, a));
        Object.entries(f.buffers || {}).forEach(([id, evs]) => state.buffers.set(id, evs));
        state.slots = f.slots || [];
        state.pending = f.pending || [];
        renderAll();
        break;
      case 'agent':
        state.agents.set(f.agent.id, f.agent);
        renderBar();
        paintAgent(f.agent.id);
        break;
      case 'events': {
        const touched = new Set();
        for (const e of f.events || []) {
          if (!state.buffers.has(e.agentId)) state.buffers.set(e.agentId, []);
          state.buffers.get(e.agentId).push(e);
          touched.add(e.agentId);
        }
        touched.forEach(paintAgent);
        break;
      }
      case 'slots':
        state.slots = f.slots || [];
        state.pending = f.pending || [];
        renderAll();
        break;
    }
  };
}

function paintAgent(id) {
  const idx = state.slots.indexOf(id);
  if (idx >= 0) renderPane(idx);
}

// ----------------------------------------------------------------- history
// The search box queries the server, so it matches every indexed run rather
// than only the page currently rendered.

const PAGE = 50;
const history = { q: '', offset: 0, total: 0 };

async function loadHistory(append) {
  const list = document.getElementById('history-list');
  const count = document.getElementById('history-count');
  const more = document.getElementById('history-more');
  if (!append) {
    history.offset = 0;
    list.innerHTML = '';
  }

  const url = `/api/history?limit=${PAGE}&offset=${history.offset}&q=${encodeURIComponent(history.q)}`;
  const data = await (await fetch(url)).json();
  history.total = data.total;

  for (const r of data.records || []) list.appendChild(historyRow(r));
  history.offset += (data.records || []).length;

  const shown = list.children.length;
  count.textContent = history.q
    ? `${history.total} match${history.total === 1 ? '' : 'es'} · showing ${shown}`
    : `${history.total} run${history.total === 1 ? '' : 's'} · showing ${shown}`;
  more.classList.toggle('hidden', shown >= history.total);
  more.textContent = `view more (${history.total - shown} left)`;
}

function historyRow(r) {
  const li = el('li', 'st-' + (r.status || 'done'));
  li.appendChild(el('div', null, `${r.provider} · ${r.name || r.id}`));
  const sub = [new Date(r.started).toLocaleString(), r.model];
  const nest = nesting(r);
  if (nest) sub.push(nest);
  if (r.title) sub.push('"' + r.title + '"');
  li.appendChild(el('div', 'muted', sub.filter(Boolean).join(' · ')));
  if (r.prompt) li.appendChild(el('div', 'muted', clip(r.prompt, 110)));

  li.addEventListener('click', async () => {
    document.querySelectorAll('#history-list li.sel').forEach((n) => n.classList.remove('sel'));
    li.classList.add('sel');
    const replay = document.getElementById('replay');
    replay.innerHTML = '<div class="muted">replaying…</div>';
    const res = await fetch('/api/replay?id=' + encodeURIComponent(r.id));
    if (!res.ok) {
      replay.innerHTML = '<div class="muted">replay failed: ' + (await res.text()) + '</div>';
      return;
    }
    const data = await res.json();
    replay.innerHTML = '';
    replay.appendChild(el('div', 'muted', data.agent.source));
    for (const e of data.events || []) replay.appendChild(renderEvent(e));
  });
  return li;
}

const clip = (s, n) => (s.length > n ? s.slice(0, n) + '…' : s).replace(/\s+/g, ' ');

function openHistory() {
  document.getElementById('drawer').classList.remove('hidden');
  document.getElementById('replay').innerHTML = '<div class="muted">select a run to replay</div>';
  const box = document.getElementById('history-search');
  loadHistory(false).then(() => box.focus());
}

let searchTimer;
document.getElementById('history-search').addEventListener('input', (e) => {
  history.q = e.target.value.trim();
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => loadHistory(false), 120);
});
document.getElementById('history-more').addEventListener('click', () => loadHistory(true));

// -------------------------------------------------------------------- init

document.getElementById('btn-history').addEventListener('click', openHistory);
document.getElementById('btn-close').addEventListener('click', () =>
  document.getElementById('drawer').classList.add('hidden'));
document.getElementById('btn-reset').addEventListener('click', () => {
  layout = Object.assign({}, DEFAULT_LAYOUT);
  applyLayout(layout);
  localStorage.setItem(LAYOUT_KEY, JSON.stringify(layout));
});

initGutters();
renderAll();
connect();
setInterval(renderBar, 1000); // keeps elapsed times honest while idle
