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
 * 花费。后端按"如果走按量计费 API 会花多少"给每条请求定价：
 * OAuth 订阅（Claude Code / Codex）本身不按请求扣费，这个数是订阅的等价价值。
 *
 * 量级从一次 Haiku 调用的 $0.0003 到一个月 Opus 的四位数，固定小数位会把前者
 * 印成 $0.00，所以精度跟着量级走。null 表示"这个模型没有定价"，显示 "—"：
 * 折成 $0 就等于宣称它免费。
 */
export function fmtMoney(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—';
  const abs = Math.abs(v);
  if (abs >= 1000) return '$' + fmtNum(v);
  if (abs >= 1) return '$' + v.toFixed(2);
  if (abs >= 0.01) return '$' + v.toFixed(3);
  if (abs > 0) return '$' + v.toFixed(4);
  return '$0';
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
  // 显示今日 token 用量，不是剩余额度百分比。
  //
  // 百分比曾经排在前面，但菜单栏里一个孤零零的 "0%" 说不清是什么归零了——是额度
  // 用光、还是根本没取到数？而且额度告警本来就会主动弹通知，面板里也有逐账号的
  // 进度条，那个数字在标题栏属于重复。今日用量是唯一"只有这里能看到"的信息。
  const t = d.today?.total_tokens;
  if (t) return fmtCompact(t);
  // 今天还没有流量时不写 0：先落回额度，至少说明挂件是活的、连得上代理
  const s = d.min_session_percent;
  if (s !== null && s !== undefined) return `${Math.round(s)}%`;
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

  // 5h 和 7d 都要盯：只看 5h 会漏掉更难受的那种——5h 见底等几小时就回血，
  // 7d 见底要等到下周。文案里必须点明是哪个窗口，否则收到通知还得点开面板
  // 才知道该歇一会儿还是该换账号。
  let worst = null;
  for (const a of accounts) {
    if (!a.has_real_data || a.status === 'disabled') continue;
    for (const [win, pct] of [
      ['5h', a.session_percent],
      ['7d', a.weekly_percent],
    ]) {
      if (pct === null || pct === undefined) continue;
      // 严格小于：两个窗口打平时报 5h，跟 bindingWindow 的取舍保持一致
      if (!worst || pct < worst.pct) worst = { win, pct, email: a.email || '' };
    }
  }

  if (worst && worst.pct <= threshold) {
    // 按 5% 分档，跌得更低会再提醒一次，但同档内反复轮询不会重复打扰。
    // 窗口也进键里：5h 报过之后 7d 再见底，是另一件事，必须能再响一次。
    const bucket = Math.floor(worst.pct / 5) * 5;
    return {
      key: `low:${worst.email}:${worst.win}:${bucket}`,
      title: 'LLM Proxy 额度偏低',
      body: `${worst.email.split('@')[0]} ${worst.win} 额度仅剩 ${Math.round(worst.pct)}%`,
    };
  }

  return null;
}

/**
 * 单个账号的瓶颈窗口：5h 与 周 里更紧的那个。
 *
 * 两个百分比里只有小的那个决定这个账号现在能不能服务请求，另一个是冗余信息。
 * 常驻挂件只显示瓶颈，完整两条留给点开的面板。没有任何数据时返回 null。
 */
export function bindingWindow(a) {
  const pairs = [
    ['5h', a && a.session_percent],
    ['周', a && a.weekly_percent],
  ].filter(([, p]) => p !== null && p !== undefined);
  if (!pairs.length) return null;
  const [win, pct] = pairs.reduce((lo, cur) => (cur[1] < lo[1] ? cur : lo));
  return { win, pct };
}

/**
 * 一个窗口（5h 或 7d）在整个池子里的总余量。
 *
 * 各账号的窗口是**独立结算**的，所以"还能用多少"是各账号剩余百分比之**和**：三个
 * 账号分别剩 100/98/51，总量就是 249%（约两个半账号的量）。取最大只能回答"最宽裕
 * 的那个还剩多少"，取最小是"最先干涸的那个"——两个都不是"我还能用多少"。
 *
 * cap 只统计有该窗口数据的账号，否则某个账号缺数据会把百分比整体压低，看着像掉了
 * 额度。被停用的账号不计入（它服务不了流量）；被限流的**要**计入——它的余量此刻
 * 就是接近 0，而它的重置时间正是"多久后回血"的答案。
 *
 * 一个账号都没有时返回 null，调用方必须显式表达"未知"。
 */
