// 纯渲染层：输入 /api/tray 的响应，输出 DOM。
// 刻意不含任何 fetch / Tauri / 配置逻辑，这样同一份代码既能跑在托盘里，
// 也能用捕获的真实数据在浏览器里离线预览和截图验证。

/** 千分位。*/
export function fmtNum(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return '–';
  return Math.round(n).toLocaleString('en-US');
}

/** 大数压缩：2159238 → 2.16M，给标题栏和窄空间用。*/
export function fmtCompact(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return '–';
  const abs = Math.abs(n);
  // 去掉尾随零：1.50B → 1.5B，2.00M → 2M。只删小数部分末尾的 0，
  // 顺带删掉光秃秃的小数点。
  const trim = (s) => s.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '');
  if (abs >= 1e9) return trim((n / 1e9).toFixed(2)) + 'B';
  if (abs >= 1e6) return trim((n / 1e6).toFixed(2)) + 'M';
  if (abs >= 1e3) return trim((n / 1e3).toFixed(1)) + 'K';
  return String(Math.round(n));
}

/**
 * 相对变化。基线为 0 时不返回“+∞%”这种没信息量的结果，
 * 而是标成 new，避免第一天用量把界面搞得像出了故障。
 * 带箭头是因为颜色本身有歧义：绿色在这里表示"用得比以前少"，
 * 不加方向符号容易被读成"增长了"。
 */
export function pctDelta(current, baseline) {
  if (!baseline || baseline <= 0) {
    return current > 0 ? { text: 'new', dir: 'up' } : null;
  }
  const d = ((current - baseline) / baseline) * 100;
  const dir = d > 0 ? 'up' : d < 0 ? 'down' : 'flat';
  const arrow = d > 0 ? '↑' : d < 0 ? '↓' : '';
  const abs = Math.abs(d);
  // 超过 10 倍就改用倍数表达："↑2506x" 比 "↑250512%" 好读，
  // 也不会因为位数太多把右侧撑破。
  if (abs >= 1000) {
    return { text: `${arrow}${Math.round(abs / 100)}x`, dir };
  }
  // 四舍五入绝不能造出"绝对值"。-99.6% 被舍成 ↓100% 会被读成"今天一个 token 都
  // 没花"（而 100% 是真实可能的：昨天有量、今天为 0），-0.4% 被舍成 ↓0% 又会被读
  // 成"没变化"。这两个边界都向内收，宁可说少也不夸大。
  if (abs >= 100) {
    // 只有真正的 100%（今天为 0）和 100% 以上的增长会走到这里
    return { text: `${arrow}${Math.round(abs)}%`, dir };
  }
  if (abs >= 99.5) {
    // floor 而非 toFixed(1)：99.96 会被 toFixed 舍成 "100.0"，又绕回原来的谎
    return { text: `${arrow}${(Math.floor(abs * 10) / 10).toFixed(1)}%`, dir };
  }
  if (abs < 1) {
    return { text: `${arrow}<1%`, dir };
  }
  return { text: `${arrow}${Math.round(abs)}%`, dir };
}

/**
 * 剩余额度 → 颜色档位。低额度必须一眼看出来。
 * 注意语义：入参是"剩余"百分比，不是"已用"。100 = 满格安全，0 = 用光。
 */
export function pctClass(p) {
  if (p === null || p === undefined) return 'warn';
  if (p <= 10) return 'bad';
  if (p <= 35) return 'warn';
  return 'ok';
}

/** 空闲时长的人话表达。*/
export function fmtIdle(seconds) {
  if (seconds === null || seconds === undefined) return '';
  if (seconds < 90) return `${Math.max(0, Math.round(seconds))}s 前`;
  const m = Math.round(seconds / 60);
  if (m < 90) return `${m}m 前`;
  const h = Math.round(m / 60);
  if (h < 36) return `${h}h 前`;
  return `${Math.round(h / 24)}d 前`;
}

/**
 * 托盘标题：菜单栏上那一小段文字。
 * 优先显示最紧张的额度——它才是会“出事”的指标；额度未知时退回今日用量。
 */
