let logPage = 0;
const logLimit = 30;
let statusBooted = false;

function apiFetch(url, opts) {
  return fetch(url, opts);
}

function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>"']/g, ch => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
  })[ch]);
}

function htmlFragment(html) {
  const template = document.createElement('template');
  template.innerHTML = html;
  return template.content;
}

// Status is polled every 15 seconds. Keep live DOM nodes when their rendered
// content is unchanged so a background refresh cannot reset focus, animations,
// scroll anchoring, or force a full section layout.
function syncHTML(el, html) {
  const next = htmlFragment(html);
  const currentNodes = el.childNodes;
  const nextNodes = next.childNodes;
  const unchanged = currentNodes.length === nextNodes.length
    && Array.from(currentNodes).every((node, i) => node.isEqualNode(nextNodes[i]));
  if (unchanged) return false;
  el.replaceChildren(next);
  return true;
}

function syncKeyedHTML(container, entries) {
  const existing = new Map(Array.from(container.children).map(node => [node.dataset.refreshKey, node]));
  const retained = new Set();

  entries.forEach(({key, html}, index) => {
    const fragment = htmlFragment(html);
    const next = fragment.firstElementChild;
    if (!next || fragment.childElementCount !== 1) throw new Error('keyed render must produce one element');
    next.dataset.refreshKey = key;

    let node = existing.get(key);
    if (!node) {
      node = next;
    } else if (!node.isEqualNode(next)) {
      node.replaceWith(next);
      node = next;
    }

    const atIndex = container.children[index];
    if (atIndex !== node) container.insertBefore(node, atIndex || null);
    retained.add(key);
  });

  Array.from(container.children).forEach(node => {
    if (!retained.has(node.dataset.refreshKey)) node.remove();
  });
}

// --- token breakdown -------------------------------------------------------
// Presentation uses "input is the whole prompt" semantics:
//
//   Input  = the entire prompt, cache included
//   Cache  = the part of Input that was served from / written to a cache
//   Output = everything generated
//   Think  = the reasoning part of Output
//   Total  = Input + Output
//
// Cache and Think are *subsets*, annotations on the two totals rather than
// buckets of their own — so only Input + Output adds up.
//
// The database stores the disjoint form instead (prompt_tokens excludes cache,
// because that is what Anthropic reports), and Total is identical either way:
//   stored: prompt + cache_read + cache_write + completion
//   shown:  (prompt + cache_read + cache_write) + completion
// This function is the single place that converts, so nothing downstream has to
// know both conventions exist.
//
// Why show it this way: with a cache breakpoint on the last message the whole
// prompt is cached, leaving the stored prompt_tokens at 1 or 2. Reading "Input 1"
// next to a 100K conversation invites the conclusion that the number is broken.
// "Input 100,740 · of which 100,739 cached" says the same thing and is right on
// first read.
const REASONING_UNKNOWN = -1;
const TOKEN_KINDS = [
  { key: 'input_tokens', label: 'Input', color: 'var(--green)' },
  { key: 'completion_tokens', label: 'Output', color: 'var(--red)' },
  { key: 'cache_tokens', label: 'Cache', color: 'var(--yellow)' },
];
const REASONING_KIND = { key: 'reasoning_tokens', label: 'Thinking', color: 'var(--cyan)' };
const REASONING_HINT = 'Anthropic folds thinking tokens into output_tokens and never reports them separately';

// --- cost ------------------------------------------------------------------
// Cost is list API price: what the same tokens would have been billed at on the
// provider's pay-per-token API. On the OAuth backends (Claude Code / Codex
// subscriptions) nothing is actually charged per request, so the figure is
// "what this would have cost you on the API", not money spent. Every place that
// shows it says so on hover.
//
// null means *unpriced* — no price is known for that model — and is rendered
// "—". Collapsing it to $0 would make an unpriced backend look free.
const COST_HINT = 'list API price for these tokens · subscription (OAuth) traffic is not actually billed per request';

function costOf(o) {
  if (!o) return null;
  if ('cost_known' in o) return o.cost_known ? (o.cost_usd || 0) : null;         // one log row
  if ('cost_known_requests' in o) {                                             // an aggregate
    return o.cost_known_requests > 0 ? (o.cost_usd || 0) : null;
  }
  return typeof o.cost_usd === 'number' ? o.cost_usd : null;
}

// unpricedCount is how many requests in an aggregate contributed no cost, so a
// partial total can be shown as partial instead of passing for the whole bill.
function unpricedCount(o) {
  const reqs = o?.requests ?? o?.request_count;
  if (typeof reqs !== 'number' || typeof o?.cost_known_requests !== 'number') return 0;
  return Math.max(0, reqs - o.cost_known_requests);
}

// Money spans six orders of magnitude here: a Haiku call is $0.0003 and a
// month of Opus is four figures. Fixed decimals would print "$0.00" for the
// former, so the precision follows the magnitude.
function fmtMoney(v) {
  if (v === null || v === undefined) return '—';
  const n = Math.abs(v);
  if (n >= 1000) return '$' + v.toFixed(0).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  if (n >= 1) return '$' + v.toFixed(2);
  if (n >= 0.01) return '$' + v.toFixed(3);
  if (n > 0) return '$' + v.toFixed(4);
  return '$0';
}

// tokens normalises any object carrying the stored breakdown (a log row, a
// bucket, a dimension row, the summary) into the shape described above.
function tokens(o) {
  const read = o?.cache_read_tokens || 0, write = o?.cache_write_tokens || 0;
  const uncached = o?.prompt_tokens || 0;
  const t = {
    uncached_input: uncached,          // disjoint remainder; only the bar needs it
    cache_read_tokens: read,
    cache_write_tokens: write,
    cache_tokens: read + write,        // subset of input_tokens
    input_tokens: uncached + read + write,
    completion_tokens: o?.completion_tokens || 0,
    reasoning_tokens: o?.reasoning_tokens ?? REASONING_UNKNOWN, // subset of output
  };
  // Aggregates clamp unknown (-1) rows to 0 when summing, so a range of pure
  // Anthropic traffic arrives as reasoning 0. reasoning_known_requests === 0
  // means no row in it ever reported a figure — that is "unknown", not "none".
  if (o?.reasoning_known_requests === 0) t.reasoning_tokens = REASONING_UNKNOWN;
  t.total = t.input_tokens + t.completion_tokens;
  t.cost = costOf(o);
  t.unpriced = unpricedCount(o);
  return t;
}

// --- token hover card ------------------------------------------------------
// A native `title` renders as an OS tooltip: light background, system font, and
// a run-on sentence that has to be read rather than scanned. This reuses the
// same treatment the charts already use (.chart-tip) and lays the buckets out as
// an aligned table, so the numbers compare vertically.
let hoverTokens = {};

function tokenTipHTML(t, footer) {
  const row = (color, label, val, sub) =>
    `<tr><td><i class="tok-dot" style="background:${color}"></i>${label}</td>` +
    `<td class="tip-num">${val}</td></tr>` +
    (sub ? `<tr class="tip-sub-row"><td colspan="2">${sub}</td></tr>` : '');

  const unknown = t.reasoning_tokens === REASONING_UNKNOWN;
  // Sub-lines carry the subset relationship, so nobody has to wonder why the four
  // numbers don't add up to the total. Cache also splits into read (cheap, ~10%
  // of input price) and write (~125%), which are worth telling apart.
  const cacheSub = t.cache_tokens
    ? `of input · read ${t.cache_read_tokens.toLocaleString()} · write ${t.cache_write_tokens.toLocaleString()}`
    : 'of input';
  const thinkSub = unknown ? 'of output · upstream does not report it' : 'of output';

  return `<table class="tok-tip-table">` +
    row(TOKEN_KINDS[0].color, 'Input', t.input_tokens.toLocaleString()) +
    row(TOKEN_KINDS[2].color, 'Cache', t.cache_tokens.toLocaleString(), cacheSub) +
    row(TOKEN_KINDS[1].color, 'Output', t.completion_tokens.toLocaleString()) +
    row(REASONING_KIND.color, 'Thinking', unknown ? '—' : t.reasoning_tokens.toLocaleString(), thinkSub) +
    `<tr class="tip-total"><td>Total <em>input + output</em></td>` +
    `<td class="tip-num">${t.total.toLocaleString()}</td></tr>` +
    // Cost sits below the total because it is derived from it, and carries its
    // own caveat line when part of the traffic had no price.
    `<tr class="tip-total"><td>Cost <em>list API price</em></td>` +
    `<td class="tip-num">${fmtMoney(t.cost)}</td></tr>` +
    (t.cost === null
      ? `<tr class="tip-sub-row"><td colspan="2">no price for this model</td></tr>`
      : t.unpriced
        ? `<tr class="tip-sub-row"><td colspan="2">excludes ${t.unpriced.toLocaleString()} unpriced request(s)</td></tr>`
        : '') +
    (footer ? `<tr class="tip-foot"><td colspan="2">${escapeHTML(footer)}</td></tr>` : '') +
    `</table>`;
}

