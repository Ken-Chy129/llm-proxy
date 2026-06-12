let logPage = 0;
const logLimit = 30;

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
  el.innerHTML = d.backends.map(b => {
    const bc = b.status === 'active' ? 'badge-active' : b.status === 'expired' ? 'badge-expired' : 'badge-inactive';
    const bl = b.status === 'active' ? 'Active' : b.status === 'expired' ? 'Expired' : 'Offline';
    const dc = b.status === 'active' ? 'dot-green' : b.status === 'expired' ? 'dot-yellow' : 'dot-gray';
    const isOAuth = b.name === 'claude' || b.name === 'codex';
    let accts = '';
    if (b.accounts && b.accounts.length) {
      accts = b.accounts.map(a => {
        const ad = a.status === 'active' ? 'dot-green' : 'dot-yellow';
        return `<div class="account-row"><span class="dot ${ad}"></span><span class="email">${a.email}</span>`
          + (a.expires ? `<span class="exp">${a.expires}</span>` : '')
          + `<button class="btn-delete" onclick="removeAccount('${b.name}','${a.id}')">&times;</button></div>`;
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
    return `<div class="backend-card"><div class="backend-header"><span class="dot ${dc}"></span><span class="backend-name" style="text-transform:capitalize">${b.name}</span><span class="backend-badge ${bc}">${bl}</span></div>`
      + `<div class="backend-info">${b.info || ''}</div>`
      + `<div class="backend-models">${(b.models || []).map(m => `<span class="model-tag">${m}</span>`).join('')}</div>`
      + accts + `<div style="display:flex;gap:4px;flex-wrap:wrap">${addBtn}${syncBtn}</div></div>`;
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
  const statusIcon = s => s === 'active' ? '✓' : s === 'expired' ? '!' : '✗';
  sel.innerHTML = d.backends.map(b => {
    const lbl = b.name.charAt(0).toUpperCase() + b.name.slice(1) + ' (' + statusIcon(b.status) + ')';
    return `<optgroup label="${lbl}">${(b.models || []).map(m =>
      `<option value="${m}"${b.status !== 'active' ? ' disabled' : ''}>${m}</option>`
    ).join('')}</optgroup>`;
  }).join('');
  const first = sel.querySelector('option:not([disabled])');
  if (first) first.selected = true;
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
        try { const c = JSON.parse(line.slice(6)); const t = c.choices?.[0]?.delta?.content; if (t) output.textContent += t; } catch {}
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
    return `<tr><td class="text-muted text-mono">${t}</td><td class="text-mono">${l.model}</td><td class="text-muted">${l.backend}</td><td>${l.latency_ms}ms</td><td>${tok}</td><td class="${sc}">${l.status}</td></tr>`;
  }).join('') || '<tr><td colspan="6" class="text-muted" style="text-align:center;padding:24px">No requests yet</td></tr>';
}

function prevPage() { if (logPage > 0) { logPage--; loadLogs(); } }
function nextPage() { logPage++; loadLogs(); }

async function loadStats(range, btn) {
  if (btn) { document.querySelectorAll('.stats-btn').forEach(b => b.classList.remove('active')); btn.classList.add('active'); }
  const r = await apiFetch('/api/stats?range=' + (range || '7d'));
  const d = await r.json();
  const empty = '<tr><td colspan="5" class="text-muted" style="text-align:center;padding:20px">No data</td></tr>';
  document.getElementById('stats-model-body').innerHTML = (d.by_model || []).map(s =>
    `<tr><td class="text-mono">${s.model}</td><td>${s.request_count}</td><td>${(s.total_prompt_tokens + s.total_completion_tokens).toLocaleString()}</td><td>${Math.round(s.avg_latency_ms)}</td><td class="${s.error_count ? 'text-red' : ''}">${s.error_count}</td></tr>`
  ).join('') || empty;
  document.getElementById('stats-day-body').innerHTML = (d.by_day || []).map(s =>
    `<tr><td class="text-mono">${s.date}</td><td>${s.request_count}</td><td>${(s.total_prompt_tokens + s.total_completion_tokens).toLocaleString()}</td><td class="${s.error_count ? 'text-red' : ''}">${s.error_count}</td></tr>`
  ).join('') || empty.replace('5', '4');
}

async function loadConfig() {
  const r = await apiFetch('/api/config');
  const d = await r.json();
  document.getElementById('config-display').textContent = JSON.stringify(d, null, 2);
}

function switchTab(name, el) {
  document.querySelectorAll('[id^="tab-"]').forEach(e => e.classList.add('hidden'));
  document.querySelectorAll('.tab').forEach(e => e.classList.remove('active'));
  document.getElementById('tab-' + name).classList.remove('hidden');
  if (el) el.classList.add('active');
  if (name === 'logs') loadLogs();
  if (name === 'stats') loadStats('7d');
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
  if (!url) { document.getElementById('modal-error').textContent = 'Please paste the callback URL'; return; }
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

async function removeAccount(provider, id) {
  if (!confirm('Remove ' + id + '?')) return;
  await apiFetch('/api/accounts/' + provider + '/' + encodeURIComponent(id), { method: 'DELETE' });
  loadStatus();
}

loadStatus();
let lastFocusLoad = 0;
window.addEventListener('focus', () => {
  if (Date.now() - lastFocusLoad > 30000) { lastFocusLoad = Date.now(); loadStatus(); }
});