export function poolWindow(accounts, key) {
  let sum = 0;
  let count = 0;
  for (const a of accounts || []) {
    if (a.status === 'disabled' || !a.has_real_data) continue;
    const p = a[key];
    if (p === null || p === undefined) continue;
    sum += p;
    count += 1;
  }
  if (!count) return null;
  const cap = count * 100;
  return { sum, cap, count, pct: (sum / cap) * 100 };
}

/**
 * 下一次会回血的重置：最早的未来重置时刻。传 only('5h'/'7d') 只看那个窗口，
 * 不传就是两个窗口一起比。
 *
 * 这比任何百分比都更直接决定"现在要不要停手"。附带 gain（该窗口会补回多少）是因为
 * 最早的那次重置未必有意义——一个已经 98% 的 5h 窗口重置只补 2%，看到 gain 才知道
 * 值不值得等。
 */
export function nextReset(accounts, nowMs, only) {
  const now = nowMs === undefined ? Date.now() : nowMs;
  let best = null;
  for (const a of accounts || []) {
    if (a.status === 'disabled' || !a.has_real_data) continue;
    for (const [win, unix, at, pct] of [
      ['5h', a.session_reset_unix, a.session_reset_at, a.session_percent],
      ['7d', a.weekly_reset_unix, a.weekly_reset_at, a.weekly_percent],
    ]) {
      if (only && win !== only) continue;
      if (!unix) continue;
      const ms = unix * 1000;
      if (ms <= now) continue; // 已过期的快照，等下一轮抓取纠正
      if (!best || ms < best.ms) {
        best = {
          ms,
          win,
          at: at || '',
          email: a.email || '',
          gain: pct === null || pct === undefined ? null : Math.max(0, 100 - pct),
        };
      }
    }
  }
  return best;
}

