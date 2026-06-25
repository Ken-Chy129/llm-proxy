let logPage = 0;
const logLimit = 30;
let statusBooted = false;

function apiFetch(url, opts) {
  return fetch(url, opts);
}

async function loadStatus() {
  const r = await apiFetch('/api/status');
  if (r.status === 401) { window.location.href = '/login'; return; }
  const d = await r.json();
  document.getElementById('total-requests').textContent = (d.total_requests || 0).toLocaleString();
  document.getElementById('total-tokens').textContent = (d.total_tokens || 0).toLocaleString();

  const el = document.getElementById('backends');
  // Entrance animation plays once; later refreshes (e.g. on window focus) skip
  // it so the cards don't visibly flash/re-fade on every re-render.
  if (statusBooted) el.classList.add('no-anim');
  statusBooted = true;
  el.innerHTML = d.backends.map(b => {
    const bc = b.status === 'active' ? 'badge-active' : b.status === 'expired' ? 'badge-expired' : 'badge-inactive';
    const bl = b.status === 'active' ? 'Active' : b.status === 'expired' ? 'Expired' : 'Offline';
    const dc = b.status === 'active' ? 'dot-green' : b.status === 'expired' ? 'dot-yellow' : 'dot-gray';
    const isOAuth = b.name === 'claude' || b.name === 'codex';
    let accts = '';
    if (b.accounts && b.accounts.length) {
      accts = b.accounts.map(a => {
        // Operational dot: gray=paused, red=rate-limited, green=usable. An
        // expired OAuth access token is NOT flagged — it auto-refreshes on next
        // use, so surfacing it as a warning would be noise.
        let dotClass = 'dot-green', dotStyle = '', title = 'Active';
        if (a.disabled) { dotClass = 'dot-gray'; title = 'Paused'; }
        else if (a.rate_limited) {
          dotClass = ''; dotStyle = 'background:var(--red)';
          title = a.rate_limited_estimated ? 'Rate-limited upstream — no reset time, re-checking periodically' : 'Rate-limited upstream until ' + a.rate_limited_until;
        } else {
          title = a.token_expired ? 'Active — access token refreshes on next use' : (a.expires ? 'Active — access token valid until ' + a.expires : 'Active');
        }
        const toggleAccBtn = `<button class="btn-delete" style="font-size:10px;color:${a.disabled ? 'var(--green)' : 'var(--yellow)'}" title="${a.disabled ? 'Resume' : 'Pause'}" onclick="toggleAccount('${b.name}','${a.id}')">${a.disabled ? '▶' : '⏸'}</button>`;
        const rlBadge = a.rate_limited
          ? `<span class="exp" style="color:var(--red)" title="${a.rate_limited_estimated ? 'Rate-limited upstream — no reset time provided, re-checking periodically' : 'Rate-limited upstream until ' + a.rate_limited_until}">limited${a.rate_limited_estimated ? '' : ' · until ' + a.rate_limited_until}</span>`
          : '';
        return `<div class="account-row" style="${a.disabled ? 'opacity:0.4' : ''}"><span class="dot ${dotClass}" style="${dotStyle}" title="${title}"></span><span class="email">${a.email}</span>`
          + rlBadge
          + toggleAccBtn
          + `<button class="btn-delete" title="Remove" onclick="removeAccount('${b.name}','${a.id}')">&times;</button></div>`;
      }).join('');
    }
    const isVertex = b.name === 'vertex';
    let addBtn = '';
    if (isOAuth) {
      addBtn = `<button class="btn-add" onclick="openAddAccount('${b.name}')"><span>+</span> Add Account</button>`;
    } else if (isVertex) {
      addBtn = `<button class="btn-add" onclick="openVertexModal()"><span>+</span> ${b.status === 'active' ? 'Update' : 'Add'} Credentials</button>`;
      if (b.credential_source === 'uploaded') {
        addBtn += `<button class="btn-add" style="margin-left:4px;color:var(--red);border-color:var(--red)" onclick="removeVertexCredentials()">Remove</button>`;
      }
    }
    const syncBtn = isOAuth && b.status === 'active' ? `<button class="btn-add" style="margin-left:4px" onclick="syncModels()">Sync</button>` : '';
    const toggleBtn = `<button class="btn-add" style="${b.disabled ? 'color:var(--green);border-color:var(--green)' : 'color:var(--yellow);border-color:var(--yellow)'}" onclick="toggleBackend('${b.name}')">${b.disabled ? 'Enable' : 'Pause'}</button>`;
    return `<div class="backend-card" style="${b.disabled ? 'opacity:0.5' : ''}"><div class="backend-header"><span class="dot ${dc}"></span><span class="backend-name" style="text-transform:capitalize">${b.name}</span><span class="backend-badge ${bc}">${bl}</span></div>`
      + `<div class="backend-info">${b.info || ''}</div>`
      + `<div class="backend-models">${(b.models || []).map(m => `<span class="model-tag">${m}</span>`).join('')}</div>`
      + accts + `<div style="display:flex;gap:4px;flex-wrap:wrap">${addBtn}${syncBtn}${toggleBtn}</div></div>`;
  }).join('');

  // Render per-account quota cards
  let allQuotas = [];
  d.backends.forEach(b => { if (b.quotas) allQuotas = allQuotas.concat(b.quotas); });
  const qSection = document.getElementById('quota-section');
  const qGrid = document.getElementById('quota-grid');
  if (allQuotas.length) {
    qSection.style.display = '';
    qGrid.innerHTML = allQuotas.map(q => {
      const planCls = q.plan_type?.toLowerCase().includes('pro') ? 'plan-pro' : q.plan_type?.toLowerCase().includes('plus') ? 'plan-plus' : 'plan-team';
      const planLabel = q.plan_type || 'Unknown';
      const displayName = q.email || q.account_id;
      const renderRow = (w) => {
        if (!w) return '';
        const pct = Math.round(w.remaining_percent || 0);
        const barColor = w.limit_reached ? 'var(--red)' : pct < 20 ? 'var(--yellow)' : 'var(--green)';
        return `<div class="quota-row"><div class="quota-row-header"><span class="quota-row-label">${w.label}</span><span class="quota-row-value"><span class="pct">${pct}%</span>${w.reset_at || ''}</span></div><div class="quota-bar"><div class="quota-bar-fill" style="width:${Math.min(pct, 100)}%;background:${barColor}"></div></div></div>`;
      };
      let rows = '';
      if (q.has_real_data) {
        rows = renderRow(q.primary) + renderRow(q.secondary);
        if (q.additional) { q.additional.forEach(a => { if (a.primary) rows += renderRow(a.primary); }); }
      } else {
        rows = `<div style="font-size:12px;color:var(--text-2);padding:4px 0">No quota data yet — click <span style="color:var(--accent);cursor:pointer" onclick="refreshQuota('${q.account_id}')">&#8635; refresh</span> to fetch</div>`;
      }
      const refreshBtn = `<button class="btn-delete" style="font-size:11px;color:var(--accent)" onclick="refreshQuota('${q.account_id}')">&#8635;</button>`;
      const fetchedAt = q.fetched_at ? `<span style="font-size:10px;color:var(--text-2);margin-left:auto">cached ${q.fetched_at}</span>` : '';
      return `<div class="quota-card" data-account="${q.account_id}"><div class="quota-card-header"><span class="model-tag" style="background:var(--accent-dim);color:var(--text-0)">Codex</span><span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${displayName}</span>${refreshBtn}</div><div style="display:flex;align-items:center;gap:6px;margin-bottom:8px"><span class="plan-badge ${planCls}">${planLabel}</span>${fetchedAt}</div>${rows}</div>`;
    }).join('');
  } else {
    qSection.style.display = 'none';
  }

  const sel = document.getElementById('chat-model');
  const prevModel = sel.value;
  const statusIcon = s => s === 'active' ? '✓' : s === 'expired' ? '!' : '✗';
  sel.innerHTML = d.backends.map(b => {
    const lbl = b.name.charAt(0).toUpperCase() + b.name.slice(1) + ' (' + statusIcon(b.status) + ')';
    return `<optgroup label="${lbl}">${(b.models || []).map(m =>
      `<option value="${m}"${b.status !== 'active' ? ' disabled' : ''}>${m}</option>`
    ).join('')}</optgroup>`;
  }).join('');
  const prev = prevModel && sel.querySelector(`option[value="${prevModel}"]:not([disabled])`);
  if (prev) prev.selected = true;
  else { const first = sel.querySelector('option:not([disabled])'); if (first) first.selected = true; }
}