function showTokenTip(anchor, entry) {
  if (!entry) return;
  const tip = document.getElementById('tok-tip');
  if (!tip) return;
  tip.innerHTML = tokenTipHTML(entry.t, entry.footer);
  tip.classList.remove('hidden');

  // position:fixed against the viewport — the log table lives in a scrollable
  // wrapper, so an absolutely positioned card would be clipped by it.
  const a = anchor.getBoundingClientRect();
  const box = tip.getBoundingClientRect();
  const pad = 8;
  let left = a.right - box.width;                       // right-align to the number
  left = Math.max(pad, Math.min(left, window.innerWidth - box.width - pad));
  let top = a.bottom + 6;
  if (top + box.height > window.innerHeight - pad) top = a.top - box.height - 6;
  tip.style.left = left + 'px';
  tip.style.top = Math.max(pad, top) + 'px';
}

function hideTokenTip() {
  document.getElementById('tok-tip')?.classList.add('hidden');
}

// Delegated so one listener and one card serve both hover surfaces: a log row's
// Tokens figure and a breakdown bar.
document.addEventListener('mouseover', e => {
  const el = e.target.closest?.('[data-tok]');
  if (el) showTokenTip(el, hoverTokens[el.dataset.tok]);
});
document.addEventListener('mouseout', e => {
  if (e.target.closest?.('[data-tok]')) hideTokenTip();
});
// Scrolling the table would leave the card floating over unrelated rows.
document.addEventListener('scroll', hideTokenTip, true);

// tokenChips is the expanded legend-style readout (the four labelled pills).
function tokenChips(t) {
  const chip = (k, val, extra = '', title = '') =>
    `<span class="tok-chip"${title ? ` title="${escapeHTML(title)}"` : ''}>` +
    `<i class="tok-dot" style="background:${k.color}"></i>${k.label} <b>${val}</b>` +
    (extra ? `<em>${extra}</em>` : '') + '</span>';
  // Cache and Thinking are subsets; their pills say so, and the hit-rate is the
  // number worth reading off the cache pill at a glance.
  const hitPct = t.input_tokens ? Math.round(t.cache_tokens / t.input_tokens * 100) : 0;
  const cacheDetail = t.cache_tokens
    ? `${hitPct}% of input · read ${fmtCompact(t.cache_read_tokens)} / write ${fmtCompact(t.cache_write_tokens)}`
    : '';
  return [
    chip(TOKEN_KINDS[0], t.input_tokens.toLocaleString(), '', 'the whole prompt, cache included'),
    chip(TOKEN_KINDS[1], t.completion_tokens.toLocaleString(), '', 'everything generated, thinking included'),
    chip(TOKEN_KINDS[2], t.cache_tokens.toLocaleString(), cacheDetail, 'part of input, not added on top of it'),
    t.reasoning_tokens === REASONING_UNKNOWN
      ? chip(REASONING_KIND, '—', 'of output', REASONING_HINT)
      : chip(REASONING_KIND, t.reasoning_tokens.toLocaleString(), 'of output', 'part of output, not added on top of it'),
  ].join('');
}

async function loadStatus() {
  const r = await apiFetch('/api/status');
  if (r.status === 401) { window.location.href = '/login'; return; }
  const d = await r.json();
  document.getElementById('total-requests').textContent = (d.total_requests || 0).toLocaleString();
  // All-time tokens run to nine digits now that cache is counted, and the exact
  // figure is never what this readout is for — it answers "what order of
  // magnitude". Full number stays on hover.
  const tokEl = document.getElementById('total-tokens');
  tokEl.textContent = fmtCompact(d.total_tokens || 0);
  tokEl.title = (d.total_tokens || 0).toLocaleString() + ' tokens';

  const costEl = document.getElementById('total-cost');
  if (costEl) {
    costEl.textContent = fmtMoney(d.total_cost_usd || 0);
    costEl.title = COST_HINT;
  }

  const oauthEl = document.getElementById('backends-oauth');
  const apiEl = document.getElementById('backends-api');
  // Entrance animation plays once; later refreshes (e.g. on window focus) skip
  // it so the cards don't visibly flash/re-fade on every re-render.
  if (statusBooted) { oauthEl.classList.add('no-anim'); apiEl.classList.add('no-anim'); }
  statusBooted = true;
  const backendCard = b => {
    // A manually paused backend reads "Paused", not "Offline" (which means
    // unconfigured/unreachable).
    const bc = b.disabled ? 'badge-inactive' : b.status === 'active' ? 'badge-active' : b.status === 'expired' ? 'badge-expired' : 'badge-inactive';
    const bl = b.disabled ? 'Paused' : b.status === 'active' ? 'Active' : b.status === 'expired' ? 'Expired' : 'Offline';
    const dc = b.disabled ? 'dot-gray' : b.status === 'active' ? 'dot-green' : b.status === 'expired' ? 'dot-yellow' : 'dot-gray';
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
    // No card dimming for paused backends — the top-right badge (Paused/Active/
    // Expired/Offline) and the header dot already convey state.
    return `<div class="backend-card"><div class="backend-header"><span class="dot ${dc}"></span><span class="backend-name" style="text-transform:capitalize">${escapeHTML(b.name)}</span><span class="backend-badge ${bc}">${bl}</span></div>`
      + `<div class="backend-info">${escapeHTML(b.info || '')}</div>`
      + `<div class="backend-models">${(b.models || []).map(m => `<span class="model-tag">${escapeHTML(m)}</span>`).join('')}</div>`
      + accts + `<div style="display:flex;gap:4px;flex-wrap:wrap">${addBtn}${syncBtn}${toggleBtn}</div></div>`;
  };
  // OAuth/credential backends (account-rotated) vs API-key backends group into
  // two labelled sections; the API group hides itself when nothing lives there.
  const OAUTH_BACKENDS = ['claude', 'codex', 'vertex'];
  const oauthList = d.backends.filter(b => OAUTH_BACKENDS.includes(b.name));
  const apiList = d.backends.filter(b => !OAUTH_BACKENDS.includes(b.name));
  syncKeyedHTML(oauthEl, oauthList.map(b => ({key: b.name, html: backendCard(b)})));
  syncKeyedHTML(apiEl, apiList.map(b => ({key: b.name, html: backendCard(b)})));
  document.getElementById('api-group').style.display = apiList.length ? '' : 'none';

  // Render per-account quota cards
  let allQuotas = [];
  d.backends.forEach(b => {
    if (b.quotas) allQuotas = allQuotas.concat(b.quotas.map(q => ({...q, provider: b.name})));
  });
  const qGrid = document.getElementById('quota-grid');
  const qEmpty = document.getElementById('quota-empty');
  qEmpty.style.display = allQuotas.length ? 'none' : '';
  {
    const quotaCards = allQuotas.map(q => {
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
        rows = `<div style="font-size:12px;color:var(--text-2);padding:4px 0">No quota data yet — click <span style="color:var(--accent);cursor:pointer" onclick="refreshQuota('${q.provider}','${q.account_id}')">&#8635; refresh</span> to fetch</div>`;
      }
      const refreshBtn = `<button class="btn-delete" style="font-size:11px;color:var(--accent)" onclick="refreshQuota('${q.provider}','${q.account_id}')">&#8635;</button>`;
      const fetchedAt = q.fetched_at ? `<span style="font-size:10px;color:var(--text-2);margin-left:auto">cached ${q.fetched_at}</span>` : '';
      const providerLabel = (q.provider || '').charAt(0).toUpperCase() + (q.provider || '').slice(1);
      return {
        key: JSON.stringify([q.provider || '', q.account_id || '']),
        html: `<div class="quota-card" data-provider="${q.provider}" data-account="${q.account_id}"><div class="quota-card-header"><span class="model-tag" style="background:var(--accent-dim);color:var(--text-0)">${providerLabel}</span><span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${displayName}</span>${refreshBtn}</div><div style="display:flex;align-items:center;gap:6px;margin-bottom:8px"><span class="plan-badge ${planCls}">${planLabel}</span>${fetchedAt}</div>${rows}</div>`,
      };
    });
    syncKeyedHTML(qGrid, quotaCards);
  }

  const sel = document.getElementById('chat-model');
  const prevModel = sel.value;
  const statusIcon = s => s === 'active' ? '✓' : s === 'expired' ? '!' : '✗';
  const modelOptions = d.backends.map(b => {
    const lbl = b.name.charAt(0).toUpperCase() + b.name.slice(1) + ' (' + statusIcon(b.status) + ')';
    return `<optgroup label="${lbl}">${(b.models || []).map(m =>
      `<option value="${m}"${b.status !== 'active' ? ' disabled' : ''}>${m}</option>`
    ).join('')}</optgroup>`;
  }).join('');
  if (syncHTML(sel, modelOptions)) {
    const prev = prevModel && sel.querySelector(`option[value="${prevModel}"]:not([disabled])`);
    if (prev) prev.selected = true;
    else { const first = sel.querySelector('option:not([disabled])'); if (first) first.selected = true; }
    if (sel._sync) sel._sync();
  }
}