export function trayTitle(d) {
  if (!d) return '–';
  const s = d.min_session_percent;
  if (s !== null && s !== undefined) return `${Math.round(s)}%`;
  const t = d.today?.total_tokens;
  if (t) return fmtCompact(t);
  return '–';
}

/**
 * 根据一份 tray 数据算出该不该告警，以及告警文案。
 *
 * 放在这里（而不是 app.js）是为了让 app.js 和离线预览共用同一份逻辑——
 * 两处各写一遍的话，预览截图验证的就不是实际会弹出的内容了。
 *
 * 返回 null 表示无需告警。key 用于去重：同一档位只提醒一次。
 */
export function alertFor(d, threshold = 20) {
  const accounts = (d && d.accounts) || [];
  const limited = accounts.filter((a) => a.status === 'rate_limited');
  const worst = accounts
    .filter(
      (a) =>
        a.has_real_data &&
        a.status !== 'disabled' &&
        a.session_percent !== null &&
        a.session_percent !== undefined
    )
    .sort((a, b) => a.session_percent - b.session_percent)[0];

  if (limited.length > 0) {
    // 账号一多就不逐个列举：面板只有 320px 宽，四五个账号会把横幅撑到
    // 把用量卡片挤出可视区。只报最早恢复时间，其余折成计数。
    let body;
    if (limited.length === 1) {
      body = `${limited[0].email.split('@')[0]} 至 ${limited[0].rate_limited_until || '?'}`;
    } else {
      const earliest = limited
        .map((a) => a.rate_limited_until)
        .filter(Boolean)
        .sort()[0];
      body = `${limited.length} 个账号被限流${earliest ? `，最早 ${earliest} 恢复` : ''}`;
    }
    return {
      key: `limited:${limited.map((a) => a.email).sort().join(',')}`,
      title: 'LLM Proxy 账号被限流',
      body,
    };
  }

  if (worst && worst.session_percent <= threshold) {
    // 按 5% 分档，跌得更低会再提醒一次，但同档内反复轮询不会重复打扰
    const bucket = Math.floor(worst.session_percent / 5) * 5;
    return {
      key: `low:${worst.email}:${bucket}`,
      title: 'LLM Proxy 额度偏低',
      body: `${worst.email.split('@')[0]} 会话额度仅剩 ${Math.round(worst.session_percent)}%`,
    };
  }

  return null;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function meterRow(tag, pct, resetAt) {
  const row = el('div', 'meter-row');
  row.appendChild(el('span', 'meter-tag', tag));
  const cls = pctClass(pct);
  const meter = el('div', 'meter');
  // 额度耗尽时给整条轨道上色：0% 的填充条只有几个像素宽，
  // 光靠填充色传达不了"用光了"这个最该被看见的状态。
  if (pct !== null && pct !== undefined && pct <= 0) meter.classList.add('empty-bad');
  const fill = el('span', cls);
  // 0% 也留 2px 可见宽度，否则“用完了”和“没数据”视觉上一样。
  fill.style.width = pct === null || pct === undefined ? '0%' : `${Math.max(2, Math.min(100, pct))}%`;
  meter.appendChild(fill);
  row.appendChild(meter);
  // 百分比数字跟着分档上色，让告警在数字上也有体现
  const num = el('span', `meter-pct ${pct === null || pct === undefined ? '' : cls}`);
  num.textContent = pct === null || pct === undefined ? '?' : `${Math.round(pct)}%`;
  row.appendChild(num);
  return row;
}

/**
 * 缩短重置时间。
 *
 * 后端给的是 "MM/DD HH:MM"。5h 窗口几乎总在当天重置，日期是纯冗余；当天就只留
 * HH:MM。跨天时必须保留日期——不然凌晨 4 点重置显示成 "04:00"，看着像已经过去了。
 * todayISO 形如 "2026-07-31"；取不到就原样返回，宁可啰嗦也不能显示错的时间。
 */
export function shortReset(resetAt, todayISO) {
  if (!resetAt || !todayISO) return resetAt || '';
  const [, m, d] = todayISO.split('-');
  return resetAt.startsWith(`${m}/${d} `) ? resetAt.slice(6) : resetAt;
}

function renderAccounts(container, accounts, todayISO) {
  container.textContent = '';
  if (!accounts || accounts.length === 0) {
    container.appendChild(el('div', 'empty', '没有已登录的账号'));
    return;
  }
  for (const a of accounts) {
    const box = el('div', 'acct');
    const top = el('div', 'acct-top');
    // 邮箱只留 @ 前的部分，320px 宽放不下完整地址
    const short = (a.email || '').split('@')[0];
    top.appendChild(el('span', 'acct-name', short));

    const right = el('div');
    right.style.cssText = 'display:flex;align-items:center;gap:5px;flex-shrink:0';
    if (a.status === 'rate_limited') {
      right.appendChild(el('span', 'badge limited', a.rate_limited_until ? `限流至 ${a.rate_limited_until}` : '限流'));
    } else if (a.status === 'disabled') {
      right.appendChild(el('span', 'badge disabled', '已停用'));
    } else if (!a.has_real_data) {
      right.appendChild(el('span', 'badge nodata', '无数据'));
    }
    if (a.plan_type) right.appendChild(el('span', 'acct-plan', a.plan_type));
    top.appendChild(right);
    box.appendChild(top);

    box.appendChild(meterRow('5h', a.session_percent));
    box.appendChild(meterRow('周', a.weekly_percent));
    // 两个重置时间挤在同一行：卡片高度按账号数线性增长，每个账号多一行的代价
    // 在三四个账号时就很明显了。
    const resets = [];
    if (a.session_reset_at) resets.push(`5h 重置 ${shortReset(a.session_reset_at, todayISO)}`);
    if (a.weekly_reset_at) resets.push(`周重置 ${shortReset(a.weekly_reset_at, todayISO)}`);
    if (resets.length) {
      box.appendChild(el('div', 'acct-reset', resets.join(' · ')));
    }
    container.appendChild(box);
  }
}

function renderKeys(container, byKey) {
  container.textContent = '';
  if (!byKey || byKey.length === 0) {
    container.appendChild(el('div', 'empty', '今日还没有请求'));
    return;
  }
  const max = Math.max(...byKey.map((k) => k.total_tokens), 1);
  const total = byKey.reduce((s, k) => s + k.total_tokens, 0);
  // 只显示前 5 个，其余归入"其他"，否则 key 一多面板就撑爆了
  const top = byKey.slice(0, 5);
  const rest = byKey.slice(5);
  if (rest.length) {
    top.push({
      key_name: `其他 ${rest.length} 个`,
      total_tokens: rest.reduce((s, k) => s + k.total_tokens, 0),
      request_count: rest.reduce((s, k) => s + k.request_count, 0),
    });
  }
  for (const k of top) {
    const row = el('div', 'keyrow');
    const t = el('div', 'keyrow-top');
    t.appendChild(el('span', 'keyrow-name', k.key_name || '(未命名)'));
    const share = total > 0 ? Math.round((k.total_tokens / total) * 100) : 0;
    // 占比不足 1% 的显示 <1% 而不是 0%，否则会让人以为完全没用
    const shareText = share === 0 && k.total_tokens > 0 ? '<1%' : `${share}%`;
    t.appendChild(el('span', 'keyrow-val', `${fmtCompact(k.total_tokens)} · ${shareText}`));
    row.appendChild(t);
    const bar = el('div', 'keybar');
    const fill = el('span');
    // 最小宽度取 2%：够看见是根条，又不至于把 1% 撑成 5% 那么夸张。
    // 精确数值旁边有百分比文字兜底，条形只负责传达量级。
    fill.style.width = `${Math.max(2, (k.total_tokens / max) * 100)}%`;
    bar.appendChild(fill);
    row.appendChild(bar);
    container.appendChild(row);
  }
}

function renderSpark(container, hourly, nowHour, axisEl) {
  container.textContent = '';
  const arr = hourly && hourly.length === 24 ? hourly : new Array(24).fill(0);
  const max = Math.max(...arr, 1);
  arr.forEach((v, h) => {
    // 每小时是一个"槽位"：槽位本身画出浅色底，柱子在里面长高。
    // 否则 24 个空小时会连成一条虚线，看起来像图表没渲染完。
    const slot = el('i', 'slot');
    const bar = el('b');
    // 用平方根压一下，否则一个尖峰会把其他小时压成看不见的一条线
    const ratio = v > 0 ? Math.sqrt(v / max) : 0;
    bar.style.height = v > 0 ? `${Math.max(10, ratio * 100)}%` : '0%';
    slot.appendChild(bar);
    if (h === nowHour) slot.classList.add('now');
    slot.title = `${String(h).padStart(2, '0')}:00 — ${fmtNum(v)} tokens`;
    container.appendChild(slot);
  });

  // 刻度必须和槽位用同一套网格，否则标签会漂移，让人误读成差一小时。
  // 做法：轴也是 24 个等宽格子，只在选定的小时上写数字。
  if (axisEl) {
    axisEl.textContent = '';
    // 当前小时的刻度优先；与它相邻（±1）的固定刻度要让位，否则两个数字
    // 会挤在一起读成 "1112" 这种乱码。
    const fixed = [0, 6, 12, 18, 23].filter(
      (h) => nowHour === undefined || Math.abs(h - nowHour) > 1
    );
    const ticks = new Set(fixed);
    if (nowHour >= 0 && nowHour < 24) ticks.add(nowHour);
    for (let h = 0; h < 24; h++) {
      const cell = el('span', h === nowHour ? 'tick now' : 'tick');
      cell.textContent = ticks.has(h) ? String(h) : '';
      axisEl.appendChild(cell);
    }
  }
}

/** 把一份 /api/tray 响应画到页面上。*/
export function render(d, opts = {}) {
  const doc = opts.doc || document;
  const q = (id) => doc.getElementById(id);
  const nowHour = opts.nowHour !== undefined ? opts.nowHour : new Date().getHours();

  // 今日
  q('today-tokens').textContent = fmtCompact(d.today?.total_tokens ?? 0);
  q('today-reqs').textContent = fmtNum(d.today?.request_count ?? 0);
  q('today-date').textContent = d.today?.date || '';

  const dy = pctDelta(d.today?.total_tokens ?? 0, d.yesterday?.total_tokens ?? 0);
  const elY = q('delta-yday');
  elY.textContent = dy ? `${dy.text} vs 昨日` : '';
  elY.className = `delta ${dy ? dy.dir : ''}`;

  const da = pctDelta(d.today?.total_tokens ?? 0, d.avg_7d_tokens ?? 0);
  const elA = q('delta-avg');
  elA.textContent = da ? `${da.text} vs 7日均` : '';
  elA.className = `delta ${da ? da.dir : ''}`;

  renderSpark(q('spark'), d.hourly_tokens, nowHour, q('spark-axis'));
  // today.date 是后端按客户端时区算出的日历天，用来判断重置时间是否就在今天
  renderAccounts(q('accounts'), d.accounts, d.today?.date);
  renderKeys(q('keys'), d.today?.by_key);

  // 额度抓取时间：所有账号里最新的那个
  const stamps = (d.accounts || []).map((a) => a.quota_fetched_at).filter(Boolean).sort();
  q('quota-stamp').textContent = stamps.length ? stamps[stamps.length - 1] : '';

  q('foot-backends').textContent =
    d.backends_total ? `后端 ${d.backends_active}/${d.backends_total} 启用` : '';
  q('foot-idle').textContent = d.last_request_at ? `最近请求 ${fmtIdle(d.idle_seconds)}` : '';

  if (d.generated_at) {
    const t = new Date(d.generated_at);
    q('stamp').textContent = `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`;
  }
}