async function sendChat() {
  const model = document.getElementById('chat-model').value;
  const input = document.getElementById('chat-input').value.trim();
  if (!input) return;
  const output = document.getElementById('chat-output');
  const sendBtn = document.querySelector('.btn-primary');
  output.textContent = '';
  output.classList.add('loading');
  sendBtn.disabled = true; sendBtn.textContent = 'Sending...';
  try {
    const resp = await apiFetch('/v1/chat/completions', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model, messages: [{ role: 'user', content: input }], stream: true })
    });
    if (!resp.ok) { const e = await resp.json(); output.classList.remove('loading'); output.textContent = 'Error: ' + (e.error?.message || resp.statusText); return; }
    output.classList.remove('loading');
    const reader = resp.body.getReader(); const dec = new TextDecoder(); let buf = '';
    while (true) {
      const { done, value } = await reader.read(); if (done) break;
      buf += dec.decode(value, { stream: true });
      const lines = buf.split('\n'); buf = lines.pop();
      for (const line of lines) {
        if (!line.startsWith('data: ') || line === 'data: [DONE]') continue;
        try {
          const c = JSON.parse(line.slice(6));
          if (c.error) { output.textContent = 'Error: ' + (c.error.message || JSON.stringify(c.error)); return; }
          const t = c.choices?.[0]?.delta?.content; if (t) output.textContent += t;
        } catch {}
      }
    }
  } catch (e) { output.classList.remove('loading'); output.textContent = 'Error: ' + e.message; }
  finally { sendBtn.disabled = false; sendBtn.textContent = 'Send'; loadStatus(); }
}

function clearChat() {
  document.getElementById('chat-output').textContent = '';
  document.getElementById('chat-input').value = '';
}