let chatStatusTimer = 0;

function hideChatStatus() {
  if (chatStatusTimer) clearInterval(chatStatusTimer);
  chatStatusTimer = 0;
  const status = document.getElementById('chat-status');
  const output = document.getElementById('chat-output');
  status.hidden = true;
  output.setAttribute('aria-busy', 'false');
}

function showChatStatus(model) {
  hideChatStatus();
  const status = document.getElementById('chat-status');
  const output = document.getElementById('chat-output');
  const elapsed = document.getElementById('chat-status-elapsed');
  document.getElementById('chat-status-model').textContent = model || 'model';
  status.hidden = false;
  output.setAttribute('aria-busy', 'true');
  const startedAt = performance.now();
  const updateElapsed = () => { elapsed.textContent = ((performance.now() - startedAt) / 1000).toFixed(1) + 's'; };
  updateElapsed();
  chatStatusTimer = setInterval(updateElapsed, 100);
}

async function sendChat() {
  const model = document.getElementById('chat-model').value;
  const input = document.getElementById('chat-input').value.trim();
  if (!input) return;
  const output = document.getElementById('chat-output');
  const sendBtn = document.getElementById('chat-send');
  output.textContent = '';
  output.classList.add('waiting');
  showChatStatus(model);
  sendBtn.disabled = true; sendBtn.textContent = 'Working';
  let hasVisibleText = false;
  try {
    const resp = await apiFetch('/v1/chat/completions', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model, messages: [{ role: 'user', content: input }], stream: true })
    });
    if (!resp.ok) { const e = await resp.json(); hideChatStatus(); output.classList.remove('waiting'); output.textContent = 'Error: ' + (e.error?.message || resp.statusText); return; }
    const reader = resp.body.getReader(); const dec = new TextDecoder(); let buf = '';
    while (true) {
      const { done, value } = await reader.read(); if (done) break;
      buf += dec.decode(value, { stream: true });
      const lines = buf.split('\n'); buf = lines.pop();
      for (const line of lines) {
        if (!line.startsWith('data: ') || line === 'data: [DONE]') continue;
        try {
          const c = JSON.parse(line.slice(6));
          if (c.error) { hideChatStatus(); output.classList.remove('waiting'); output.textContent = 'Error: ' + (c.error.message || JSON.stringify(c.error)); return; }
          const t = c.choices?.[0]?.delta?.content;
          if (t) {
            if (!hasVisibleText) {
              hasVisibleText = true;
              hideChatStatus();
              output.classList.remove('waiting');
            }
            output.textContent += t;
            output.scrollTop = output.scrollHeight;
          }
        } catch {}
      }
    }
    if (!hasVisibleText) output.textContent = 'No visible text returned.';
  } catch (e) { output.textContent = 'Error: ' + e.message; }
  finally { hideChatStatus(); output.classList.remove('waiting'); sendBtn.disabled = false; sendBtn.textContent = 'Send'; loadStatus(); }
}

function clearChat() {
  hideChatStatus();
  const output = document.getElementById('chat-output');
  output.classList.remove('waiting');
  output.textContent = '';
  document.getElementById('chat-input').value = '';
}

let logErrorsOnly = false, logSearch = '', logSearchTimer = 0;

function toggleLogErrors(btn) {
  logErrorsOnly = !logErrorsOnly;
  btn.classList.toggle('active', logErrorsOnly);
  logPage = 0;
  loadLogs();
}
function onLogSearch() {
  clearTimeout(logSearchTimer);
  logSearchTimer = setTimeout(() => {
    logSearch = document.getElementById('log-search').value.trim();
    logPage = 0;
    loadLogs();
  }, 300);
}

async function loadLogs() {
  hideTokenTip();
  hoverTokens = {}; // only the visible page is ever hovered; don't grow unbounded
  const q = '/api/logs?limit=' + logLimit + '&offset=' + (logPage * logLimit) +
    (logErrorsOnly ? '&errors=1' : '') +
    (logSearch ? '&q=' + encodeURIComponent(logSearch) : '');
  const r = await apiFetch(q);
  const d = await r.json();
  document.getElementById('log-total').textContent = d.total;
  document.getElementById('page-info').textContent = (logPage + 1) + ' / ' + Math.max(1, Math.ceil(d.total / logLimit));
  document.getElementById('log-body').innerHTML = (d.logs || []).map(l => {
    const t = new Date(l.time).toLocaleString('en-GB', { hour: '2-digit', minute: '2-digit', second: '2-digit', day: '2-digit', month: 'short' });
    const tk = tokens(l);
    const sc = l.status < 400 ? 'text-green' : 'text-red';
    const keyTag = l.api_key_name ? '<span style="font-size:10px;color:var(--accent);margin-left:4px">[' + l.api_key_name + ']</span>' : '';
    const acct = l.account || '-';
    const foTag = l.failover_from
      ? ` <span style="color:var(--yellow);font-size:10px;cursor:help" title="failed over from: ${l.failover_from}">↩</span>`
      : '';
    const errRow = l.error ? `<tr class="log-err-row"><td colspan="8"><div class="log-err" title="${escAttr(l.error)}">${escHtml(l.error)}</div></td></tr>` : '';
    // The breakdown is hover-only. A per-row bar or an expandable chip row both
    // cost permanent visual weight for a detail that is looked up occasionally,
    // and the table's job is scanning for anomalies.
    hoverTokens['log:' + l.id] = { t: tk };
    const tokCell = tk.total
      ? `<span class="tok-total" data-tok="log:${l.id}">${fmtCompact(tk.total)}</span>`
      : '<span class="text-muted">–</span>';
    // Cache hit = read tokens as a share of the full input. cache_write is the
    // miss that seeds the cache, so it is not a hit; a request with no input at
    // all (or no cache read) shows "–" rather than a misleading 0%.
    const hitCell = (tk.input_tokens && tk.cache_read_tokens)
      ? `<span title="${tk.cache_read_tokens.toLocaleString()} of ${tk.input_tokens.toLocaleString()} input read from cache">${Math.round(tk.cache_read_tokens / tk.input_tokens * 100)}%</span>`
      : '<span class="text-muted">–</span>';
    return `<tr><td class="text-muted text-mono">${t}</td><td class="text-mono">${l.model}${keyTag}</td><td class="text-muted">${l.backend}</td><td class="text-muted text-mono" style="font-size:11px" title="${acct}${l.failover_from ? ' (failover from ' + l.failover_from + ')' : ''}">${acct}${foTag}</td><td>${l.latency_ms}ms</td><td>${tokCell}</td><td>${hitCell}</td><td class="${sc}">${l.status}</td></tr>${errRow}`;
  }).join('') || '<tr><td colspan="8" class="text-muted" style="text-align:center;padding:24px">' + (logErrorsOnly || logSearch ? 'No matching requests' : 'No requests yet') + '</td></tr>';
}

function prevPage() { if (logPage > 0) { logPage--; loadLogs(); } }
function nextPage() { logPage++; loadLogs(); }

let statsRange = '7d', statsMetric = 'requests', statsDim = 'model', statsData = null;
let statsFilter = { dim: '', val: '' };

function setActive(groupId, btn) {
  document.querySelectorAll('#' + groupId + ' button').forEach(b => b.classList.remove('active'));
  if (btn) btn.classList.add('active');
}

async function loadStats(range, btn) {
  if (range) statsRange = range;
  if (btn) setActive('stats-range', btn);
  const tz = -new Date().getTimezoneOffset(); // minutes east of UTC
  const q = '/api/stats?range=' + statsRange + '&tz=' + tz +
    '&dim=' + encodeURIComponent(statsFilter.dim) + '&val=' + encodeURIComponent(statsFilter.val);
  const r = await apiFetch(q);
  if (r.status === 401) { window.location.href = '/login'; return; }
  statsData = await r.json();
  populateFilter();
  renderSummary();
  renderTrend();
  renderBreakdown();
  renderCalendar();
}

function setRange(range, btn) { loadStats(range, btn); }
function setMetric(metric, btn) { statsMetric = metric; setActive('metric-seg', btn); renderSummary(); renderTrend(); renderBreakdown(); renderCalendar(); }
function setDimension(dim, btn) { statsDim = dim; setActive('dim-seg', btn); renderBreakdown(); }
function setFilter(value) {
  const i = value.indexOf(':');
  statsFilter = i < 0 ? { dim: '', val: '' } : { dim: value.slice(0, i), val: value.slice(i + 1) };
  loadStats();
}