/** 倒计时：<1m 显示"即将"，然后 42m / 3h20m / 3d。 */
export function fmtEta(ms) {
  if (ms === null || ms === undefined || ms <= 0) return '';
  const min = Math.floor(ms / 60000);
  if (min < 1) return '即将';
  if (min < 60) return `${min}m`;
  const h = Math.floor(min / 60);
  const m = min % 60;
  if (h < 24) return m ? `${h}h${m}m` : `${h}h`;
  return `${Math.round(h / 24)}d`;
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
      cost_usd: rest.reduce((s, k) => s + (k.cost_usd || 0), 0),
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
    // 花费放 title 而不是行内：一行里已经有名字、token、占比三个数，再塞一个
    // 金额就没人能一眼扫完了。想知道某个 key 花了多少，悬停即可。
    t.title = `${fmtNum(k.request_count || 0)} 请求 · ${fmtNum(k.total_tokens)} tokens · ${fmtMoney(k.cost_usd || 0)}`;
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

// 口径："输入"是完整 prompt（含缓存），"缓存"是其中命中/写入缓存的那部分，
// "思考"是"输出"里的推理部分。所以只有 输入 + 输出 == 总数，缓存和思考是
// 子集标注，不参与相加。
//
// 后端存的是互不重叠的形式（prompt_tokens 已扣除缓存，因为 Anthropic 就是这么
// 报的），两种算法的总数完全相同，这里只做展示层换算。
//
// 为什么这样展示：缓存断点打在最后一条消息上时整个 prompt 都被缓存，后端存的
// prompt_tokens 只剩 1-2。在一个十万 token 的对话旁边显示"输入 1"，任何人都会
// 以为数据坏了 —— 而"输入 100,740，其中 100,739 命中缓存"第一眼就是对的。
//
// 思考 token 只有 OpenAI 系上游（Codex / Kimi）会单独返回；Anthropic 把它算进
// output_tokens 且不拆分，后端因此记成 -1。这里显示 "—" 而不是 0：
// "上游没给这个数" 和 "模型没思考" 是两件不同的事，写成 0 就是在编数据。
const REASONING_UNKNOWN = -1;
const BREAKDOWN_KINDS = [
  { key: 'input', label: '输入', color: 'var(--ok)' },
  { key: 'cache', label: '缓存', color: 'var(--warn)' },
  { key: 'completion_tokens', label: '输出', color: 'var(--bad)' },
  { key: 'reasoning_tokens', label: '思考', color: 'var(--info)' },
];

function renderBreakdown(container, day) {
  if (!container) return;
  container.textContent = '';
  const total = day?.total_tokens ?? 0;
  if (!total) return;

  const read = day.cache_read_tokens || 0;
  const write = day.cache_write_tokens || 0;
  const cache = read + write;
  // 聚合求和时 -1 会被 clamp 成 0，所以纯 Anthropic 流量传过来是 reasoning=0。
  // reasoning_known_requests === 0 表示这一天没有任何一条请求拿到过思考数——
  // 那是"不知道"，不是"没思考"，仍然要显示 "—"。
  const reasoning = day.reasoning_known_requests === 0
    ? REASONING_UNKNOWN
    : (day.reasoning_tokens ?? REASONING_UNKNOWN);
  const vals = {
    input: (day.prompt_tokens || 0) + cache, // 完整 prompt，含缓存
    cache,
    completion_tokens: day.completion_tokens || 0,
    reasoning_tokens: reasoning,
  };
  const hitPct = vals.input ? Math.round((cache / vals.input) * 100) : 0;

  for (const k of BREAKDOWN_KINDS) {
    const v = vals[k.key];
    const item = el('span', 'bd-item');
    const dot = el('i', 'bd-dot');
    dot.style.background = k.color;
    item.appendChild(dot);
    item.appendChild(document.createTextNode(`${k.label} `));
    const unknown = k.key === 'reasoning_tokens' && v === REASONING_UNKNOWN;
    item.appendChild(el('b', null, unknown ? '—' : fmtCompact(v)));
    if (unknown) {
      item.title = 'Anthropic 不单独返回 thinking token（已计入输出），只有 Codex / Kimi 会给';
    } else if (k.key === 'cache') {
      item.title = `输入的 ${hitPct}%（不额外计入总数）· 读 ${fmtNum(read)} / 写 ${fmtNum(write)}`;
    } else if (k.key === 'reasoning_tokens') {
      item.title = '输出的一部分，不额外计入总数';
    } else if (k.key === 'input') {
      item.title = `完整 prompt（含缓存）${fmtNum(v)}`;
    } else {
      item.title = fmtNum(v);
    }
    container.appendChild(item);
  }

  // 花费跟在四个 token 桶后面：它是从同样这些数算出来的，放同一行才能一眼
  // 对上"这些 token 值多少钱"。cost_known_requests === 0 表示今天没有任何一条
  // 请求能定价（比如全是订阅模型且未在 pricing 里声明），显示 "—"。
  const priced = day.cost_known_requests !== 0;
  const cost = el('span', 'bd-item bd-cost');
  cost.appendChild(document.createTextNode('花费 '));
  cost.appendChild(el('b', null, priced ? fmtMoney(day.cost_usd || 0) : '—'));
  const unpriced = Math.max(0, (day.request_count || 0) - (day.cost_known_requests || 0));
  cost.title = priced
    ? '按量计费 API 的等价价格；OAuth 订阅流量并不按请求扣费' +
      (unpriced ? `（不含 ${unpriced} 条未定价请求）` : '')
    : '这些模型没有定价，在 config.yaml 的 pricing.models 里补';
  container.appendChild(cost);
}

// ---------- 历史用量热力图（卡片背面） ----------

// 18 列 ≈ 4 个月。格子 12px + 间隙 3px 铺满 320px 面板的卡内宽度。
// 原来是 25 列 / 9px（半年），但 9px 太密、鼠标停不准某一天，而"悬停看当天用量"
// 就是这张图的主要用法。宽度定死在 CSS 的 .heat-grid 里，两处要一起改。
export const HEAT_WEEKS = 18;
const HEAT_LEVELS = 4;   // 0 = 无流量，1..4 = 由浅到深

/** 本地日期 → "YYYY-MM-DD"，和后端按客户端时区分出的桶键对齐。 */
function dayKey(d) {
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

/**
 * 分档。相对最大值而不是绝对阈值——用量在几千和几千万之间浮动，写死阈值会让
 * 整张图要么全空要么全满。
 *
 * 开平方压一下，和 renderSpark 里同样的理由：日用量的分布极度偏斜（实测一天
 * 98M、隔天 10M、更早的日子几十万），线性分档会把除了尖峰那天以外的所有日子
 * 全压进第 1 档，整张图看上去像只用过一天。
 *
 * 有流量就至少 1 档：跑了几百 token 的一天不该和"完全没用"同色。
 */
export function heatLevel(v, max) {
  if (v <= 0 || max <= 0) return 0;
  const r = Math.sqrt(v / max);
  return Math.min(HEAT_LEVELS, Math.max(1, Math.ceil(r * HEAT_LEVELS)));
}

/**
 * 把接口返回的稀疏天（只含有流量的日子）铺成连续的日历网格。
 * 结尾对齐到本周六、开头回退到某个周日，这样列就是完整的周，
 * 星期几固定落在同一行（和 GitHub 贡献图一致）。
 * 未来的格子标出来：它们不是"没用量"，而是还没到。
 */
export function buildHeatGrid(days, weeks = HEAT_WEEKS, today = new Date()) {
  const byDate = new Map();
  for (const d of days || []) byDate.set(d.d, d);

  const end = new Date(today);
  end.setHours(0, 0, 0, 0);
  end.setDate(end.getDate() + (6 - end.getDay()));
  const start = new Date(end);
  start.setDate(start.getDate() - (weeks * 7 - 1));

  const todayKey = dayKey(today);
  const cells = [];
  for (let i = 0; i < weeks * 7; i++) {
    const d = new Date(start);
    d.setDate(d.getDate() + i);
    const k = dayKey(d);
    const hit = byDate.get(k);
    cells.push({
      date: d,
      key: k,
      tokens: hit ? hit.t || 0 : 0,
      requests: hit ? hit.r || 0 : 0,
      cache: hit ? hit.c || 0 : 0,
      cost: hit ? hit.u || 0 : 0,
      future: k > todayKey,
      isToday: k === todayKey,
    });
  }
  return cells;
}

/** 某一天的卡片文案。导出是为了能单测，不必去戳 DOM。 */
export function heatDayLines(c) {
  const md = c.key.slice(5).replace('-', '/');
  if (c.requests <= 0) return { date: md, rows: [['用量', '无']] };
  const rows = [['请求', fmtNum(c.requests)]];
  if (c.tokens > 0) {
    rows.push(['tokens', fmtCompact(c.tokens)]);
    // 缓存占比是这一天"贵不贵"的唯一线索：请求数一样但缓存命中差一截，
    // 花的钱能差一个数量级
    rows.push(['缓存', `${Math.round((c.cache / c.tokens) * 100)}%`]);
    // 缓存占比说的是"贵不贵"，花费直接说"多少钱"——两个都留着，因为便宜的
    // 一天也可能是因为根本没跑几个请求。
    if (c.cost > 0) rows.push(['花费', fmtMoney(c.cost)]);
  } else {
    // 有请求但没 token 记录是真实存在的（早期未采集、或全是失败请求），
    // 说成"无用量"是在编数据
    rows.push(['tokens', '无记录']);
  }
  return { date: md, rows };
}

/**
 * 给热力图挂上日用量卡：悬停显示，点击也显示（并且**不**冒泡到卡片，
 * 否则点格子会把整张卡翻回正面——想看某天用量的人绝不希望这样）。
 */
function bindHeatTip(doc, grid, cells) {
  const tip = doc.getElementById('heat-tip');
  if (!tip) return;

  const show = (cell) => {
    const idx = Number(cell.dataset.day);
    const c = cells[idx];
    if (!c) return;
    const { date, rows } = heatDayLines(c);
    tip.textContent = '';
    const head = doc.createElement('div');
    head.className = 'heat-tip-date';
    head.textContent = date;
    tip.appendChild(head);
    for (const [k, v] of rows) {
      const line = doc.createElement('div');
      line.className = 'heat-tip-row';
      const label = doc.createElement('span');
      label.textContent = k;
      const val = doc.createElement('b');
      val.textContent = v;
      line.append(label, val);
      tip.appendChild(line);
    }
    tip.classList.remove('hidden');

    // 定位在格子上方居中，放不下就翻到下方；左右夹在卡片内。
    // 相对 .flip-back（已是 absolute 定位的包含块）算坐标。
    const host = tip.offsetParent || tip.parentElement;
    const hb = host.getBoundingClientRect();
    const cb = cell.getBoundingClientRect();
    const tb = tip.getBoundingClientRect();
    const pad = 6;
    let left = cb.left - hb.left + cb.width / 2 - tb.width / 2;
    left = Math.max(pad, Math.min(left, hb.width - tb.width - pad));
    let top = cb.top - hb.top - tb.height - 5;
    if (top < 0) top = cb.bottom - hb.top + 5;
    tip.style.left = `${Math.round(left)}px`;
    tip.style.top = `${Math.round(top)}px`;
  };

  const hide = () => tip.classList.add('hidden');

  grid.addEventListener('mouseover', (e) => {
    const cell = e.target.closest?.('.heat-cell[data-day]');
    if (cell) show(cell);
  });
  grid.addEventListener('mouseleave', hide);
  // 点格子照旧翻回正面（不拦冒泡）：看某天的数据靠悬停就够了，
  // 再让点击也变成"只看数据"，整张卡就只剩边角能翻，反而难用。
}

/**
 * 渲染热力图。纯函数：只吃数据和容器，不碰 fetch/Tauri，
 * 这样 preview.mjs 能拿一份 fixture 直接截图验证。
 */
export function renderHistory(container, days, opts = {}) {
  if (!container) return;
  const doc = opts.doc || container.ownerDocument || document;
  const mk = (tag, cls, text) => {
    const e = doc.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  container.textContent = '';

  if (!days || days.length === 0) {
    container.appendChild(mk('div', 'empty', '暂无历史数据'));
    return;
  }

  const cells = buildHeatGrid(days, HEAT_WEEKS, opts.today || new Date());
  // 按**请求数**着色，不是 token 数。
  //
  // token 总量跨不了口径变更那道坎：缓存是 2026-08-02 才开始记的，之前的行缓存列
  // 为 0，于是同样忙的一天在改动前后能差两个数量级。实测 06-05 有 2,405 个请求但
  // 只记了 408K token，而 08-03 只有 857 个请求却记了 98.3M —— 按 token 着色会把
  // 请求数最多的那些天画成空白，"活跃图"于是在说反话。
  // 请求数没有这个问题，而且它本来就是"活跃度"更自然的度量（GitHub 数的是提交
  // 次数，不是改了多少行）。token 仍然出现在悬停和汇总里。
  const max = Math.max(...cells.map((c) => c.requests), 1);

  // 月份标签：每列取该列第一天，月份变了就写一次。只在月初那周写，
  // 否则跨月那一列会紧挨着上一个标签叠在一起。
  const months = mk('div', 'heat-months');
  const MN = ['1月', '2月', '3月', '4月', '5月', '6月', '7月', '8月', '9月', '10月', '11月', '12月'];
  let prevMonth = -1;
  for (let w = 0; w < HEAT_WEEKS; w++) {
    const first = cells[w * 7].date;
    const m = first.getMonth();
    const label = m !== prevMonth && first.getDate() <= 7 ? MN[m] : '';
    if (label) prevMonth = m;
    months.appendChild(mk('span', null, label));
  }
  container.appendChild(months);

  const grid = mk('div', 'heat-grid');
  for (const [i, c] of cells.entries()) {
    const cell = mk('i', `heat-cell l${c.future ? 0 : heatLevel(c.requests, max)}`);
    if (c.future) cell.classList.add('heat-future');
    if (c.isToday) cell.classList.add('heat-today');
    if (!c.future) cell.dataset.day = String(i);
    grid.appendChild(cell);
  }
  container.appendChild(grid);
  bindHeatTip(doc, grid, cells);

  // 底部一行：活跃天数 · 色阶 · 总量。
  //
  // 原来图例和汇总各占一行，两行都是 10px 灰字挤在一条分隔线上下，看着很局促。
  // 现在只留这一行两个数。色阶图例也去掉了：它只说明"深=多"，而这件事看一眼格子
  // 就知道，占掉的横向空间反而是让这行挤起来的原因；精确数值靠悬停某一格来看。
  //
  // "活跃天数"数的是真有请求的日子，不是窗口长度——窗口恒为 18 周，说了等于没说。
  const past = cells.filter((c) => !c.future);
  const activeDays = past.filter((c) => c.requests > 0).length;
  const total = past.reduce((s, c) => s + c.tokens, 0);

  const foot = mk('div', 'heat-foot');
  foot.appendChild(mk('span', null, `活跃 ${activeDays} 天`));
  foot.appendChild(mk('span', 'heat-total', `${fmtCompact(total)} tokens`));
  container.appendChild(foot);

  const lastPast = past.length ? past[past.length - 1].key : cells[0].key;
  return { first: cells[0].key, last: lastPast, max, activeDays, total };
}

/**
 * 画背面。app.js 拿到接口数据后调它，preview.mjs 通过 fixture 里的
 * history 字段间接调它。
 *
 * 标题栏不再写日期范围：月份标签已经标出了跨度，汇总行还写了天数，
 * 再来一个 "02-15 – 08-04" 是第三遍说同一件事。
 */
export function paintHistory(doc, days, opts = {}) {
  renderHistory(doc.getElementById('history'), days, { ...opts, doc });
  fitFlipHeight(doc);
}

/**
 * 让翻面容器取两面中较高的那个高度。
 *
 * 背面是 absolute + inset:0，高度完全由正面决定——正面矮一点，背面就溢出到下一张
 * 卡上去。这事已经犯过两次（加汇总行时、格子放大时），每次都是靠截图才发现，而且
 * 只差几个像素时截图也看不出来。所以不再靠"目测两面差不多高"，直接量。
 */
function fitFlipHeight(doc) {
  const inner = doc.querySelector('.flip-inner');
  const front = doc.querySelector('.flip-front');
  const back = doc.querySelector('.flip-back');
  if (!inner || !front || !back) return;
  // 先清掉上一次的值，否则量到的是被自己撑开后的高度，只会单调变大
  inner.style.minHeight = '';
  front.style.minHeight = '';
  const frontH = front.getBoundingClientRect().height;
  // scrollHeight 不含边框，补上上下各 1px
  const backH = back.scrollHeight + 2;
  const h = Math.ceil(Math.max(frontH, backH));
  // 两处都要压：正面在常规文档流里，高度是自己的内容高；背面 inset:0，高度是容器高。
  // 只设容器的话两者会差几个像素——正面 197、背面 198，翻面时卡片会肉眼可见地
  // 长/短一点点。给正面也设同一个 min-height，两面的盒子就严格一样大。
  inner.style.minHeight = `${h}px`;
  front.style.minHeight = `${h}px`;
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
/**
 * 悬浮挂件的渲染：折叠是一个球，展开是一张紧凑卡。
 *
 * 只回答两个问题，其它一概不放：
 *   1. 下一次重置之前，我总共还能用多少（5h 是当下的闸门，7d 是这周的预算）
 *   2. 下一次回血是什么时候、能补回多少
 *
 * 逐账号明细刻意不放——代理自动挑账号，"哪个账号剩多少"不改变你的行动，那是出问题
 * 时才看的诊断信息，菜单栏面板里有完整两条。
 */
export function renderFloat(d, opts = {}) {
  const doc = opts.doc || document;
  const q = (id) => doc.getElementById(id);
  const nowMs = opts.nowMs === undefined ? Date.now() : opts.nowMs;

  const accounts = (d && d.accounts) || [];
  const session = poolWindow(accounts, 'session_percent');
  const weekly = poolWindow(accounts, 'weekly_percent');
  const next = nextReset(accounts, nowMs);
  const eta = next ? fmtEta(next.ms - nowMs) : '';

  // 球上放 5h：它才是"这一小时还能不能干活"的闸门，7d 是预算，放展开里
  const cls = pctClass(session ? session.pct : null);
  const ball = q('ball');
  if (ball) {
    ball.style.setProperty('--p', session ? Math.max(0, Math.min(100, session.pct)) : 0);
    ball.className = cls;
    // 刻意不设 title：鼠标停在球上本来就会展开卡片，再弹一个系统 tooltip 是同一份
    // 信息说两遍，而且它会盖住球下方的桌面。
  }
  if (q('ball-pct')) q('ball-pct').textContent = session ? `${Math.round(session.pct)}%` : '–';
  // 第二行是倒计时而不是别的数字：它是唯一"等一等就会变好"的信息
  if (q('ball-eta')) q('ball-eta').textContent = eta;

  if (q('fcard-tokens')) {
    q('fcard-tokens').textContent = d.today?.total_tokens ? `今日 ${fmtCompact(d.today.total_tokens)}` : '';
  }

  const rows = q('fcard-rows');
  if (!rows) return;
  rows.textContent = '';
  if (!session && !weekly) {
    rows.appendChild(el('div', 'empty', '暂无额度数据'));
    return;
  }
  for (const [label, w, key] of [['5h', session, '5h'], ['7d', weekly, '7d']]) {
    if (!w) continue;
    const wcls = pctClass(w.pct);
    const row = el('div', 'wrow');
    row.appendChild(el('span', 'wrow-tag', label));
    const meter = el('div', 'meter');
    if (w.pct <= 0) meter.classList.add('empty-bad');
    const fill = el('span', wcls);
    fill.style.width = `${Math.max(2, Math.min(100, w.pct))}%`;
    meter.appendChild(fill);
    row.appendChild(meter);
    row.appendChild(el('span', `wrow-pct ${wcls}`, `${Math.round(w.pct)}%`));

    // 右侧是这个窗口最近一次重置：什么时候 + 能补多少。
    // gain 不能省——最早的那次重置常常没意义（一个已经剩 98% 的窗口只补 2%），
    // 只报时间会让人白等半小时。账号名放 title，一行放不下四样东西。
    const r = nextReset(accounts, nowMs, key);
    const cell = el('span', 'wrow-reset');
    if (r) {
      // gain 必须和这一行的百分比同口径：那个账号补满是 +52 个"账号百分点"，但池子
      // 是 240/300，它只让池子涨 52/300 ≈ 17 点。两个口径混在一行会被读成
      // "80% + 52% = 132%"。账号级的原始数字留在 title 里。
      const poolGain = r.gain === null ? null : (r.gain / w.cap) * 100;
      const gain =
        poolGain === null
          ? ''
          : poolGain > 0 && poolGain < 0.5
            ? ' +<1%' // 四舍五入成 +0% 会让人以为"白等"，但它确实不是零
            : ` +${Math.round(poolGain)}%`;
      // 今天的重置报时刻（"18:10"，你会照它安排接下来做什么），跨天的报倒计时
      // （"2d"）——"08/01 21:59" 既占不下这一列，也不是你会拿来决策的形式。
      const short = shortReset(r.at, d.today?.date);
      const when = short && short !== r.at ? short : fmtEta(r.ms - nowMs);
      // 时刻和增量拆成两个定宽列：写在一个 span 里，两行的 "+58%" / "+42%" 会被
      // 前面长度不同的时刻推得参差不齐，扫一眼比不了。
      cell.appendChild(el('span', 'wrow-when', when));
      cell.appendChild(el('span', 'wrow-gain', gain.trim()));
      cell.title =
        `${r.email.split('@')[0]} 的 ${label} 窗口 ${r.at} 重置（${fmtEta(r.ms - nowMs)} 后）` +
        (r.gain === null ? '' : `\n该账号补满 +${Math.round(r.gain)}%，池子${gain}`);
    } else {
      // 没有重置时间：窗口还没启用（余量满）或上游没给。两者都不该编个时间出来。
      cell.appendChild(el('span', 'wrow-when', '–'));
      cell.title = '暂无重置时间';
    }
    row.appendChild(cell);
    rows.appendChild(row);
  }
}

export function render(d, opts = {}) {
  const doc = opts.doc || document;
  const q = (id) => doc.getElementById(id);
  const nowHour = opts.nowHour !== undefined ? opts.nowHour : new Date().getHours();

  // 今日。不写日期："今日用量"这个标题已经说明是哪天了，而顶栏还有刷新时刻，
  // 再印一遍 2026-08-04 只是占掉一行宽度。
  q('today-tokens').textContent = fmtCompact(d.today?.total_tokens ?? 0);
  q('today-reqs').textContent = fmtNum(d.today?.request_count ?? 0);

  const dy = pctDelta(d.today?.total_tokens ?? 0, d.yesterday?.total_tokens ?? 0);
  const elY = q('delta-yday');
  elY.textContent = dy ? `${dy.text} vs 昨日` : '';
  elY.className = `delta ${dy ? dy.dir : ''}`;

  const da = pctDelta(d.today?.total_tokens ?? 0, d.avg_7d_tokens ?? 0);
  const elA = q('delta-avg');
  elA.textContent = da ? `${da.text} vs 7日均` : '';
  elA.className = `delta ${da ? da.dir : ''}`;

  renderBreakdown(q('breakdown'), d.today);
  renderSpark(q('spark'), d.hourly_tokens, nowHour, q('spark-axis'));

  // 历史热力图走独立接口（见 app.js 的懒加载），正常不会出现在 /api/tray 的
  // 响应里。这个钩子只为让 preview.mjs 往 fixture 里塞个 history 就能截到背面
  // ——CSS 改动必须真的看一眼（见 desktop/README.md）。
  if (d.history) paintHistory(doc, d.history, opts);
  else fitFlipHeight(doc); // 正面内容会随每轮刷新变高变矮，两面必须跟着重新对齐
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