async function loadLogs() {
  const r = await apiFetch('/api/logs?limit=' + logLimit + '&offset=' + (logPage * logLimit));
  const d = await r.json();
  document.getElementById('log-total').textContent = d.total;
  document.getElementById('page-info').textContent = (logPage + 1) + ' / ' + Math.max(1, Math.ceil(d.total / logLimit));
  document.getElementById('log-body').innerHTML = (d.logs || []).map(l => {
    const t = new Date(l.time).toLocaleString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', day: '2-digit', month: 'short' });
    const tok = (l.prompt_tokens || 0) + (l.completion_tokens || 0);
    const sc = l.status < 400 ? 'text-green' : 'text-red';
    const keyTag = l.api_key_name ? '<span style="font-size:10px;color:var(--accent);margin-left:4px">[' + l.api_key_name + ']</span>' : '';
    const acct = l.account || '-';
    const foTag = l.failover_from
      ? ` <span style="color:var(--yellow);font-size:10px;cursor:help" title="failed over from: ${l.failover_from}">↩</span>`
      : '';
    const errRow = l.error ? `<tr><td colspan="7" style="padding:2px 12px 8px;font-size:11px;color:var(--red);border:none">${l.error}</td></tr>` : '';
    return `<tr><td class="text-muted text-mono">${t}</td><td class="text-mono">${l.model}${keyTag}</td><td class="text-muted">${l.backend}</td><td class="text-muted text-mono" style="font-size:11px" title="${acct}${l.failover_from ? ' (failover from ' + l.failover_from + ')' : ''}">${acct}${foTag}</td><td>${l.latency_ms}ms</td><td>${tok}</td><td class="${sc}">${l.status}</td></tr>${errRow}`;
  }).join('') || '<tr><td colspan="6" class="text-muted" style="text-align:center;padding:24px">No requests yet</td></tr>';
}

function prevPage() { if (logPage > 0) { logPage--; loadLogs(); } }
function nextPage() { logPage++; loadLogs(); }

let statsRange = '7d', statsMetric = 'requests', statsDim = 'model', statsData = null;

function setActive(groupId, btn) {
  document.querySelectorAll('#' + groupId + ' button').forEach(b => b.classList.remove('active'));
  if (btn) btn.classList.add('active');
}

async function loadStats(range, btn) {
  if (range) statsRange = range;
  if (btn) setActive('stats-range', btn);
  const tz = -new Date().getTimezoneOffset(); // minutes east of UTC
  const r = await apiFetch('/api/stats?range=' + statsRange + '&tz=' + tz);
  if (r.status === 401) { window.location.href = '/login'; return; }
  statsData = await r.json();
  renderCalendar();
  renderTrend();
  renderBreakdown();
}

function setRange(range, btn) { loadStats(range, btn); }
function setMetric(metric, btn) { statsMetric = metric; setActive('metric-seg', btn); renderCalendar(); renderTrend(); renderBreakdown(); }
function setDimension(dim, btn) { statsDim = dim; setActive('dim-seg', btn); renderBreakdown(); }

// --- axis helpers (local time, matching the tz-shifted SQLite bucket keys) ---
const pad2 = n => String(n).padStart(2, '0');
const dayKey = d => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
const hourKey = d => `${dayKey(d)}T${pad2(d.getHours())}:00`;

function buildAxis(series) {
  const map = {};
  (series || []).forEach(s => { map[s.bucket] = s; });
  const zero = b => map[b] || { bucket: b, request_count: 0, prompt_tokens: 0, completion_tokens: 0, error_count: 0 };
  let keys = [];
  if (statsRange === 'all') {
    keys = (series || []).map(s => s.bucket); // unbounded: plot returned buckets as-is
  } else if (statsData && statsData.granularity === 'hour') {
    const now = new Date(); now.setMinutes(0, 0, 0);
    for (let i = 23; i >= 0; i--) keys.push(hourKey(new Date(now.getTime() - i * 3600e3)));
  } else {
    const days = statsRange === '30d' ? 30 : 7;
    const now = new Date();
    for (let i = days - 1; i >= 0; i--) keys.push(dayKey(new Date(now.getTime() - i * 86400e3)));
  }
  return keys.map(zero);
}

const metricVal = p => statsMetric === 'tokens' ? (p.prompt_tokens + p.completion_tokens) : p.request_count;
const xLabel = b => statsData && statsData.granularity === 'hour' ? b.slice(11, 16) : b.slice(5);

