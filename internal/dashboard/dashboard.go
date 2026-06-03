package dashboard

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(dashboardHTML))
	}
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>CLI Proxy Dashboard</title>
<script src="https://cdn.tailwindcss.com"></script>
<style>
  body { background: #0f172a; color: #e2e8f0; font-family: -apple-system, system-ui, sans-serif; }
  .card { background: #1e293b; border: 1px solid #334155; border-radius: 12px; }
  .status-dot { width: 10px; height: 10px; border-radius: 50%; display: inline-block; }
  .status-active { background: #22c55e; box-shadow: 0 0 6px #22c55e; }
  .status-expired { background: #eab308; box-shadow: 0 0 6px #eab308; }
  .status-inactive { background: #64748b; }
  .btn { padding: 6px 16px; border-radius: 8px; font-size: 14px; cursor: pointer; transition: all 0.15s; }
  .btn-primary { background: #3b82f6; color: white; }
  .btn-primary:hover { background: #2563eb; }
  .btn-green { background: #22c55e; color: white; }
  .btn-green:hover { background: #16a34a; }
  #chat-output { min-height: 120px; max-height: 400px; overflow-y: auto; white-space: pre-wrap; word-break: break-word; }
  table { width: 100%; border-collapse: collapse; }
  th { text-align: left; padding: 8px 12px; color: #94a3b8; font-weight: 500; font-size: 13px; border-bottom: 1px solid #334155; }
  td { padding: 8px 12px; font-size: 13px; border-bottom: 1px solid #1e293b; }
  tr:hover td { background: #1e293b; }
  select, textarea { background: #0f172a; border: 1px solid #334155; color: #e2e8f0; border-radius: 8px; padding: 8px 12px; }
  select:focus, textarea:focus { outline: none; border-color: #3b82f6; }
  .tab { padding: 8px 20px; cursor: pointer; border-bottom: 2px solid transparent; color: #94a3b8; }
  .tab.active { border-color: #3b82f6; color: #e2e8f0; }
  .tab:hover { color: #e2e8f0; }
</style>
</head>
<body class="p-6">
<div class="max-w-6xl mx-auto">
  <!-- Header -->
  <div class="flex items-center justify-between mb-6">
    <div>
      <h1 class="text-2xl font-bold">CLI Proxy</h1>
      <p class="text-sm text-slate-400 mt-1">AI API Proxy Dashboard</p>
    </div>
    <div class="text-right text-sm text-slate-400">
      <span id="total-requests">-</span> requests &middot; <span id="total-tokens">-</span> tokens
    </div>
  </div>

  <!-- Backend Status Cards -->
  <div id="backends" class="grid grid-cols-1 md:grid-cols-3 gap-4 mb-6"></div>

  <!-- Tabs -->
  <div class="flex gap-1 mb-4 border-b border-slate-700">
    <div class="tab active" onclick="switchTab('chat')">Test Chat</div>
    <div class="tab" onclick="switchTab('logs')">Request Logs</div>
    <div class="tab" onclick="switchTab('stats')">Usage Stats</div>
    <div class="tab" onclick="switchTab('config')">Config</div>
  </div>

  <!-- Chat Tab -->
  <div id="tab-chat">
    <div class="card p-4">
      <div class="flex gap-3 mb-3">
        <select id="chat-model" class="flex-1"></select>
        <button class="btn btn-primary" onclick="sendChat()">Send</button>
        <button class="btn" style="background:#334155" onclick="clearChat()">Clear</button>
      </div>
      <textarea id="chat-input" rows="2" class="w-full mb-3" placeholder="Enter your message..."></textarea>
      <div id="chat-output" class="card p-3 text-sm font-mono text-slate-300 bg-slate-900"></div>
    </div>
  </div>

  <!-- Logs Tab -->
  <div id="tab-logs" class="hidden">
    <div class="card overflow-hidden">
      <div class="flex items-center justify-between p-3 border-b border-slate-700">
        <span class="text-sm text-slate-400"><span id="log-total">0</span> total requests</span>
        <button class="btn btn-primary text-xs" onclick="loadLogs()">Refresh</button>
      </div>
      <div class="overflow-x-auto">
        <table>
          <thead><tr>
            <th>Time</th><th>Model</th><th>Backend</th><th>Latency</th><th>Tokens</th><th>Status</th>
          </tr></thead>
          <tbody id="log-body"></tbody>
        </table>
      </div>
      <div class="flex justify-center gap-2 p-3">
        <button class="btn text-xs" style="background:#334155" onclick="prevPage()">Prev</button>
        <span id="page-info" class="text-sm text-slate-400 py-1"></span>
        <button class="btn text-xs" style="background:#334155" onclick="nextPage()">Next</button>
      </div>
    </div>
  </div>

  <!-- Stats Tab -->
  <div id="tab-stats" class="hidden">
    <div class="flex gap-2 mb-4">
      <button class="btn text-xs btn-primary" onclick="loadStats('today')">Today</button>
      <button class="btn text-xs" style="background:#334155" onclick="loadStats('7d')">7 Days</button>
      <button class="btn text-xs" style="background:#334155" onclick="loadStats('30d')">30 Days</button>
      <button class="btn text-xs" style="background:#334155" onclick="loadStats('all')">All</button>
    </div>
    <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
      <div class="card p-4">
        <h3 class="text-sm font-medium text-slate-400 mb-3">By Model</h3>
        <table>
          <thead><tr><th>Model</th><th>Requests</th><th>Tokens</th><th>Avg Latency</th><th>Errors</th></tr></thead>
          <tbody id="stats-model-body"></tbody>
        </table>
      </div>
      <div class="card p-4">
        <h3 class="text-sm font-medium text-slate-400 mb-3">By Day</h3>
        <table>
          <thead><tr><th>Date</th><th>Requests</th><th>Tokens</th><th>Errors</th></tr></thead>
          <tbody id="stats-day-body"></tbody>
        </table>
      </div>
    </div>
  </div>

  <!-- Config Tab -->
  <div id="tab-config" class="hidden">
    <div class="card p-4">
      <pre id="config-display" class="text-sm font-mono text-slate-300 whitespace-pre-wrap"></pre>
    </div>
  </div>
</div>

<script>
let logPage = 0;
const logLimit = 30;

async function loadStatus() {
  const r = await fetch('/api/status');
  const d = await r.json();
  document.getElementById('total-requests').textContent = d.total_requests?.toLocaleString() || '0';
  document.getElementById('total-tokens').textContent = d.total_tokens?.toLocaleString() || '0';

  const container = document.getElementById('backends');
  container.innerHTML = d.backends.map(b => {
    const dot = b.status === 'active' ? 'status-active' : b.status === 'expired' ? 'status-expired' : 'status-inactive';
    const label = b.status === 'active' ? 'Active' : b.status === 'expired' ? 'Expired' : 'Not Connected';
    const isOAuth = b.name === 'claude' || b.name === 'codex';
    const addBtn = isOAuth ? '<a href="/auth/' + b.name + '" class="btn btn-green text-xs">+ Add Account</a>' : '';
    let accountsHTML = '';
    if (b.accounts && b.accounts.length > 0) {
      accountsHTML = '<div class="mt-2 space-y-1">' + b.accounts.map(a => {
        const aDot = a.status === 'active' ? 'status-active' : 'status-expired';
        const exp = a.expires ? ' exp ' + a.expires : '';
        return '<div class="flex items-center gap-2 text-xs py-1 px-2 rounded" style="background:#0f172a">'
          + '<span class="status-dot ' + aDot + '"></span>'
          + '<span class="flex-1 truncate">' + a.email + exp + '</span>'
          + '<button onclick="removeAccount(\'' + b.name + '\',\'' + a.id + '\')" class="text-red-400 hover:text-red-300 text-xs">x</button>'
          + '</div>';
      }).join('') + '</div>';
    }
    return '<div class="card p-4"><div class="flex items-center gap-2 mb-2"><span class="status-dot ' + dot + '"></span>'
      + '<span class="font-medium capitalize">' + b.name + '</span>'
      + '<span class="text-xs text-slate-400 ml-auto">' + label + '</span></div>'
      + '<div class="text-xs text-slate-400 mb-1">' + (b.info || '') + '</div>'
      + '<div class="text-xs text-slate-500 mb-1">' + (b.models || []).join(', ') + '</div>'
      + accountsHTML
      + (isOAuth ? '<div class="mt-2">' + addBtn + '</div>' : '')
      + '</div>';
  }).join('');

  const select = document.getElementById('chat-model');
  const statusLabel = s => s === 'active' ? '✅' : s === 'expired' ? '⚠️' : '🔴';
  select.innerHTML = d.backends.map(b => {
    const label = b.name.charAt(0).toUpperCase() + b.name.slice(1) + ' ' + statusLabel(b.status);
    const opts = (b.models || []).map(m =>
      '<option value="' + m + '"' + (b.status !== 'active' ? ' disabled' : '') + '>' + m + '</option>'
    ).join('');
    return '<optgroup label="' + label + '">' + opts + '</optgroup>';
  }).join('');
  // Select first enabled option
  const firstEnabled = select.querySelector('option:not([disabled])');
  if (firstEnabled) firstEnabled.selected = true;
}

async function sendChat() {
  const model = document.getElementById('chat-model').value;
  const input = document.getElementById('chat-input').value.trim();
  if (!input) return;
  const output = document.getElementById('chat-output');
  output.textContent = '';

  const resp = await fetch('/v1/chat/completions', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({model, messages: [{role: 'user', content: input}], stream: true})
  });

  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  while (true) {
    const {done, value} = await reader.read();
    if (done) break;
    buf += decoder.decode(value, {stream: true});
    const lines = buf.split('\n');
    buf = lines.pop();
    for (const line of lines) {
      if (!line.startsWith('data: ') || line === 'data: [DONE]') continue;
      try {
        const chunk = JSON.parse(line.slice(6));
        const content = chunk.choices?.[0]?.delta?.content;
        if (content) output.textContent += content;
      } catch {}
    }
  }
  loadLogs();
  loadStatus();
}

function clearChat() {
  document.getElementById('chat-output').textContent = '';
  document.getElementById('chat-input').value = '';
}

async function loadLogs() {
  const r = await fetch('/api/logs?limit=' + logLimit + '&offset=' + (logPage * logLimit));
  const d = await r.json();
  document.getElementById('log-total').textContent = d.total;
  document.getElementById('page-info').textContent = 'Page ' + (logPage + 1) + ' / ' + Math.max(1, Math.ceil(d.total / logLimit));

  const body = document.getElementById('log-body');
  body.innerHTML = (d.logs || []).map(l => {
    const t = new Date(l.time).toLocaleString();
    const tokens = (l.prompt_tokens || 0) + (l.completion_tokens || 0);
    const statusColor = l.status < 400 ? 'text-green-400' : 'text-red-400';
    const latency = l.latency_ms + 'ms';
    return '<tr><td class="text-slate-400">' + t + '</td><td>' + l.model + '</td><td class="text-slate-400">' + l.backend
      + '</td><td>' + latency + '</td><td>' + tokens + '</td><td class="' + statusColor + '">' + l.status
      + (l.error ? ' <span class="text-red-400 text-xs" title="' + l.error.replace(/"/g, '') + '">!</span>' : '') + '</td></tr>';
  }).join('');
}

function prevPage() { if (logPage > 0) { logPage--; loadLogs(); } }
function nextPage() { logPage++; loadLogs(); }

async function loadStats(range) {
  const r = await fetch('/api/stats?range=' + (range || '7d'));
  const d = await r.json();

  document.getElementById('stats-model-body').innerHTML = (d.by_model || []).map(s =>
    '<tr><td>' + s.model + '</td><td>' + s.request_count + '</td><td>'
    + (s.total_prompt_tokens + s.total_completion_tokens).toLocaleString()
    + '</td><td>' + Math.round(s.avg_latency_ms) + 'ms</td><td class="' + (s.error_count > 0 ? 'text-red-400' : '') + '">' + s.error_count + '</td></tr>'
  ).join('') || '<tr><td colspan="5" class="text-slate-500 text-center py-4">No data</td></tr>';

  document.getElementById('stats-day-body').innerHTML = (d.by_day || []).map(s =>
    '<tr><td>' + s.date + '</td><td>' + s.request_count + '</td><td>'
    + (s.total_prompt_tokens + s.total_completion_tokens).toLocaleString()
    + '</td><td class="' + (s.error_count > 0 ? 'text-red-400' : '') + '">' + s.error_count + '</td></tr>'
  ).join('') || '<tr><td colspan="4" class="text-slate-500 text-center py-4">No data</td></tr>';
}

async function loadConfig() {
  const r = await fetch('/api/config');
  const d = await r.json();
  document.getElementById('config-display').textContent = JSON.stringify(d, null, 2);
}

function switchTab(name) {
  document.querySelectorAll('[id^="tab-"]').forEach(el => el.classList.add('hidden'));
  document.querySelectorAll('.tab').forEach(el => el.classList.remove('active'));
  document.getElementById('tab-' + name).classList.remove('hidden');
  event.target.classList.add('active');
  if (name === 'logs') loadLogs();
  if (name === 'stats') loadStats('7d');
  if (name === 'config') loadConfig();
}

async function removeAccount(provider, id) {
  if (!confirm('Remove account ' + id + '?')) return;
  await fetch('/api/accounts/' + provider + '/' + encodeURIComponent(id), {method: 'DELETE'});
  loadStatus();
}

loadStatus();
setInterval(loadStatus, 30000);
</script>
</body>
</html>`