const DIM_LABELS = { model: 'Model', key: 'Key', backend: 'Backend', account: 'Account', status: 'Status' };

// Custom themed dropdown (a native <select> popup can't be styled to match).
function populateFilter() {
  document.getElementById('filter-label').textContent =
    statsFilter.dim ? `${DIM_LABELS[statsFilter.dim]}: ${statsFilter.val}` : 'All traffic';
  const facets = statsData.facets || {};
  let html = `<div class="dd-opt${statsFilter.dim ? '' : ' sel'}" onclick="pickFilter('')">All traffic</div>`;
  ['model', 'key', 'backend', 'account'].forEach(dim => {
    const rows = facets[dim] || [];
    if (!rows.length) return;
    html += `<div class="dd-group">${DIM_LABELS[dim]}</div>`;
    html += rows.map(r => {
      const sel = statsFilter.dim === dim && statsFilter.val === r.label;
      return `<div class="dd-opt${sel ? ' sel' : ''}" onclick="pickFilter('${escAttr(dim + ':' + r.label)}')">` +
        `<span class="dd-opt-l">${escHtml(r.label)}</span><span class="dd-opt-n">${fmtCompact(r.request_count)}</span></div>`;
    }).join('');
  });
  document.getElementById('filter-panel').innerHTML = html;
}

function closeAllDD() { document.querySelectorAll('.dd-panel').forEach(p => p.classList.add('hidden')); }
function toggleFilterDD(e) {
  e.stopPropagation();
  const p = document.getElementById('filter-panel');
  const wasOpen = !p.classList.contains('hidden');
  closeAllDD();
  if (!wasOpen) p.classList.remove('hidden');
}
function pickFilter(value) { closeAllDD(); setFilter(value); }
document.addEventListener('click', closeAllDD);

// enhanceSelect wraps a native <select> in a themed dropdown, keeping the
// <select> as the source of truth (options/value/onchange untouched). Reads
// options live on open, so dynamically-repopulated selects just need _sync().
function enhanceSelect(sel) {
  if (sel._enhanced) return;
  sel._enhanced = true;
  sel.classList.add('dd-native');
  const dd = document.createElement('div');
  dd.className = 'dd' + (sel.classList.contains('model-select') ? ' dd-grow' : '');
  const trigger = document.createElement('button');
  trigger.type = 'button';
  trigger.className = 'dd-trigger';
  const label = document.createElement('span');
  label.className = 'dd-trigger-label';
  const caret = document.createElement('span');
  caret.className = 'dd-caret';
  caret.textContent = '▾';
  trigger.append(label, caret);
  const panel = document.createElement('div');
  panel.className = 'dd-panel hidden';
  dd.append(trigger);
  sel.after(dd);
  // Portal the panel to <body>: image/chat controls live inside panels with
  // overflow:hidden (and a transform from the entrance animation), which would
  // otherwise clip a fixed-positioned dropdown.
  document.body.appendChild(panel);
  const sync = () => { const o = sel.options[sel.selectedIndex]; label.textContent = o ? o.textContent : ''; };
  const addOpt = o => {
    const el = document.createElement('div');
    el.className = 'dd-opt' + (o.selected ? ' sel' : '') + (o.disabled ? ' dis' : '');
    el.textContent = o.textContent;
    if (!o.disabled) el.onclick = e => {
      e.stopPropagation();
      sel.value = o.value; sync();
      panel.classList.add('hidden');
      sel.dispatchEvent(new Event('change'));
    };
    panel.append(el);
  };
  trigger.onclick = e => {
    e.stopPropagation();
    const wasOpen = !panel.classList.contains('hidden');
    closeAllDD();
    if (wasOpen) return;
    panel.innerHTML = '';
    [...sel.children].forEach(node => {
      if (node.tagName === 'OPTGROUP') {
        const g = document.createElement('div'); g.className = 'dd-group'; g.textContent = node.label;
        panel.append(g);
        [...node.children].forEach(addOpt);
      } else if (node.tagName === 'OPTION') addOpt(node);
    });
    // Fixed positioning escapes the panel's overflow:hidden clipping.
    const r = trigger.getBoundingClientRect();
    panel.style.position = 'fixed';
    panel.style.top = (r.bottom + 4) + 'px';
    panel.style.left = r.left + 'px';
    panel.style.minWidth = r.width + 'px';
    panel.classList.remove('hidden');
  };
  sel._sync = sync;
  sync();
}