// GitHub-style contribution heatmap over ~53 weeks, colored by the active metric.
function renderCalendar() {
  if (!statsData) return;
  const map = {};
  (statsData.calendar || []).forEach(c => { map[c.bucket] = c; });
  const valOf = c => statsMetric === 'tokens' ? (c.prompt_tokens + c.completion_tokens) : c.request_count;

  const cell = 11, gap = 3, stride = cell + gap, topPad = 18, leftPad = 26, WEEKS = 53;
  const today = new Date(); today.setHours(0, 0, 0, 0);
  const end = new Date(today); end.setDate(end.getDate() + (6 - end.getDay()));   // Saturday of this week
  const start = new Date(end); start.setDate(start.getDate() - (WEEKS * 7 - 1));  // a Sunday

  let max = 0;
  const days = [];
  for (let d = new Date(start); d <= end; d.setDate(d.getDate() + 1)) {
    const k = dayKey(new Date(d));
    const c = map[k];
    const v = c ? valOf(c) : 0;
    if (v > max) max = v;
    days.push({ d: new Date(d), k, v, err: c ? c.error_count : 0 });
  }
  const lvl = v => v <= 0 ? 0 : max <= 0 ? 0 : v <= max * 0.25 ? 1 : v <= max * 0.5 ? 2 : v <= max * 0.75 ? 3 : 4;

  const W = leftPad + WEEKS * stride + 4, H = topPad + 7 * stride + 2;
  const MN = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  let cells = '', months = '', prevMonth = -1;
  days.forEach((day, i) => {
    const wk = Math.floor(i / 7), dow = i % 7;
    const x = leftPad + wk * stride, y = topPad + dow * stride;
    const future = day.d > today;
    const cls = future ? 'cal-future' : 'cal-l' + lvl(day.v);
    const title = future ? '' : `<title>${day.k}: ${fmtCompact(day.v)} ${statsMetric}${day.err ? ' · ' + day.err + ' err' : ''}</title>`;
    cells += `<rect x="${x}" y="${y}" width="${cell}" height="${cell}" rx="2" class="${cls}">${title}</rect>`;
    if (dow === 0) {
      const m = day.d.getMonth();
      if (m !== prevMonth && day.d.getDate() <= 7) { months += `<text x="${x}" y="11" class="cal-axis">${MN[m]}</text>`; prevMonth = m; }
    }
  });
  const wd = [[1, 'Mon'], [3, 'Wed'], [5, 'Fri']]
    .map(([r, t]) => `<text x="0" y="${topPad + r * stride + cell - 1}" class="cal-axis">${t}</text>`).join('');
  document.getElementById('calendar').innerHTML =
    `<svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" class="cal-svg">${months}${wd}${cells}</svg>`;
}

function renderTrend() {
  if (!statsData) return;
  document.getElementById('trend-title').textContent = (statsMetric === 'tokens' ? 'Tokens' : 'Requests') + ' over time';
  // range-scoped readouts
  const s = statsData.series || [];
  const reqs = s.reduce((a, p) => a + p.request_count, 0);
  const toks = s.reduce((a, p) => a + p.prompt_tokens + p.completion_tokens, 0);
  const errs = s.reduce((a, p) => a + p.error_count, 0);
  document.getElementById('rd-reqs').textContent = reqs.toLocaleString();
  document.getElementById('rd-tokens').textContent = toks.toLocaleString();
  document.getElementById('rd-err').textContent = (reqs ? (errs / reqs * 100).toFixed(1) : '0') + '%';

  const pts = buildAxis(s);
  const svg = document.getElementById('trend-svg');
  const empty = document.getElementById('trend-empty');
  if (pts.length < 2 || metricMax(pts) === 0) {
    svg.innerHTML = '';
    empty.classList.remove('hidden');
    return;
  }
  empty.classList.add('hidden');

  const wrap = document.getElementById('trend-wrap');
  const W = wrap.clientWidth || 900, H = 240;
  const padL = 46, padR = 14, padT = 16, padB = 26;
  const max = metricMax(pts);
  const innerW = W - padL - padR, innerH = H - padT - padB;
  const x = i => padL + (pts.length === 1 ? innerW / 2 : i * innerW / (pts.length - 1));
  const y = v => padT + innerH - (v / max) * innerH;

  const coords = pts.map((p, i) => [x(i), y(metricVal(p))]);
  const line = coords.map((c, i) => (i ? 'L' : 'M') + c[0].toFixed(1) + ',' + c[1].toFixed(1)).join(' ');
  const area = `M${coords[0][0].toFixed(1)},${(padT + innerH).toFixed(1)} ` +
    coords.map(c => 'L' + c[0].toFixed(1) + ',' + c[1].toFixed(1)).join(' ') +
    ` L${coords[coords.length - 1][0].toFixed(1)},${(padT + innerH).toFixed(1)} Z`;

  // y gridlines at 0, .5, 1
  let grid = '';
  [0, 0.5, 1].forEach(f => {
    const gy = padT + innerH - f * innerH;
    grid += `<line x1="${padL}" y1="${gy.toFixed(1)}" x2="${W - padR}" y2="${gy.toFixed(1)}" class="grid-line"/>`;
    grid += `<text x="${padL - 8}" y="${(gy + 3).toFixed(1)}" class="axis-label" text-anchor="end">${fmtCompact(Math.round(f * max))}</text>`;
  });
  // x labels: ~5 evenly spaced
  let xlabels = '';
  const step = Math.max(1, Math.ceil(pts.length / 5));
  for (let i = 0; i < pts.length; i += step) {
    xlabels += `<text x="${x(i).toFixed(1)}" y="${H - 8}" class="axis-label" text-anchor="middle">${xLabel(pts[i].bucket)}</text>`;
  }

  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.innerHTML = `
    <defs><linearGradient id="trendFill" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0%" stop-color="var(--accent)" stop-opacity="0.28"/>
      <stop offset="100%" stop-color="var(--accent)" stop-opacity="0"/>
    </linearGradient></defs>
    ${grid}
    <path d="${area}" fill="url(#trendFill)"/>
    <path d="${line}" class="trend-line" fill="none"/>
    ${xlabels}
    <line id="trend-guide" class="trend-guide hidden" y1="${padT}" y2="${padT + innerH}"/>
    <circle id="trend-dot" class="trend-dot hidden" r="3.5"/>`;

  // hover
  svg._pts = pts; svg._coords = coords; svg._geom = { padL, innerW, n: pts.length };
}

