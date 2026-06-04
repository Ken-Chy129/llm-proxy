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
<title>CLI Proxy</title>
<style>
  :root {
    --bg-0: #0a0a0f;
    --bg-1: #12121a;
    --bg-2: #1a1a26;
    --bg-3: #24243a;
    --border: #2a2a3e;
    --border-hover: #3a3a55;
    --text-0: #eeeef2;
    --text-1: #b0b0c0;
    --text-2: #707088;
    --accent: #5b8aff;
    --accent-dim: #3a5ccc;
    --green: #34d399;
    --green-bg: rgba(52,211,153,0.1);
    --red: #f87171;
    --red-bg: rgba(248,113,113,0.1);
    --yellow: #fbbf24;
    --yellow-bg: rgba(251,191,36,0.1);
  }
  * { margin:0; padding:0; box-sizing:border-box; }
  body { background:var(--bg-0); color:var(--text-0); font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif; font-size:14px; line-height:1.5; }
  .container { max-width:1120px; margin:0 auto; padding:24px 20px; }

  /* Header */
  .header { display:flex; align-items:center; justify-content:space-between; margin-bottom:24px; padding-bottom:16px; border-bottom:1px solid var(--border); }
  .header h1 { font-size:18px; font-weight:600; letter-spacing:-0.3px; }
  .header-stats { display:flex; gap:16px; }
  .header-stat { text-align:right; }
  .header-stat-value { font-size:20px; font-weight:600; font-variant-numeric:tabular-nums; }
  .header-stat-label { font-size:11px; color:var(--text-2); text-transform:uppercase; letter-spacing:0.5px; }

  /* Backend Grid */
  .backends { display:grid; grid-template-columns:repeat(auto-fit,minmax(300px,1fr)); gap:12px; margin-bottom:24px; }
  .backend-card { background:var(--bg-1); border:1px solid var(--border); border-radius:8px; padding:16px; transition:border-color 0.15s; }
  .backend-card:hover { border-color:var(--border-hover); }
  .backend-header { display:flex; align-items:center; gap:8px; margin-bottom:10px; }
  .backend-name { font-weight:600; font-size:14px; }
  .backend-badge { font-size:11px; padding:2px 8px; border-radius:10px; font-weight:500; margin-left:auto; }
  .badge-active { background:var(--green-bg); color:var(--green); }
  .badge-expired { background:var(--yellow-bg); color:var(--yellow); }
  .badge-inactive { background:var(--bg-3); color:var(--text-2); }
  .backend-info { font-size:12px; color:var(--text-2); margin-bottom:8px; }
  .backend-models { display:flex; flex-wrap:wrap; gap:4px; margin-bottom:10px; }
  .model-tag { font-size:11px; padding:2px 7px; background:var(--bg-3); border-radius:4px; color:var(--text-1); font-family:'SF Mono',Menlo,monospace; }
  .account-row { display:flex; align-items:center; gap:6px; font-size:12px; padding:5px 8px; background:var(--bg-0); border-radius:5px; margin-bottom:4px; }
  .account-row .email { flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; color:var(--text-1); }
  .account-row .exp { color:var(--text-2); }
  .dot { width:7px; height:7px; border-radius:50%; flex-shrink:0; }
  .dot-green { background:var(--green); }
  .dot-yellow { background:var(--yellow); }
  .dot-gray { background:var(--text-2); }
  .btn-delete { background:none; border:none; color:var(--text-2); cursor:pointer; font-size:14px; padding:0 2px; line-height:1; }
  .btn-delete:hover { color:var(--red); }
  .btn-add { display:inline-flex; align-items:center; gap:4px; font-size:12px; color:var(--accent); background:none; border:1px solid var(--accent-dim); border-radius:5px; padding:4px 10px; cursor:pointer; text-decoration:none; margin-top:4px; transition:background 0.15s; }
  .btn-add:hover { background:rgba(91,138,255,0.1); }

  /* Tabs */
  .tabs { display:flex; gap:0; border-bottom:1px solid var(--border); margin-bottom:16px; }
  .tab { padding:8px 16px; font-size:13px; color:var(--text-2); cursor:pointer; border-bottom:2px solid transparent; transition:all 0.15s; user-select:none; }
  .tab:hover { color:var(--text-1); }
  .tab.active { color:var(--text-0); border-color:var(--accent); }

  /* Chat Panel */
  .chat-panel { background:var(--bg-1); border:1px solid var(--border); border-radius:8px; overflow:hidden; }
  .chat-toolbar { display:flex; gap:8px; padding:12px; border-bottom:1px solid var(--border); align-items:center; }
  .model-select { flex:1; appearance:none; background:var(--bg-2) url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='12' fill='%23707088'%3E%3Cpath d='M2 4l4 4 4-4'/%3E%3C/svg%3E") no-repeat right 10px center; border:1px solid var(--border); color:var(--text-0); border-radius:6px; padding:7px 28px 7px 10px; font-size:13px; font-family:inherit; cursor:pointer; }
  .model-select:focus { outline:none; border-color:var(--accent); }
  .model-select optgroup { color:var(--text-2); font-style:normal; font-size:11px; }
  .model-select option { color:var(--text-0); background:var(--bg-2); padding:4px 8px; }
  .model-select option:disabled { color:var(--text-2); }
  .btn { padding:7px 14px; border-radius:6px; font-size:13px; font-weight:500; cursor:pointer; border:none; transition:background 0.15s; }
  .btn-primary { background:var(--accent); color:#fff; }
  .btn-primary:hover { background:var(--accent-dim); }
  .btn-secondary { background:var(--bg-3); color:var(--text-1); }
  .btn-secondary:hover { background:var(--border); }
  .chat-input { display:block; width:100%; background:transparent; border:none; border-bottom:1px solid var(--border); color:var(--text-0); padding:12px; font-size:13px; font-family:inherit; resize:vertical; min-height:48px; }
  .chat-input:focus { outline:none; border-color:var(--accent); }
  .chat-input::placeholder { color:var(--text-2); }
  .chat-output { padding:12px; min-height:100px; max-height:400px; overflow-y:auto; font-size:13px; line-height:1.6; white-space:pre-wrap; word-break:break-word; color:var(--text-1); font-family:'SF Mono',Menlo,Consolas,monospace; }
  .chat-output:empty::before { content:'Response will appear here...'; color:var(--text-2); font-style:italic; font-family:inherit; }

  /* Tables */
  .panel { background:var(--bg-1); border:1px solid var(--border); border-radius:8px; overflow:hidden; }
  .panel-header { display:flex; align-items:center; justify-content:space-between; padding:12px 16px; border-bottom:1px solid var(--border); }
  .panel-header span { font-size:12px; color:var(--text-2); }
  table { width:100%; border-collapse:collapse; }
  th { text-align:left; padding:8px 16px; font-size:11px; font-weight:500; color:var(--text-2); text-transform:uppercase; letter-spacing:0.5px; border-bottom:1px solid var(--border); }
  td { padding:7px 16px; font-size:13px; border-bottom:1px solid rgba(42,42,62,0.5); font-variant-numeric:tabular-nums; }
  tr:hover td { background:rgba(255,255,255,0.02); }
  .text-green { color:var(--green); }
  .text-red { color:var(--red); }
  .text-muted { color:var(--text-2); }
  .text-mono { font-family:'SF Mono',Menlo,monospace; font-size:12px; }

  /* Stats */
  .stats-toolbar { display:flex; gap:4px; margin-bottom:12px; }
  .stats-btn { padding:5px 12px; font-size:12px; border-radius:5px; border:1px solid var(--border); background:transparent; color:var(--text-1); cursor:pointer; }
  .stats-btn.active { background:var(--accent); border-color:var(--accent); color:#fff; }
  .stats-grid { display:grid; grid-template-columns:1fr 1fr; gap:12px; }

  /* Pagination */
  .pagination { display:flex; align-items:center; justify-content:center; gap:8px; padding:10px 16px; }
  .page-btn { padding:4px 10px; font-size:12px; border-radius:4px; border:1px solid var(--border); background:transparent; color:var(--text-1); cursor:pointer; }
  .page-btn:hover { background:var(--bg-3); }
  .page-info { font-size:12px; color:var(--text-2); }

  /* Config */
  .config-display { padding:16px; font-size:12px; font-family:'SF Mono',Menlo,monospace; color:var(--text-1); white-space:pre-wrap; line-height:1.7; }

  /* Quota Cards */
  .quota-section { margin-top:8px; }
  .quota-section h3 { font-size:13px; font-weight:600; margin-bottom:8px; color:var(--text-1); }
  .quota-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(280px,1fr)); gap:10px; }
  .quota-card { background:var(--bg-1); border:1px solid var(--border); border-radius:8px; padding:14px; }
  .quota-card-header { display:flex; align-items:center; gap:6px; margin-bottom:10px; font-size:13px; }
  .quota-card-header .plan-badge { font-size:11px; padding:2px 8px; border-radius:10px; font-weight:500; }
  .plan-team { background:rgba(91,138,255,0.15); color:var(--accent); }
  .plan-pro { background:rgba(251,191,36,0.15); color:var(--yellow); }
  .plan-plus { background:var(--green-bg); color:var(--green); }
  .quota-row { margin-bottom:8px; }
  .quota-row-header { display:flex; justify-content:space-between; font-size:12px; margin-bottom:3px; }
  .quota-row-label { color:var(--text-1); font-weight:500; }
  .quota-row-value { color:var(--text-2); }
  .quota-row-value .pct { color:var(--text-0); font-weight:500; margin-right:4px; }
  .quota-bar { height:4px; background:var(--bg-3); border-radius:2px; overflow:hidden; }
  .quota-bar-fill { height:100%; border-radius:2px; transition: width 0.3s; }

  .hidden { display:none; }
  @media(max-width:768px) {
    .backends { grid-template-columns:1fr; }
    .stats-grid { grid-template-columns:1fr; }
    .header { flex-direction:column; align-items:flex-start; gap:12px; }
    .header-stats { align-self:flex-end; }
  }
</style>
</head>
<body>
<div class="container">
  <div class="header">
    <h1>CLI Proxy</h1>
    <div class="header-stats">
      <div class="header-stat">
        <div class="header-stat-value" id="total-requests">-</div>
        <div class="header-stat-label">Requests</div>
      </div>
      <div class="header-stat">
        <div class="header-stat-value" id="total-tokens">-</div>
        <div class="header-stat-label">Tokens</div>
      </div>
    </div>
  </div>

  <div class="backends" id="backends"></div>

  <!-- Quota Cards Section -->
  <div id="quota-section" class="quota-section" style="display:none;margin-bottom:20px">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">
      <h3 style="margin:0">Codex Quota</h3>
      <button class="btn btn-secondary" style="padding:3px 10px;font-size:11px" onclick="syncModels()">Refresh</button>
    </div>
    <div id="quota-grid" class="quota-grid"></div>
  </div>

  <div class="tabs">
    <div class="tab active" onclick="switchTab('chat',this)">Chat</div>
    <div class="tab" onclick="switchTab('logs',this)">Logs</div>
    <div class="tab" onclick="switchTab('stats',this)">Stats</div>
    <div class="tab" onclick="switchTab('config',this)">Config</div>
  </div>

  <div id="tab-chat">
    <div class="chat-panel">
      <div class="chat-toolbar">
        <select id="chat-model" class="model-select"></select>
        <button class="btn btn-primary" onclick="sendChat()">Send</button>
        <button class="btn btn-secondary" onclick="clearChat()">Clear</button>
      </div>
      <textarea id="chat-input" class="chat-input" rows="2" placeholder="Type a message..."></textarea>
      <div id="chat-output" class="chat-output"></div>
    </div>
  </div>

  <div id="tab-logs" class="hidden">
    <div class="panel">
      <div class="panel-header">
        <span><strong id="log-total">0</strong> total</span>
        <button class="btn btn-secondary" style="padding:4px 10px;font-size:12px" onclick="loadLogs()">Refresh</button>
      </div>
      <table>
        <thead><tr><th>Time</th><th>Model</th><th>Backend</th><th>Latency</th><th>Tokens</th><th>Status</th></tr></thead>
        <tbody id="log-body"></tbody>
      </table>
      <div class="pagination">
        <button class="page-btn" onclick="prevPage()">&larr;</button>
        <span class="page-info" id="page-info">1 / 1</span>
        <button class="page-btn" onclick="nextPage()">&rarr;</button>
      </div>
    </div>
  </div>

  <div id="tab-stats" class="hidden">
    <div class="stats-toolbar" id="stats-toolbar">
      <button class="stats-btn" onclick="loadStats('today',this)">Today</button>
      <button class="stats-btn active" onclick="loadStats('7d',this)">7d</button>
      <button class="stats-btn" onclick="loadStats('30d',this)">30d</button>
      <button class="stats-btn" onclick="loadStats('all',this)">All</button>
    </div>
    <div class="stats-grid">
      <div class="panel">
        <div class="panel-header"><span>By Model</span></div>
        <table>
          <thead><tr><th>Model</th><th>Reqs</th><th>Tokens</th><th>Avg ms</th><th>Err</th></tr></thead>
          <tbody id="stats-model-body"></tbody>
        </table>
      </div>
      <div class="panel">
        <div class="panel-header"><span>By Day</span></div>
        <table>
          <thead><tr><th>Date</th><th>Reqs</th><th>Tokens</th><th>Err</th></tr></thead>
          <tbody id="stats-day-body"></tbody>
        </table>
      </div>
    </div>
  </div>

  <div id="tab-config" class="hidden">
    <div class="panel">
      <pre class="config-display" id="config-display"></pre>
    </div>
  </div>
</div>

<script>
let logPage=0;const logLimit=30;

async function loadStatus(){
  const r=await fetch('/api/status');const d=await r.json();
  document.getElementById('total-requests').textContent=(d.total_requests||0).toLocaleString();
  document.getElementById('total-tokens').textContent=(d.total_tokens||0).toLocaleString();

  const el=document.getElementById('backends');
  el.innerHTML=d.backends.map(b=>{
    const bc=b.status==='active'?'badge-active':b.status==='expired'?'badge-expired':'badge-inactive';
    const bl=b.status==='active'?'Active':b.status==='expired'?'Expired':'Offline';
    const dc=b.status==='active'?'dot-green':b.status==='expired'?'dot-yellow':'dot-gray';
    const isOAuth=b.name==='claude'||b.name==='codex';
    let accts='';
    if(b.accounts&&b.accounts.length){
      accts=b.accounts.map(a=>{
        const ad=a.status==='active'?'dot-green':'dot-yellow';
        return '<div class="account-row"><span class="dot '+ad+'"></span><span class="email">'+a.email+'</span>'
          +(a.expires?'<span class="exp">'+a.expires+'</span>':'')
          +'<button class="btn-delete" onclick="removeAccount(\''+b.name+"','"+a.id+"')\">&times;</button></div>";
      }).join('');
    }
    const addBtn=isOAuth?'<a href="/auth/'+b.name+'" class="btn-add"><span>+</span> Add Account</a>':'';
    const syncBtn=isOAuth&&b.status==='active'?'<button class="btn-add" style="margin-left:4px" onclick="syncModels()">Sync</button>':'';
    return '<div class="backend-card"><div class="backend-header"><span class="dot '+dc+'"></span><span class="backend-name" style="text-transform:capitalize">'+b.name+'</span><span class="backend-badge '+bc+'">'+bl+'</span></div>'
      +'<div class="backend-info">'+(b.info||'')+'</div>'
      +'<div class="backend-models">'+(b.models||[]).map(m=>'<span class="model-tag">'+m+'</span>').join('')+'</div>'
      +accts+'<div style="display:flex;gap:4px;flex-wrap:wrap">'+addBtn+syncBtn+'</div></div>';
  }).join('');

  // Render per-account quota cards
  let allQuotas=[];
  d.backends.forEach(b=>{if(b.quotas)allQuotas=allQuotas.concat(b.quotas);});
  const qSection=document.getElementById('quota-section');
  const qGrid=document.getElementById('quota-grid');
  if(allQuotas.length){
    qSection.style.display='';
    qGrid.innerHTML=allQuotas.map(q=>{
      const planCls=q.plan_type?.toLowerCase().includes('pro')?'plan-pro':q.plan_type?.toLowerCase().includes('plus')?'plan-plus':'plan-team';
      const planLabel=q.plan_type||'Unknown';
      const displayName=q.email||q.account_id;
      const renderRow=(w)=>{
        if(!w)return '';
        const pct=Math.round(w.remaining_percent||0);
        const barColor=w.limit_reached?'var(--red)':pct<20?'var(--yellow)':'var(--green)';
        return '<div class="quota-row"><div class="quota-row-header"><span class="quota-row-label">'+w.label+'</span><span class="quota-row-value"><span class="pct">'+pct+'%</span>'+(w.reset_at||'')+'</span></div><div class="quota-bar"><div class="quota-bar-fill" style="width:'+Math.min(pct,100)+'%;background:'+barColor+'"></div></div></div>';
      };
      let rows='';
      if(q.has_real_data){
        rows=renderRow(q.primary)+renderRow(q.secondary);
        if(q.additional){q.additional.forEach(a=>{if(a.primary)rows+=renderRow(a.primary);});}
      } else {
        rows='<div style="font-size:12px;color:var(--text-2);padding:4px 0">No quota data yet — click <span style="color:var(--accent);cursor:pointer" onclick="refreshQuota(\''+q.account_id+'\')">↻ refresh</span> to fetch</div>';
      }
      const refreshBtn='<button class="btn-delete" style="font-size:11px;color:var(--accent)" onclick="refreshQuota(\''+q.account_id+'\')">&#8635;</button>';
      const fetchedAt=q.fetched_at?'<span style="font-size:10px;color:var(--text-2);margin-left:auto">cached '+q.fetched_at+'</span>':'';
      return '<div class="quota-card" data-account="'+q.account_id+'"><div class="quota-card-header"><span class="model-tag" style="background:var(--accent-dim);color:var(--text-0)">Codex</span><span style="flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">'+displayName+'</span>'+refreshBtn+'</div><div style="display:flex;align-items:center;gap:6px;margin-bottom:8px"><span class="plan-badge '+planCls+'">'+planLabel+'</span>'+fetchedAt+'</div>'+rows+'</div>';
    }).join('');
  } else {
    qSection.style.display='none';
  }

  const sel=document.getElementById('chat-model');
  const statusIcon=s=>s==='active'?'✓':s==='expired'?'!':'✗';
  sel.innerHTML=d.backends.map(b=>{
    const lbl=b.name.charAt(0).toUpperCase()+b.name.slice(1)+' ('+statusIcon(b.status)+')';
    return '<optgroup label="'+lbl+'">'+(b.models||[]).map(m=>
      '<option value="'+m+'"'+(b.status!=='active'?' disabled':'')+'>'+m+'</option>'
    ).join('')+'</optgroup>';
  }).join('');
  const first=sel.querySelector('option:not([disabled])');
  if(first)first.selected=true;
}

async function sendChat(){
  const model=document.getElementById('chat-model').value;
  const input=document.getElementById('chat-input').value.trim();
  if(!input)return;
  const output=document.getElementById('chat-output');
  output.textContent='';
  const resp=await fetch('/v1/chat/completions',{
    method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({model,messages:[{role:'user',content:input}],stream:true})
  });
  const reader=resp.body.getReader();const dec=new TextDecoder();let buf='';
  while(true){
    const{done,value}=await reader.read();if(done)break;
    buf+=dec.decode(value,{stream:true});
    const lines=buf.split('\n');buf=lines.pop();
    for(const line of lines){
      if(!line.startsWith('data: ')||line==='data: [DONE]')continue;
      try{const c=JSON.parse(line.slice(6));const t=c.choices?.[0]?.delta?.content;if(t)output.textContent+=t;}catch{}
    }
  }
  loadLogs();loadStatus();
}

function clearChat(){document.getElementById('chat-output').textContent='';document.getElementById('chat-input').value='';}

async function loadLogs(){
  const r=await fetch('/api/logs?limit='+logLimit+'&offset='+(logPage*logLimit));const d=await r.json();
  document.getElementById('log-total').textContent=d.total;
  document.getElementById('page-info').textContent=(logPage+1)+' / '+Math.max(1,Math.ceil(d.total/logLimit));
  document.getElementById('log-body').innerHTML=(d.logs||[]).map(l=>{
    const t=new Date(l.time).toLocaleString('en-GB',{hour:'2-digit',minute:'2-digit',second:'2-digit',day:'2-digit',month:'short'});
    const tok=(l.prompt_tokens||0)+(l.completion_tokens||0);
    const sc=l.status<400?'text-green':'text-red';
    return '<tr><td class="text-muted text-mono">'+t+'</td><td class="text-mono">'+l.model+'</td><td class="text-muted">'+l.backend
      +'</td><td>'+l.latency_ms+'ms</td><td>'+tok+'</td><td class="'+sc+'">'+l.status+'</td></tr>';
  }).join('')||'<tr><td colspan="6" class="text-muted" style="text-align:center;padding:24px">No requests yet</td></tr>';
}

function prevPage(){if(logPage>0){logPage--;loadLogs();}}
function nextPage(){logPage++;loadLogs();}

async function loadStats(range,btn){
  if(btn){document.querySelectorAll('.stats-btn').forEach(b=>b.classList.remove('active'));btn.classList.add('active');}
  const r=await fetch('/api/stats?range='+(range||'7d'));const d=await r.json();
  const empty='<tr><td colspan="5" class="text-muted" style="text-align:center;padding:20px">No data</td></tr>';
  document.getElementById('stats-model-body').innerHTML=(d.by_model||[]).map(s=>
    '<tr><td class="text-mono">'+s.model+'</td><td>'+s.request_count+'</td><td>'
    +(s.total_prompt_tokens+s.total_completion_tokens).toLocaleString()
    +'</td><td>'+Math.round(s.avg_latency_ms)+'</td><td class="'+(s.error_count?'text-red':'')+'">'+s.error_count+'</td></tr>'
  ).join('')||empty;
  document.getElementById('stats-day-body').innerHTML=(d.by_day||[]).map(s=>
    '<tr><td class="text-mono">'+s.date+'</td><td>'+s.request_count+'</td><td>'
    +(s.total_prompt_tokens+s.total_completion_tokens).toLocaleString()
    +'</td><td class="'+(s.error_count?'text-red':'')+'">'+s.error_count+'</td></tr>'
  ).join('')||empty.replace('5','4');
}

async function loadConfig(){
  const r=await fetch('/api/config');const d=await r.json();
  document.getElementById('config-display').textContent=JSON.stringify(d,null,2);
}

function switchTab(name,el){
  document.querySelectorAll('[id^="tab-"]').forEach(e=>e.classList.add('hidden'));
  document.querySelectorAll('.tab').forEach(e=>e.classList.remove('active'));
  document.getElementById('tab-'+name).classList.remove('hidden');
  if(el)el.classList.add('active');
  if(name==='logs')loadLogs();
  if(name==='stats')loadStats('7d');
  if(name==='config')loadConfig();
}

async function refreshQuota(accountId){
  const card=document.querySelector('[data-account="'+accountId+'"]');
  if(card)card.style.opacity='0.5';
  try{
    await fetch('/api/refresh-quota/codex/'+encodeURIComponent(accountId),{method:'POST'});
  }finally{
    if(card)card.style.opacity='1';
    loadStatus();
  }
}

async function syncModels(){
  await fetch('/api/sync-models',{method:'POST'});
  loadStatus();
}

async function removeAccount(provider,id){
  if(!confirm('Remove '+id+'?'))return;
  await fetch('/api/accounts/'+provider+'/'+encodeURIComponent(id),{method:'DELETE'});
  loadStatus();
}

loadStatus();setInterval(loadStatus,30000);
window.addEventListener('focus',()=>loadStatus());
</script>
</body>
</html>`
