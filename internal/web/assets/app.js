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
  if (t.context_window) s += ` (${Math.round((t.total / t.context_window) * 100)}% ctx)`;
  return s;
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
  if (agent.depth) meta.push('d' + agent.depth);
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
  head.appendChild(document.createTextNode(e.header || ''));
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
        const pre = el('pre', 'body', text);
        note.after(pre);
        btn.remove();
      } catch (err) {
        btn.textContent = 'failed: ' + err;
      }
    });
    note.appendChild(btn);
    wrap.appendChild(note);
  }

  if (open && e.body) wrap.appendChild(el('pre', 'body', e.body));
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