function metricMax(pts) { return Math.max(0, ...pts.map(metricVal)); }
function fmtCompact(n) {
  if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

function trendHover(e) {
  const svg = document.getElementById('trend-svg');
  if (!svg._coords) return;
  const rect = svg.getBoundingClientRect();
  const vb = svg.viewBox.baseVal;
  const px = (e.clientX - rect.left) / rect.width * vb.width;
  const { padL, innerW, n } = svg._geom;
  let idx = Math.round((px - padL) / (n > 1 ? innerW / (n - 1) : 1));
  idx = Math.max(0, Math.min(n - 1, idx));
  const [cx, cy] = svg._coords[idx];
  const p = svg._pts[idx];
  const guide = document.getElementById('trend-guide');
  const dot = document.getElementById('trend-dot');
  guide.setAttribute('x1', cx); guide.setAttribute('x2', cx); guide.classList.remove('hidden');
  dot.setAttribute('cx', cx); dot.setAttribute('cy', cy); dot.classList.remove('hidden');
  const tip = document.getElementById('trend-tip');
  tip.innerHTML = `<b>${fmtCompact(metricVal(p))}</b> ${statsMetric}<span class="tip-sub">${p.bucket.replace('T', ' ')} · ${p.error_count} err</span>`;
  tip.classList.remove('hidden');
  const tx = cx / vb.width * rect.width;
  tip.style.left = Math.min(rect.width - 150, Math.max(0, tx + 10)) + 'px';
  tip.style.top = (cy / vb.height * rect.height - 10) + 'px';
}
function trendLeave() {
  ['trend-guide', 'trend-dot', 'trend-tip'].forEach(id => document.getElementById(id)?.classList.add('hidden'));
}

function renderBreakdown() {
  if (!statsData) return;
  const rows = (statsData['by_' + statsDim] || []).slice()
    .map(r => ({ label: r.label, val: statsMetric === 'tokens' ? r.total_tokens : r.request_count, err: r.error_count }))
    .filter(r => r.val > 0)
    .sort((a, b) => b.val - a.val)
    .slice(0, 20);
  const el = document.getElementById('breakdown-bars');
  if (!rows.length) { el.innerHTML = '<div class="chart-empty" style="position:static">No data</div>'; return; }
  const max = rows[0].val;
  el.innerHTML = rows.map(r => `
    <div class="bar-row">
      <div class="bar-label" title="${r.label}">${r.label}</div>
      <div class="bar-track"><div class="bar-fill" style="width:${Math.max(2, r.val / max * 100)}%"></div></div>
      <div class="bar-val">${fmtCompact(r.val)}</div>
      <div class="bar-err ${r.err ? 'text-red' : 'text-muted'}">${r.err}</div>
    </div>`).join('');
}

function setCfgStatus(text, cls) {
  const el = document.getElementById('cfg-status');
  el.textContent = text;
  el.className = 'cfg-status' + (cls ? ' ' + cls : '');
}

function makeChip(text) {
  const chip = document.createElement('span');
  chip.className = 'cfg-chip';
  const label = document.createElement('span');
  label.className = 'cfg-chip-label';
  label.textContent = text;
  const x = document.createElement('button');
  x.type = 'button';
  x.className = 'cfg-chip-x';
  x.textContent = '✕';
  x.onclick = () => chip.remove();
  chip.append(label, x);
  return chip;
}

function setupChips(container, values) {
  container.innerHTML = '';
  const input = document.createElement('input');
  input.className = 'cfg-chip-input';
  input.placeholder = 'add model…';
  const commit = () => {
    const v = input.value.trim();
    if (!v) return;
    const exists = chipValues(container).includes(v);
    if (!exists) container.insertBefore(makeChip(v), input);
    input.value = '';
  };
  input.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ',') { e.preventDefault(); commit(); }
    else if (e.key === 'Backspace' && !input.value) {
      const chips = container.querySelectorAll('.cfg-chip');
      if (chips.length) chips[chips.length - 1].remove();
    }
  });
  input.addEventListener('blur', commit);
  (values || []).forEach(v => container.appendChild(makeChip(v)));
  container.appendChild(input);
  container.onclick = e => { if (e.target === container) input.focus(); };
}

function chipValues(container) {
  return [...container.querySelectorAll('.cfg-chip-label')].map(s => s.textContent.trim()).filter(Boolean);
}

function addVertexRow(name, model) {
  const rows = document.getElementById('cfg-vertex-rows');
  const row = document.createElement('div');
  row.className = 'cfg-row';
  const nameInput = document.createElement('input');
  nameInput.className = 'cfg-vx-name';
  nameInput.placeholder = 'alias';
  nameInput.value = name || '';
  const arrow = document.createElement('span');
  arrow.className = 'cfg-arrow';
  arrow.textContent = '→';
  const modelInput = document.createElement('input');
  modelInput.className = 'cfg-vx-model';
  modelInput.placeholder = 'underlying model';
  modelInput.value = model || '';
  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'btn-row';
  del.textContent = '✕';
  del.onclick = () => row.remove();
  row.append(nameInput, arrow, modelInput, del);
  rows.appendChild(row);
}