function escAttr(s) { return String(s).replace(/"/g, '&quot;'); }
function escHtml(s) { return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }

function renderSummary() {
  if (!statsData) return;
  const s = statsData.summary || { requests: 0, tokens: 0, errors: 0, avg_latency_ms: 0 };
  const errPct = s.requests ? (s.errors / s.requests * 100).toFixed(1) : '0';
  const scope = statsFilter.dim ? ` · ${DIM_LABELS[statsFilter.dim]}: ${escHtml(statsFilter.val)}` : '';
  const tk = tokens(s);
  document.getElementById('stats-summary').innerHTML =
    `<span><b>${s.requests.toLocaleString()}</b> requests</span>` +
    // No tooltip here: the chip strip immediately below already spells out every
    // bucket exactly, so a hover repeating it would be pure duplication.
    `<span><b>${s.tokens.toLocaleString()}</b> tokens</span>` +
    // The unpriced-request caveat rides on the tooltip rather than the strip:
    // it is only ever true when a served model is missing from the price table,
    // which is a config problem, not a number the reader has to weigh.
    `<span title="${escAttr(COST_HINT + (tk.unpriced ? ` · excludes ${tk.unpriced} unpriced request(s)` : ''))}">` +
    `<b>${fmtMoney(tk.cost)}</b> cost${tk.unpriced ? '<sup class="text-yellow">*</sup>' : ''}</span>` +
    `<span><b class="${s.errors ? 'text-red' : ''}">${errPct}%</b> errors</span>` +
    `<span><b>${Math.round(s.avg_latency_ms)}</b> ms avg</span>` +
    `<span class="sum-scope">${statsRange}${scope}</span>`;
  // The breakdown lives on its own line: it is the legend for every colour used
  // in the logs table and the bars below, so it should not compete for space in
  // the totals strip.
  document.getElementById('stats-tokens').innerHTML = tk.total ? tokenChips(tk) : '';
}

// --- axis helpers (local time, matching the tz-shifted SQLite bucket keys) ---
const pad2 = n => String(n).padStart(2, '0');
const dayKey = d => `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
const hourKey = d => `${dayKey(d)}T${pad2(d.getHours())}:00`;

function buildAxis(series) {
  const map = {};
  (series || []).forEach(s => { map[s.bucket] = s; });
  const zero = b => map[b] || { bucket: b, request_count: 0, total_tokens: 0, error_count: 0 };
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

// total_tokens comes from the server already summed across the four buckets, so
// the chart can't drift from the totals strip the way a client-side add would.
const metricVal = p => statsMetric === 'tokens' ? (p.total_tokens || 0)
  : statsMetric === 'cost' ? (p.cost_usd || 0)
  : statsMetric === 'errors' ? p.error_count : p.request_count;
const metricLabel = () => statsMetric === 'tokens' ? 'Tokens'
  : statsMetric === 'cost' ? 'Cost' : statsMetric === 'errors' ? 'Errors' : 'Requests';
// Axis labels, bar values and the trend tooltip all read the active metric, and
// "$4.12" and "4.1k" need different formatting.
const metricFmt = v => statsMetric === 'cost' ? fmtMoney(v) : fmtCompact(Math.round(v));
const xLabel = b => statsData && statsData.granularity === 'hour' ? b.slice(11, 16) : b.slice(5);

// GitHub-style contribution heatmap over ~53 weeks, colored by the active metric.
function renderCalendar() {
  if (!statsData) return;
  const map = {};
  (statsData.calendar || []).forEach(c => { map[c.bucket] = c; });
  const valOf = c => metricVal(c);
  const pfx = statsMetric === 'errors' ? 'cal-e' : 'cal-l';
  document.getElementById('cal-wrap').classList.toggle('errors', statsMetric === 'errors');

  const gap = 3, topPad = 18, leftPad = 28, rightPad = 4, WEEKS = 53;
  // Size cells to fill the panel width (keeps squares; re-runs on resize).
  const W = Math.max(420, document.getElementById('calendar').clientWidth || 900);
  const stride = (W - leftPad - rightPad) / WEEKS;
  const cell = Math.max(6, stride - gap);

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
    days.push({ d: new Date(d), k, v, err: c ? c.error_count : 0, future: new Date(d) > today });
  }
  const lvl = v => v <= 0 ? 0 : max <= 0 ? 0 : v <= max * 0.25 ? 1 : v <= max * 0.5 ? 2 : v <= max * 0.75 ? 3 : 4;

  const H = topPad + 7 * stride + 2;
  const MN = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  let cells = '', months = '', prevMonth = -1;
  days.forEach((day, i) => {
    const wk = Math.floor(i / 7), dow = i % 7;
    const x = leftPad + wk * stride, y = topPad + dow * stride;
    const cls = day.future ? 'cal-future' : pfx + lvl(day.v);
    cells += `<rect x="${x.toFixed(1)}" y="${y.toFixed(1)}" width="${cell.toFixed(1)}" height="${cell.toFixed(1)}" rx="2" class="${cls}"></rect>`;
    if (dow === 0) {
      const m = day.d.getMonth();
      if (m !== prevMonth && day.d.getDate() <= 7) { months += `<text x="${x.toFixed(1)}" y="11" class="cal-axis">${MN[m]}</text>`; prevMonth = m; }
    }
  });
  const wd = [[1, 'Mon'], [3, 'Wed'], [5, 'Fri']]
    .map(([r, t]) => `<text x="0" y="${(topPad + r * stride + cell - 1).toFixed(1)}" class="cal-axis">${t}</text>`).join('');
  const svg = document.getElementById('calendar');
  svg.innerHTML = `<svg viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" class="cal-svg"
      onmousemove="calHover(event)" onmouseleave="calLeave()">${months}${wd}${cells}</svg>`;
  svg._cal = { days, leftPad, topPad, stride, WEEKS };
}

function calHover(e) {
  const wrap = document.getElementById('calendar');
  const g = wrap._cal;
  if (!g) return;
  const svg = wrap.firstElementChild;
  const rect = svg.getBoundingClientRect();
  const x = e.clientX - rect.left, y = e.clientY - rect.top;
  const wk = Math.floor((x - g.leftPad) / g.stride), dow = Math.floor((y - g.topPad) / g.stride);
  const tip = document.getElementById('cal-tip');
  if (wk < 0 || wk >= g.WEEKS || dow < 0 || dow > 6) { tip.classList.add('hidden'); return; }
  const day = g.days[wk * 7 + dow];
  if (!day || day.future) { tip.classList.add('hidden'); return; }
  tip.innerHTML = `<b>${fmtCompact(day.v)}</b> ${statsMetric}<span class="tip-sub">${day.k}${day.err ? ' · ' + day.err + ' err' : ''}</span>`;
  tip.classList.remove('hidden');
  const wrapRect = document.getElementById('cal-wrap').getBoundingClientRect();
  tip.style.left = Math.min(wrapRect.width - 150, e.clientX - wrapRect.left + 10) + 'px';
  tip.style.top = (e.clientY - wrapRect.top - 8) + 'px';
}
function calLeave() { document.getElementById('cal-tip').classList.add('hidden'); }

function renderTrend() {
  if (!statsData) return;
  document.getElementById('trend-title').textContent = metricLabel() + ' over time';
  const s = statsData.series || [];
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
    grid += `<text x="${padL - 8}" y="${(gy + 3).toFixed(1)}" class="axis-label" text-anchor="end">${metricFmt(f * max)}</text>`;
  });
  // x labels: ~5 evenly spaced
  let xlabels = '';
  const step = Math.max(1, Math.ceil(pts.length / 5));
  for (let i = 0; i < pts.length; i += step) {
    xlabels += `<text x="${x(i).toFixed(1)}" y="${H - 8}" class="axis-label" text-anchor="middle">${xLabel(pts[i].bucket)}</text>`;
  }

  const err = statsMetric === 'errors';
  const stroke = err ? 'var(--red)' : 'var(--accent)';
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.innerHTML = `
    <defs><linearGradient id="trendFill" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0%" stop-color="${stroke}" stop-opacity="0.28"/>
      <stop offset="100%" stop-color="${stroke}" stop-opacity="0"/>
    </linearGradient></defs>
    ${grid}
    <path d="${area}" fill="url(#trendFill)"/>
    <path d="${line}" fill="none" style="stroke:${stroke}" class="trend-line"/>
    ${xlabels}
    <line id="trend-guide" class="trend-guide hidden" y1="${padT}" y2="${padT + innerH}"/>
    <circle id="trend-dot" class="trend-dot hidden" r="3.5"/>`;

  // hover
  svg._pts = pts; svg._coords = coords; svg._geom = { padL, innerW, n: pts.length };
}

function metricMax(pts) { return Math.max(0, ...pts.map(metricVal)); }
// A B tier matters now that totals include cache: all-time crossed 100M within
// a day of shipping the breakdown, and "1.3B" beats "1,284,003,915" everywhere
// this is used (readout, axis labels, bar values).
function fmtCompact(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2).replace(/\.?0+$/, '') + 'B';
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
  tip.innerHTML = `<b>${metricFmt(metricVal(p))}</b> ${statsMetric}<span class="tip-sub">${p.bucket.replace('T', ' ')} · ${p.error_count} err</span>`;
  tip.classList.remove('hidden');
  const tx = cx / vb.width * rect.width;
  tip.style.left = Math.min(rect.width - 150, Math.max(0, tx + 10)) + 'px';
  tip.style.top = (cy / vb.height * rect.height - 10) + 'px';
}
function trendLeave() {
  ['trend-guide', 'trend-dot', 'trend-tip'].forEach(id => document.getElementById(id)?.classList.add('hidden'));
}

const STATUS_LABELS = {
  '400': 'Bad request', '401': 'Unauthorized', '403': 'Forbidden', '404': 'Not found',
  '408': 'Timeout', '413': 'Payload too large', '422': 'Unprocessable', '429': 'Rate limited',
  '500': 'Server error', '502': 'Bad gateway', '503': 'Unavailable', '504': 'Gateway timeout', '529': 'Overloaded'
};

function renderBreakdown() {
  if (!statsData) return;
  const isStatus = statsDim === 'status';
  // The status (errors) breakdown always counts failed requests; other
  // dimensions follow the active metric.
  const showTokens = !isStatus && statsMetric === 'tokens';
  const rows = (statsData['by_' + statsDim] || []).slice()
    .map(r => ({
      label: r.label,
      val: isStatus ? r.request_count : metricVal(r),
      err: r.error_count,
      tok: showTokens ? tokens(r) : null,
    }))
    .filter(r => r.val > 0)
    .sort((a, b) => b.val - a.val)
    .slice(0, 20);
  const el = document.getElementById('breakdown-bars');
  if (!rows.length) {
    el.innerHTML = `<div class="chart-empty" style="position:static">${isStatus ? 'No errors in range' : 'No data'}</div>`;
    return;
  }
  const max = rows[0].val;
  const active = statsFilter.dim === statsDim ? statsFilter.val : '';
  el.classList.toggle('bars-err', isStatus);
  el.innerHTML = rows.map((r, i) => {
    const disp = isStatus ? `${escHtml(r.label)}${STATUS_LABELS[r.label] ? ' · ' + STATUS_LABELS[r.label] : ''}` : escHtml(r.label);
    const tail = isStatus ? '' : `<div class="bar-err ${r.err ? 'text-red' : 'text-muted'}">${r.err}</div>`;
    // A stacked fill needs genuinely disjoint parts, so it uses the *uncached*
    // remainder rather than Input (which contains Cache). Cache first, since on
    // real traffic it dominates and reads best anchored to the left edge.
    const fill = r.tok && r.tok.total
      ? [
          ['cache_tokens', TOKEN_KINDS[2].color],
          ['uncached_input', TOKEN_KINDS[0].color],
          ['completion_tokens', TOKEN_KINDS[1].color],
        ].filter(([k]) => r.tok[k] > 0)
         .map(([k, color]) =>
           `<i style="width:${(r.tok[k] / r.tok.total * 100).toFixed(2)}%;background:${color}"></i>`).join('')
      : '';
    // In tokens mode a bar gets the same hover card as a log row. Other metrics
    // have no breakdown to show, so they keep the plain filter hint.
    let hover = ` title="Filter by ${escAttr(r.label)}"`;
    if (r.tok && r.tok.total) {
      const key = 'bar:' + statsDim + ':' + i;
      hoverTokens[key] = { t: r.tok, footer: 'click to filter' };
      hover = ` data-tok="${key}"`;
    }
    return `
    <div class="bar-row${r.label === active ? ' bar-active' : ''}"${hover} onclick="filterToBar('${escAttr(r.label)}')">
      <div class="bar-label">${disp}</div>
      <div class="bar-track"><div class="bar-fill${fill ? ' bar-split' : ''}" style="width:${Math.max(2, r.val / max * 100)}%">${fill}</div></div>
      <div class="bar-val">${isStatus ? fmtCompact(r.val) : metricFmt(r.val)}</div>
      ${tail}
    </div>`;
  }).join('');
}

// Clicking a breakdown bar toggles a global filter on the current dimension.
function filterToBar(label) {
  if (statsFilter.dim === statsDim && statsFilter.val === label) {
    statsFilter = { dim: '', val: '' }; // click again to clear
  } else {
    statsFilter = { dim: statsDim, val: label };
  }
  loadStats();
}

// A save used to leave "Saved." pinned to the page forever, which stops carrying
// information the moment you read it twice — you cannot tell the confirmation of
// the save you just made from the one before it. Feedback for a momentary event
// should be momentary; only errors stay, because those still need acting on.
function toast(text, kind) {
  if (!text) return;
  const host = document.getElementById('toast-host');
  if (!host) return;
  const t = el('div', 'toast' + (kind ? ' toast-' + kind : ''), text);
  // Newest on top, and never more than a handful stacked up.
  host.prepend(t);
  while (host.children.length > 3) host.lastElementChild.remove();
  requestAnimationFrame(() => t.classList.add('is-in'));
  const life = kind === 'err' ? 6000 : 2600;
  const close = () => {
    t.classList.remove('is-in');
    setTimeout(() => t.remove(), 220);
  };
  t.onclick = close;
  setTimeout(close, life);
}



// el builds an element in one call — this file creates a few hundred of them and
// the three-line createElement/className/textContent dance drowns the structure.
// `el` is a local name here because the surrounding file uses `el` for locals
// elsewhere; those shadow it harmlessly.
function el(tag, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text !== undefined && text !== null) node.textContent = text;
  return node;
}

// --- Models editor ---------------------------------------------------------
// One row per model:
//
//   [ model name ]              ↦          [ $3 / $15 ]   [✕]
//
// The name is both what clients call and what we call upstream — the executors
// pass it through unchanged unless told otherwise, and that is the case for
// nearly every row. Renaming is therefore an *attribute* of a model, like its
// price, not a second column that sits half-empty forever: the ↦ slot is a ghost
// until you hover it or it holds a value. Only Vertex and Kimi resolve names at
// all, so the other two backends have no slot rather than an inert one.
//
// The four backends used to have two different editors — chips for the OAuth
// ones (which cannot be edited, only deleted and retyped) and full-width
// two-input rows for the mapped ones (three lines of vertical space per model).
// They are the same thing: a name the client calls, optionally pointing at a
// different upstream name. Rendering them the same way makes the whole set
// scannable, and puts the price of each model where you decide whether to serve
// it.
//
// OAuth backends pass the name through unchanged, so their upstream cell is a
// muted "same" rather than an input — the column still lines up.
const MODEL_GROUPS = [
  {
    id: 'claude', label: 'Claude OAuth', live: true, mapped: false,
    hint: 'served under your Claude subscription',
  },
  {
    id: 'vertex', label: 'Vertex', live: true, mapped: true,
    hint: 'served by Vertex AI · ↦ to call upstream by another name',
  },
  {
    id: 'kimi', label: 'Kimi', live: true, mapped: true,
    hint: 'the API key stays in its env var · ↦ to call upstream by another name',
  },
  {
    id: 'codex', label: 'Codex', live: false, mapped: false,
    hint: 'fallback list only — Codex re-syncs from the API at startup',
  },
];

// cfgModels[group] = [{name, model}] — model is unused for unmapped groups.
let cfgModels = { claude: [], vertex: [], kimi: [], codex: [] };
// Effective price table from /api/pricing, plus the local override edits. Both
// are keyed by normalised model name; overrides also keep the name as typed, so
// saving round-trips it verbatim into config.yaml.
let priceTable = new Map();
let pricePrefixes = [];
let priceOverrides = new Map();

// normalizeModel and lookupPrice mirror internal/pricing (normalize + exact,
// then longest-prefix). Duplicated deliberately: the table itself comes from the
// server, and doing the resolution client-side is what lets a price appear for a
// model you are still typing, before it has ever been saved or served.
const PRICE_MIN_PREFIX = 6;

function normalizeModel(m) {
  let s = String(m || '').trim().toLowerCase();
  const slash = s.lastIndexOf('/');
  if (slash >= 0) s = s.slice(slash + 1);
  if (s.startsWith('anthropic.')) s = s.slice('anthropic.'.length);
  s = s.replace(/[-@]\d{8}$/, '');
  if (s.endsWith('-oauth')) s = s.slice(0, -'-oauth'.length);
  return s;
}

function lookupPrice(name) {
  if (!name) return null;
  if (priceTable.has(name)) return priceTable.get(name);
  for (const k of pricePrefixes) if (name.startsWith(k)) return priceTable.get(k);
  return null;
}

// resolvePrice answers the questions the price cell has to distinguish: is there
// a price, is it one you set, and — since a name can match by prefix or through
// its alias target — which table entry it actually came from.
function resolvePrice(alias, upstream) {
  const a = normalizeModel(alias);
  const custom = priceOverrides.get(a);
  if (custom) return { price: custom, source: 'custom', from: a };
  const mine = e => priceOverrides.has(e.name) ? 'custom' : 'builtin';
  let hit = lookupPrice(a);
  if (hit) return { price: hit, source: mine(hit), from: hit.name };
  const u = normalizeModel(upstream);
  if (u && u !== a) {
    hit = lookupPrice(u);
    if (hit) return { price: hit, source: mine(hit), from: hit.name, viaAlias: true };
  }
  return null;
}

const PRICE_FIELDS = [
  ['input', 'Input'],
  ['output', 'Output'],
  ['cache_read', 'Cache read'],
  ['cache_write', 'Cache write'],
];

function priceLabel(v) {
  if (v === null || v === undefined) return '—';
  if (v === 0) return '$0';
  if (v >= 1) return '$' + (Math.round(v * 100) / 100);
  return '$' + Number(v.toFixed(4));
}

function renderModels() {
  const host = document.getElementById('cfg-models');
  host.innerHTML = '';
  MODEL_GROUPS.forEach(g => host.appendChild(renderModelGroup(g)));
}

function renderModelGroup(g) {
  const list = cfgModels[g.id] || [];
  const wrap = el('div', 'mdl-group');

  const head = el('div', 'mdl-group-head');
  head.appendChild(el('span', 'mdl-group-name', g.label));
  const badge = el('span', 'cfg-badge ' + (g.live ? 'cfg-live' : 'cfg-restart'),
    g.live ? '● live' : '⟳ restart');
  badge.title = g.live
    ? 'applied immediately on save'
    : 'takes effect on the next restart';
  head.appendChild(badge);
  head.appendChild(el('span', 'mdl-group-hint', g.hint));
  const add = el('button', 'mdl-add', '+ add');
  add.type = 'button';
  add.onclick = () => {
    list.push({ name: '', model: '' });
    setPanelDirty('models', true);
    renderModels();
    focusModelRow(g.id, list.length - 1);
  };
  head.appendChild(add);
  wrap.appendChild(head);

  if (!list.length) {
    wrap.appendChild(el('div', 'mdl-empty', 'No models — this backend serves nothing.'));
    return wrap;
  }
  list.forEach((m, i) => wrap.appendChild(renderModelRow(g, m, i, list)));
  return wrap;
}

function renderModelRow(g, m, i, list) {
  const row = el('div', 'mdl-row' + (g.mapped ? '' : ' mdl-row-plain'));
  row.dataset.group = g.id;
  row.dataset.index = String(i);

  const name = document.createElement('input');
  name.className = 'mdl-name';
  name.value = m.name || '';
  name.placeholder = 'model name';
  name.spellcheck = false;
  name.oninput = () => { m.name = name.value; refreshPriceCell(row, g, m); setPanelDirty('models', true); };
  // Enter appends the next row and focuses it, so a list can be typed straight
  // through the way the old chip input allowed.
  name.onkeydown = e => {
    if (e.key !== 'Enter') return;
    e.preventDefault();
    if (i === list.length - 1) list.push({ name: '', model: '' });
    renderModels();
    focusModelRow(g.id, i + 1);
  };
  row.appendChild(name);

  if (g.mapped) row.appendChild(renderMapCell(row, g, m));

  const price = el('button', 'mdl-price');
  price.type = 'button';
  price.onclick = () => togglePriceEditor(row, g, m);
  row.appendChild(price);

  const del = el('button', 'mdl-del', '✕');
  del.type = 'button';
  del.title = 'Remove';
  del.onclick = () => { list.splice(i, 1); setPanelDirty('models', true); renderModels(); };
  row.appendChild(del);

  paintPriceCell(price, g, m);
  return row;
}

// The rename slot. One input, ghosted by CSS when empty — a click-to-create
// button would need a state machine to answer "what if they type nothing".
function renderMapCell(row, g, m) {
  const wrap = el('label', 'mdl-map');
  wrap.title = 'Optional: call the upstream model by a different name. ' +
    'Blank means the name on the left is sent as-is.';
  wrap.appendChild(el('span', 'mdl-map-mark', '↦'));
  const inp = document.createElement('input');
  inp.className = 'mdl-upstream';
  inp.value = m.model || '';
  inp.placeholder = 'upstream name';
  inp.spellcheck = false;
  const sync = () => wrap.classList.toggle('has-map', !!inp.value.trim());
  inp.oninput = () => {
    m.model = inp.value;
    sync();
    refreshPriceCell(row, g, m);
    setPanelDirty('models', true);
  };
  sync();
  wrap.appendChild(inp);
  return wrap;
}

function focusModelRow(groupId, index) {
  const row = document.querySelector(`.mdl-row[data-group="${groupId}"][data-index="${index}"]`);
  row?.querySelector('.mdl-name')?.focus();
}

function refreshPriceCell(row, g, m) {
  const cell = row.querySelector('.mdl-price');
  if (cell) paintPriceCell(cell, g, m);
}

function paintPriceCell(cell, g, m) {
  const r = resolvePrice(m.name, g.mapped ? m.model : m.name);
  cell.classList.toggle('is-custom', r?.source === 'custom');
  cell.classList.toggle('is-none', !r);
  if (!r) {
    cell.textContent = 'set price';
    // Unpriced is not cosmetic: those requests are missing from every cost
    // total on the dashboard, so the cell says what the consequence is.
    cell.title = 'No price for this model — its tokens are left out of all cost totals. Click to set one.';
    return;
  }
  cell.textContent = `${priceLabel(r.price.input)} / ${priceLabel(r.price.output)}`;
  // "matched X" is the line that stops the price looking arbitrary: a name can
  // resolve through a prefix (claude-opus-4-6-vertex → claude-opus-4-6) or
  // through its alias target, and neither is guessable from the row alone.
  const via = r.from !== normalizeModel(m.name)
    ? ` · matched ${r.viaAlias ? 'upstream ' : ''}${r.from}`
    : '';
  const cache = `cache read ${priceLabel(r.price.cache_read)} · write ${priceLabel(r.price.cache_write)}`;
  cell.title = (r.source === 'custom' ? 'Your price' : 'Built-in list price') +
    `${via} · ${cache} · click to ${r.source === 'custom' && !via ? 'edit' : 'override'}`;
}

// The editor drops in under its row rather than in a modal: you are usually
// fixing several models in one pass, and a dialog per model would mean opening
// and closing it four times.
function togglePriceEditor(row, g, m) {
  const open = row.nextElementSibling?.classList.contains('mdl-edit');
  document.querySelectorAll('.mdl-edit').forEach(e => e.remove());
  if (open) return;

  const key = normalizeModel(m.name);
  if (!key) { toast('Name the model first, then set its price.', 'err'); return; }
  const current = resolvePrice(m.name, g.mapped ? m.model : m.name);

  const box = el('div', 'mdl-edit');
  const fields = el('div', 'mdl-edit-fields');
  const inputs = {};
  for (const [field, label] of PRICE_FIELDS) {
    const f = el('label', 'mdl-edit-field');
    f.appendChild(el('span', null, label));
    const inp = document.createElement('input');
    inp.type = 'number';
    inp.min = '0';
    inp.step = '0.01';
    // Prefilled with whatever is in effect, so "adjust the output price" does
    // not silently zero the other three.
    inp.value = current ? String(current.price[field] ?? 0) : '';
    inp.placeholder = '0';
    inp.oninput = () => setPanelDirty('models', true);
    inputs[field] = inp;
    f.appendChild(inp);
    fields.appendChild(f);
  }
  box.appendChild(fields);

  const actions = el('div', 'mdl-edit-actions');
  actions.appendChild(el('span', 'mdl-edit-hint', 'USD per 1M tokens · saved to config.yaml on Save Config'));
  if (priceOverrides.has(key)) {
    const reset = el('button', 'btn-row', 'Use default');
    reset.type = 'button';
    reset.onclick = () => {
      priceOverrides.delete(key);
      box.remove();
      refreshPriceCell(row, g, m);
      setPanelDirty('models', true);
      toast('Reverted to the built-in price — save to persist.');
    };
    actions.appendChild(reset);
  }
  const apply = el('button', 'btn-row mdl-edit-apply', 'Apply');
  apply.type = 'button';
  apply.onclick = () => {
    const p = { name: (m.name || '').trim() };
    for (const [field] of PRICE_FIELDS) {
      const v = parseFloat(inputs[field].value);
      if (inputs[field].value !== '' && (!isFinite(v) || v < 0)) {
        toast(`${field} must be a non-negative number.`, 'err');
        return;
      }
      p[field] = inputs[field].value === '' ? 0 : v;
    }
    priceOverrides.set(key, p);
    box.remove();
    refreshPriceCell(row, g, m);
    setPanelDirty('models', true);
    toast('Price set — save to persist.');
  };
  actions.appendChild(apply);
  box.appendChild(actions);

  row.after(box);
  box.querySelector('input')?.focus();
}

async function loadConfig() {
  const [cfgRes, priceRes] = await Promise.all([apiFetch('/api/config'), apiFetch('/api/pricing')]);
  if (cfgRes.status === 401) { window.location.href = '/login'; return; }
  const d = await cfgRes.json();
  const pricing = priceRes.ok ? await priceRes.json() : { models: [] };

  priceTable = new Map();
  (pricing.models || []).forEach(p => priceTable.set(p.name, p));
  pricePrefixes = [...priceTable.keys()]
    .filter(k => k.length >= PRICE_MIN_PREFIX)
    .sort((a, b) => b.length - a.length || (a < b ? -1 : 1));

  priceOverrides = new Map();
  (d.pricing?.models || []).forEach(p => {
    const key = normalizeModel(p.name);
    if (key) priceOverrides.set(key, p);
  });

  // The Go structs marshal with capitalised keys in some paths; accept both.
  // An upstream equal to the name is not a rename — older configs spell identity
  // mappings out in full, and showing them as renames would suggest the two can
  // drift apart when nothing is actually mapped.
  const pairs = list => (list || []).map(m => {
    const name = m.Name ?? m.name ?? '';
    const model = m.Model ?? m.model ?? '';
    return { name, model: model === name ? '' : model };
  });
  cfgModels = {
    claude: (d.claude_oauth?.models || []).map(n => ({ name: n, model: '' })),
    codex: (d.codex?.models || []).map(n => ({ name: n, model: '' })),
    vertex: pairs(d.vertex?.models),
    kimi: pairs(d.kimi?.models),
  };
  renderModels();

  document.getElementById('cfg-admin-user').value = d.server?.admin_user || '';
  document.getElementById('cfg-admin-pass').value = '';
  document.getElementById('cfg-tray-token').value = d.server?.tray_token || '';
  ['cfg-admin-user', 'cfg-admin-pass', 'cfg-tray-token'].forEach(id => {
    document.getElementById(id).oninput = () => setPanelDirty('admin', true);
  });
  setPanelDirty('models', false);
  setPanelDirty('admin', false);
}

// Prefixed rather than random-looking so it is never mistaken for an `sk-` API
// key from the Keys page — the two credentials guard different planes.
function genTrayToken() {
  const buf = new Uint8Array(24);
  crypto.getRandomValues(buf);
  const hex = [...buf].map(b => b.toString(16).padStart(2, '0')).join('');
  document.getElementById('cfg-tray-token').value = 'tray-' + hex;
  setPanelDirty('admin', true);
  toast('Token generated — save, then paste it into the widget.');
}

function copyTrayToken(btn) {
  const val = document.getElementById('cfg-tray-token').value.trim();
  if (!val) { toast('Nothing to copy — generate a token first.', 'err'); return; }
  copyKeyInline(btn, val);
}

// Models and Admin are saved separately because they are separate decisions —
// rotating the admin password should not re-publish the model lists, and a
// failed model edit should not hold the password hostage. Every section of the
// PUT body is optional server-side, so each save sends only its own.
async function putConfig(body, btn, okMsg) {
  if (btn) { btn.disabled = true; btn.dataset.label = btn.textContent; btn.textContent = 'Saving…'; }
  try {
    const r = await apiFetch('/api/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) { toast(d.error || 'Save failed', 'err'); return null; }
    let msg = okMsg;
    if (d.restart_required && d.restart_required.length) {
      msg += ' · restart required for ' + d.restart_required.join(', ');
    }
    toast(msg, 'ok');
    return d;
  } catch (e) {
    toast('Save failed: ' + e.message, 'err');
    return null;
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = btn.dataset.label || 'Save'; }
  }
}

async function saveModels(btn) {
  const names = id => (cfgModels[id] || []).map(m => (m.name || '').trim()).filter(Boolean);
  const pairs = id => (cfgModels[id] || [])
    .map(m => ({ name: (m.name || '').trim(), model: (m.model || '').trim() }))
    .filter(m => m.name);
  // Overrides for models that are no longer listed here are kept, not pruned:
  // they may price a Codex model that auto-syncs at startup, or one served by a
  // backend this page does not edit. An unused row costs nothing.
  const d = await putConfig({
    claude_oauth: { models: names('claude') },
    codex: { models: names('codex') },
    vertex: { models: pairs('vertex') },
    kimi: { models: pairs('kimi') },
    pricing: { models: [...priceOverrides.values()] },
  }, btn, 'Models saved');
  if (d) setPanelDirty('models', false);
}

async function saveAdmin(btn) {
  const d = await putConfig({
    server: {
      admin_user: document.getElementById('cfg-admin-user').value.trim(),
      admin_password: document.getElementById('cfg-admin-pass').value,
      // Always sent, including empty: the box is prefilled, so clearing it means
      // revoke. The server treats a missing key (not an empty one) as "keep".
      tray_token: document.getElementById('cfg-tray-token').value.trim(),
    },
  }, btn, 'Admin settings saved');
  if (d) {
    document.getElementById('cfg-admin-pass').value = '';
    setPanelDirty('admin', false);
  }
}

// The dirty mark answers "did my edit register?" without a second click. It is
// per panel, because the two save independently.
function setPanelDirty(panel, dirty) {
  document.getElementById('save-' + panel)?.classList.toggle('is-dirty', !!dirty);
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

async function refreshQuota(provider, accountId) {
  const card = document.querySelector('[data-provider="' + provider + '"][data-account="' + accountId + '"]');
  if (card) card.style.opacity = '0.5';
  try {
    await apiFetch('/api/refresh-quota/' + encodeURIComponent(provider) + '/' + encodeURIComponent(accountId), { method: 'POST' });
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

let keysCache = [];
async function loadKeys() {
  const tz = -new Date().getTimezoneOffset(); // minutes east of UTC — align "today" with the tray widget
  const r = await apiFetch('/api/keys?tz=' + tz);
  const d = await r.json();
  keysCache = d.keys || [];
  const body = document.getElementById('keys-body');
  if (!d.keys || !d.keys.length) {
    body.innerHTML = '<tr><td colspan="10" class="text-muted" style="text-align:center;padding:20px">No API keys yet — create one above</td></tr>';
    return;
  }
  const limitColorFor = (used, limit) => {
    if (!limit) return '';
    const pct = Math.round((used || 0) / limit * 100);
    return pct > 90 ? 'var(--red)' : pct > 70 ? 'var(--yellow)' : '';
  };
  body.innerHTML = d.keys.map(k => {
    const tokLimit = k.token_limit_daily ? k.token_limit_daily.toLocaleString() + ' tok' : '∞ tok';
    const reqLimit = k.request_limit_daily ? k.request_limit_daily.toLocaleString() + ' req' : '∞ req';
    // Colour by quota_used_today, not tokens_today: the daily limit is enforced
    // on the cache-exclusive figure, so grading the larger display total against
    // it would warn about a limit that is nowhere near being hit.
    const tokColor = limitColorFor(k.quota_used_today, k.token_limit_daily);
    const reqColor = limitColorFor(k.requests_today, k.request_limit_daily);
    const tokTitle = k.token_limit_daily
      ? `${(k.quota_used_today || 0).toLocaleString()} / ${k.token_limit_daily.toLocaleString()} counted against the daily limit (cache reads excluded)`
      : 'includes cache tokens; the daily limit counts only non-cache input + output';
    const dis = k.disabled;
    const toggleBtn = '<button class="btn-row" style="' + (dis ? 'border-color:var(--accent);color:var(--accent)' : '') + '" onclick="toggleKey(\'' + k.id + '\')">' + (dis ? '▶ Enable' : '⏸ Disable') + '</button>';
    return '<tr style="' + (dis ? 'opacity:0.5' : '') + '">'
      + '<td class="text-mono">' + k.name + (dis ? ' <span class="key-badge">disabled</span>' : '') + '</td>'
      + '<td class="text-muted text-mono" style="font-size:11px"><span style="vertical-align:middle">' + k.key.slice(0,10) + '...' + k.key.slice(-4) + '</span> <button class="icon-btn" title="Copy key" onclick="copyKeyInline(this, \'' + k.key + '\')">&#x2398;</button></td>'
      + '<td>' + (k.request_count || 0).toLocaleString() + '</td>'
      + '<td style="' + (reqColor ? 'color:'+reqColor : '') + '">' + (k.requests_today || 0).toLocaleString() + '</td>'
      + '<td style="' + (tokColor ? 'color:'+tokColor : '') + '" title="' + escapeHTML((k.tokens_today || 0).toLocaleString() + ' — ' + tokTitle) + '">' + fmtCompact(k.tokens_today || 0) + '</td>'
      + '<td title="' + (k.total_tokens || 0).toLocaleString() + ' tokens">' + fmtCompact(k.total_tokens || 0) + '</td>'
      + '<td title="' + escapeHTML(COST_HINT) + '">' + fmtMoney(k.cost_today || 0) + '</td>'
      + '<td title="' + escapeHTML(COST_HINT) + '">' + fmtMoney(k.cost_usd || 0) + '</td>'
      + '<td style="line-height:1.5"><div>' + reqLimit + '</div><div>' + tokLimit + '</div></td>'
      + '<td style="white-space:nowrap"><button class="btn-row" onclick="editKey(\'' + k.id + '\')">&#x270E; Edit</button> ' + toggleBtn + ' <button class="btn-row" onclick="deleteKey(\'' + k.id + '\')">&#x1F5D1; Delete</button></td>'
      + '</tr>';
  }).join('');
}

async function toggleKey(id) {
  await apiFetch('/api/keys/' + id + '/toggle', { method: 'POST' });
  loadKeys();
}

function editKey(id) {
  const k = keysCache.find(x => x.id === id);
  if (!k) return;
  document.getElementById('key-modal-title').textContent = 'Edit API Key';
  document.getElementById('key-created').style.display = 'none';
  document.getElementById('create-key-fields').style.display = '';
  document.getElementById('key-name').value = k.name;
  document.getElementById('key-limit').value = k.token_limit_daily || 0;
  document.getElementById('key-req-limit').value = k.request_limit_daily || 0;
  const btn = document.getElementById('create-key-submit');
  btn.textContent = 'Save';
  btn.setAttribute('onclick', "submitEditKey('" + id + "')");
  document.getElementById('create-key-modal').classList.add('show');
  document.getElementById('key-name').focus();
}

async function submitEditKey(id) {
  const name = document.getElementById('key-name').value.trim();
  if (!name) { document.getElementById('key-name').focus(); return; }
  const limit = parseInt(document.getElementById('key-limit').value) || 0;
  const reqLimit = parseInt(document.getElementById('key-req-limit').value) || 0;
  await apiFetch('/api/keys/' + id, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name: name, token_limit_daily: limit, request_limit_daily: reqLimit })
  });
  closeCreateKey();
  loadKeys();
}

function openCreateKey() {
  document.getElementById('key-modal-title').textContent = 'Create API Key';
  document.getElementById('key-created').style.display = 'none';
  document.getElementById('create-key-fields').style.display = '';
  document.getElementById('key-name').value = '';
  document.getElementById('key-limit').value = '0';
  document.getElementById('key-req-limit').value = '0';
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
  const reqLimit = parseInt(document.getElementById('key-req-limit').value) || 0;
  const r = await apiFetch('/api/keys', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({name: name, token_limit_daily: limit, request_limit_daily: reqLimit})
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

document.querySelectorAll('.model-select, .image-select').forEach(enhanceSelect);

loadStatus();
let lastFocusLoad = 0;
window.addEventListener('focus', () => {
  if (Date.now() - lastFocusLoad > 30000) { lastFocusLoad = Date.now(); loadStatus(); }
});

// Keep the dashboard current without manual refocus: poll while the tab is
// visible so server-side state changes (a backend pausing, an account
// rate-limiting or recovering at its reset) reflect within the interval
// instead of only on focus/action. Skipped when hidden to avoid waste. Must not
// touch lastFocusLoad — otherwise it suppresses the immediate on-focus refresh,
// making a returning tab wait a full tick before catching up.
setInterval(() => {
  if (document.visibilityState === 'visible') loadStatus();
}, 15000);

// Width-filling charts re-render on resize while the Stats tab is visible.
let resizeTimer = 0;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => {
    const tab = document.getElementById('tab-stats');
    if (statsData && tab && !tab.classList.contains('hidden')) { renderTrend(); renderCalendar(); }
  }, 150);
});