async function loadConfig() {
  const r = await apiFetch('/api/config');
  if (r.status === 401) { window.location.href = '/login'; return; }
  const d = await r.json();
  setupChips(document.getElementById('cfg-claude-models'), d.claude_oauth?.models || []);
  setupChips(document.getElementById('cfg-codex-models'), d.codex?.models || []);
  const rows = document.getElementById('cfg-vertex-rows');
  rows.innerHTML = '';
  const vmodels = d.vertex?.models || [];
  vmodels.forEach(m => addVertexRow(m.Name ?? m.name, m.Model ?? m.model));
  if (!vmodels.length) addVertexRow('', '');
  document.getElementById('cfg-admin-user').value = d.server?.admin_user || '';
  document.getElementById('cfg-admin-pass').value = '';
  setCfgStatus('', '');
}

async function saveConfig() {
  const claude = chipValues(document.getElementById('cfg-claude-models'));
  const codex = chipValues(document.getElementById('cfg-codex-models'));
  const vertex = [...document.querySelectorAll('#cfg-vertex-rows .cfg-row')].map(row => ({
    name: row.querySelector('.cfg-vx-name').value.trim(),
    model: row.querySelector('.cfg-vx-model').value.trim(),
  })).filter(m => m.name || m.model);
  const body = {
    claude_oauth: { models: claude },
    codex: { models: codex },
    vertex: { models: vertex },
    server: {
      admin_user: document.getElementById('cfg-admin-user').value.trim(),
      admin_password: document.getElementById('cfg-admin-pass').value,
    },
  };
  setCfgStatus('Saving…', '');
  const r = await apiFetch('/api/config', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) { setCfgStatus(d.error || 'Save failed', 'err'); return; }
  let msg = 'Saved.';
  if (d.restart_required && d.restart_required.length) {
    msg += ' Restart required for: ' + d.restart_required.join(', ') + '.';
  }
  setCfgStatus(msg, 'ok');
  document.getElementById('cfg-admin-pass').value = '';
}

function switchTab(name, el) {
  document.querySelectorAll('[id^="tab-"]').forEach(e => e.classList.add('hidden'));
  document.querySelectorAll('.tab').forEach(e => e.classList.remove('active'));
  document.getElementById('tab-' + name).classList.remove('hidden');
  if (el) el.classList.add('active');
  if (name === 'logs') loadLogs();
  if (name === 'stats') loadStats(statsRange);
  if (name === 'keys') loadKeys();
  if (name === 'config') loadConfig();
}

let currentProvider = '';
async function openAddAccount(provider) {
  currentProvider = provider;
  document.getElementById('modal-title').textContent = 'Add ' + provider.charAt(0).toUpperCase() + provider.slice(1) + ' Account';
  document.getElementById('modal-callback-url').value = '';
  document.getElementById('modal-error').textContent = '';
  document.getElementById('modal-step1').style.display = '';
  document.getElementById('modal-step2').style.display = 'none';
  const r = await apiFetch('/auth/' + provider + '?json=1');
  const d = await r.json();
  if (d.auth_url) {
    document.getElementById('modal-auth-url').textContent = d.auth_url;
    document.getElementById('modal-auth-link').href = d.auth_url;
  }
  document.getElementById('add-account-modal').classList.add('show');
}

function closeModal() {
  document.getElementById('add-account-modal').classList.remove('show');
}

async function submitCallbackURL() {
  const url = document.getElementById('modal-callback-url').value.trim();
  if (!url) { document.getElementById('modal-error').textContent = 'Please paste the callback URL or authentication code'; return; }
  const btn = document.getElementById('modal-submit');
  btn.disabled = true; btn.textContent = 'Submitting...';
  document.getElementById('modal-error').textContent = '';
  try {
    const r = await apiFetch('/api/auth/' + currentProvider + '/exchange', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ callback_url: url })
    });
    const d = await r.json();
    if (d.error) { document.getElementById('modal-error').textContent = d.error; return; }
    closeModal();
    loadStatus();
  } catch (e) { document.getElementById('modal-error').textContent = e.message; }
  finally { btn.disabled = false; btn.textContent = 'Submit'; }
}

function openVertexModal() {
  document.getElementById('vertex-creds').value = '';
  document.getElementById('vertex-project').value = '';
  document.getElementById('vertex-file').value = '';
  document.getElementById('vertex-error').textContent = '';
  document.getElementById('vertex-modal').classList.add('show');
}

function closeVertexModal() {
  document.getElementById('vertex-modal').classList.remove('show');
}

document.getElementById('vertex-file').addEventListener('change', (e) => {
  const file = e.target.files[0];
  if (!file) return;
  const reader = new FileReader();
  reader.onload = () => {
    document.getElementById('vertex-creds').value = reader.result;
    try {
      const j = JSON.parse(reader.result);
      if (j.project_id) document.getElementById('vertex-project').value = j.project_id;
    } catch {}
  };
  reader.readAsText(file);
});

document.getElementById('vertex-creds').addEventListener('input', (e) => {
  try {
    const j = JSON.parse(e.target.value);
    if (j.project_id && !document.getElementById('vertex-project').value) {
      document.getElementById('vertex-project').value = j.project_id;
    }
  } catch {}
});

async function submitVertexCredentials() {
  const creds = document.getElementById('vertex-creds').value.trim();
  const errEl = document.getElementById('vertex-error');
  if (!creds) { errEl.textContent = 'Please upload or paste the credentials JSON'; return; }
  const btn = document.getElementById('vertex-submit');
  btn.disabled = true; btn.textContent = 'Verifying...';
  errEl.textContent = '';
  try {
    const r = await apiFetch('/api/vertex/credentials', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        credentials_json: creds,
        project_id: document.getElementById('vertex-project').value.trim(),
        region: document.getElementById('vertex-region').value.trim()
      })
    });
    const d = await r.json();
    if (d.error) { errEl.textContent = d.error; return; }
    closeVertexModal();
    loadStatus();
  } catch (e) { errEl.textContent = e.message; }
  finally { btn.disabled = false; btn.textContent = 'Verify & Save'; }
}

async function removeVertexCredentials() {
  if (!confirm('Remove uploaded GCP credentials?')) return;
  await apiFetch('/api/vertex/credentials', { method: 'DELETE' });
  loadStatus();
}

async function refreshQuota(accountId) {
  const card = document.querySelector('[data-account="' + accountId + '"]');
  if (card) card.style.opacity = '0.5';
  try {
    await apiFetch('/api/refresh-quota/codex/' + encodeURIComponent(accountId), { method: 'POST' });
  } finally {
    if (card) card.style.opacity = '1';
    loadStatus();
  }
}

async function syncModels() {
  await apiFetch('/api/sync-models', { method: 'POST' });
  loadStatus();
}

async function toggleBackend(backend) {
  await apiFetch('/api/backends/' + backend + '/toggle', { method: 'POST' });
  loadStatus();
}

async function toggleAccount(provider, id) {
  await apiFetch('/api/accounts/' + provider + '/' + encodeURIComponent(id) + '/toggle', { method: 'POST' });
  loadStatus();
}

async function removeAccount(provider, id) {
  if (!confirm('Remove ' + id + '?')) return;
  await apiFetch('/api/accounts/' + provider + '/' + encodeURIComponent(id), { method: 'DELETE' });
  loadStatus();
}

// Image generation
let imageMode = 'generate';
let uploadedImageFile = null;

function switchImageMode(mode, btn) {
  imageMode = mode;
  document.querySelectorAll('.image-mode-btn').forEach(b => b.classList.remove('active'));
  if (btn) btn.classList.add('active');
  const uploadArea = document.getElementById('image-upload-area');
  const prompt = document.getElementById('image-prompt');
  if (mode === 'edit') {
    uploadArea.style.display = '';
    prompt.placeholder = 'Describe how to modify the image...';
  } else {
    uploadArea.style.display = 'none';
    prompt.placeholder = 'Describe the image you want to generate...';
  }
}

function handleImageSelect(input) {
  if (input.files && input.files[0]) {
    uploadedImageFile = input.files[0];
    showImagePreview(uploadedImageFile);
  }
}

function handleImageDrop(e) {
  const files = e.dataTransfer.files;
  if (files && files[0] && files[0].type.startsWith('image/')) {
    uploadedImageFile = files[0];
    showImagePreview(uploadedImageFile);
  }
}

function showImagePreview(file) {
  const reader = new FileReader();
  reader.onload = function(e) {
    document.getElementById('image-preview-img').src = e.target.result;
    document.getElementById('image-upload-placeholder').style.display = 'none';
    document.getElementById('image-upload-preview').style.display = '';
  };
  reader.readAsDataURL(file);
}

function clearUploadedImage() {
  uploadedImageFile = null;
  document.getElementById('image-file-input').value = '';
  document.getElementById('image-preview-img').src = '';
  document.getElementById('image-upload-placeholder').style.display = '';
  document.getElementById('image-upload-preview').style.display = 'none';
}

async function submitImage() {
  const prompt = document.getElementById('image-prompt').value.trim();
  if (!prompt) return;
  if (imageMode === 'edit' && !uploadedImageFile) {
    alert('Please upload a reference image first');
    return;
  }

  const result = document.getElementById('image-result');
  const btn = document.getElementById('image-gen-btn');
  result.innerHTML = '';
  result.classList.add('loading');
  btn.disabled = true; btn.textContent = 'Generating...';

  const size = document.getElementById('image-size').value;
  const quality = document.getElementById('image-quality').value;
  const background = document.getElementById('image-bg').value;

  try {
    let resp;
    if (imageMode === 'edit') {
      const fd = new FormData();
      fd.append('image', uploadedImageFile);
      fd.append('prompt', prompt);
      fd.append('model', 'gpt-image-2');
      if (size) fd.append('size', size);
      if (quality) fd.append('quality', quality);
      resp = await apiFetch('/v1/images/edits', { method: 'POST', body: fd });
    } else {
      resp = await apiFetch('/v1/images/generations', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: 'gpt-image-2', prompt, size, quality, background, response_format: 'b64_json' })
      });
    }
    result.classList.remove('loading');
    if (!resp.ok) {
      const e = await resp.json();
      result.textContent = 'Error: ' + (e.error?.message || resp.statusText);
      return;
    }
    const data = await resp.json();
    if (data.data && data.data.length > 0) {
      result.innerHTML = data.data.map((img, i) => {
        const src = img.b64_json ? 'data:image/png;base64,' + img.b64_json : img.url;
        const b64 = img.b64_json || (img.url && img.url.startsWith('data:') ? img.url.split(',')[1] : '');
        return '<div class="image-result-item">'
          + '<img src="' + src + '" onclick="window.open(this.src)">'
          + (img.revised_prompt ? '<div style="font-size:12px;color:var(--text-2);margin-top:8px;text-align:left">' + img.revised_prompt + '</div>' : '')
          + (b64 ? '<button class="image-download-btn" onclick="downloadImage(\'' + i + '\')">Download PNG</button>' : '')
          + '</div>';
      }).join('');
    } else {
      result.textContent = 'No image returned';
    }
  } catch (e) {
    result.classList.remove('loading');
    result.textContent = 'Error: ' + e.message;
  } finally {
    btn.disabled = false; btn.textContent = 'Generate';
    loadStatus();
  }
}

function downloadImage(index) {
  const imgs = document.querySelectorAll('#image-result .image-result-item img');
  if (!imgs[index]) return;
  const src = imgs[index].src;
  const a = document.createElement('a');
  a.href = src;
  a.download = 'generated-image-' + Date.now() + '.png';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
}

function copyKeyInline(btn, key) {
  navigator.clipboard.writeText(key);
  const prev = btn.innerHTML;
  btn.innerHTML = '&#x2713;';
  btn.classList.add('copied');
  btn.title = 'Copied!';
  setTimeout(() => { btn.innerHTML = prev; btn.classList.remove('copied'); btn.title = 'Copy key'; }, 1200);
}

async function loadKeys() {
  const r = await apiFetch('/api/keys');
  const d = await r.json();
  const body = document.getElementById('keys-body');
  if (!d.keys || !d.keys.length) {
    body.innerHTML = '<tr><td colspan="7" class="text-muted" style="text-align:center;padding:20px">No API keys yet — create one above</td></tr>';
    return;
  }
  body.innerHTML = d.keys.map(k => {
    const limitStr = k.token_limit_daily ? k.token_limit_daily.toLocaleString() : '∞';
    const pct = k.token_limit_daily ? Math.round((k.tokens_today || 0) / k.token_limit_daily * 100) : 0;
    const limitColor = pct > 90 ? 'var(--red)' : pct > 70 ? 'var(--yellow)' : '';
    return '<tr>'
      + '<td class="text-mono">' + k.name + '</td>'
      + '<td class="text-muted text-mono" style="font-size:11px"><span style="vertical-align:middle">' + k.key.slice(0,10) + '...' + k.key.slice(-4) + '</span> <button class="icon-btn" title="Copy key" onclick="copyKeyInline(this, \'' + k.key + '\')">&#x2398;</button></td>'
      + '<td>' + (k.request_count || 0).toLocaleString() + '</td>'
      + '<td style="' + (limitColor ? 'color:'+limitColor : '') + '">' + (k.tokens_today || 0).toLocaleString() + '</td>'
      + '<td>' + (k.total_tokens || 0).toLocaleString() + '</td>'
      + '<td>' + limitStr + '</td>'
      + '<td><button class="btn-row" onclick="deleteKey(\'' + k.id + '\')">&#x1F5D1; Delete</button></td>'
      + '</tr>';
  }).join('');
}

function openCreateKey() {
  document.getElementById('key-created').style.display = 'none';
  document.getElementById('create-key-fields').style.display = '';
  document.getElementById('key-name').value = '';
  document.getElementById('key-limit').value = '0';
  const btn = document.getElementById('create-key-submit');
  btn.textContent = 'Create';
  btn.setAttribute('onclick', 'submitCreateKey()');
  document.getElementById('create-key-modal').classList.add('show');
  document.getElementById('key-name').focus();
}

function closeCreateKey() {
  document.getElementById('create-key-modal').classList.remove('show');
}

function copyCreatedKey() {
  const key = document.getElementById('key-created-value').textContent;
  navigator.clipboard.writeText(key);
  const btn = document.getElementById('key-copy-btn');
  btn.textContent = 'Copied';
  setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
}

async function submitCreateKey() {
  const name = document.getElementById('key-name').value.trim();
  if (!name) { document.getElementById('key-name').focus(); return; }
  const limit = parseInt(document.getElementById('key-limit').value) || 0;
  const r = await apiFetch('/api/keys', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({name: name, token_limit_daily: limit})
  });
  const d = await r.json();
  if (d.key) {
    document.getElementById('create-key-fields').style.display = 'none';
    document.getElementById('key-created').style.display = '';
    document.getElementById('key-created-value').textContent = d.key.key;
    const btn = document.getElementById('create-key-submit');
    btn.textContent = 'Done';
    btn.setAttribute('onclick', 'closeCreateKey()');
    loadKeys();
  }
}

async function deleteKey(id) {
  if (!confirm('Delete this API key?')) return;
  await apiFetch('/api/keys/' + id, {method: 'DELETE'});
  loadKeys();
}

// Dismiss any open modal by clicking the overlay backdrop or pressing Esc.
document.querySelectorAll('.modal-overlay').forEach((overlay) => {
  overlay.addEventListener('mousedown', (e) => {
    if (e.target === overlay) overlay.classList.remove('show');
  });
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    document.querySelectorAll('.modal-overlay.show').forEach((o) => o.classList.remove('show'));
  }
});

loadStatus();
let lastFocusLoad = 0;
window.addEventListener('focus', () => {
  if (Date.now() - lastFocusLoad > 30000) { lastFocusLoad = Date.now(); loadStatus(); }
});
