// 渲染层纯函数的单元测试。跑法：node --test scripts/
// 这些函数决定了"1% 会不会画成看不见的一个点""额度用光是不是红的"
// 这类问题，全是肉眼容易漏、但用户一定会遇到的边界。

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const src = readFileSync(resolve(__dirname, '../src/render.js'), 'utf8');
// render.js 依赖 document，这里只取纯函数部分做数据层测试。
const mod = await import(
  'data:text/javascript;base64,' + Buffer.from(src).toString('base64')
);
const { fmtNum, fmtCompact, pctDelta, pctClass, fmtIdle, trayTitle, alertFor, shortReset, bindingWindow, poolWindow, nextReset, fmtEta } = mod;

test('fmtNum 加千分位，空值不显示 NaN', () => {
  assert.equal(fmtNum(25969045), '25,969,045');
  assert.equal(fmtNum(0), '0');
  assert.equal(fmtNum(null), '–');
  assert.equal(fmtNum(undefined), '–');
  assert.equal(fmtNum(NaN), '–');
});

test('fmtCompact 压缩大数', () => {
  assert.equal(fmtCompact(2159238), '2.16M');
  assert.equal(fmtCompact(25754202), '25.75M');
  assert.equal(fmtCompact(24485), '24.5K');
  assert.equal(fmtCompact(999), '999');
  assert.equal(fmtCompact(0), '0');
  assert.equal(fmtCompact(1500000000), '1.5B');
  // 尾随零必须去干净，不能出现 1.50B / 2.00M / 5.0K
  assert.equal(fmtCompact(2000000), '2M');
  assert.equal(fmtCompact(5000), '5K');
  assert.equal(fmtCompact(3100000), '3.1M');
  assert.equal(fmtCompact(null), '–');
});

test('pctDelta 基线为 0 时不返回无穷大', () => {
  // 第一天使用时昨日基线是 0，绝不能显示 "+Infinity%"
  assert.deepEqual(pctDelta(1000, 0), { text: 'new', dir: 'up' });
  assert.equal(pctDelta(0, 0), null);
  assert.equal(pctDelta(0, undefined), null);
});

test('pctDelta 带方向箭头且百分比不重复正负号', () => {
  const down = pctDelta(50, 100);
  assert.equal(down.text, '↓50%');
  assert.equal(down.dir, 'down');

  const up = pctDelta(150, 100);
  assert.equal(up.text, '↑50%');
  assert.equal(up.dir, 'up');

  // 数值里不该出现 "↓-50%" 这种双重否定
  assert.ok(!down.text.includes('-'), `不应含负号: ${down.text}`);

  const flat = pctDelta(100, 100);
  assert.equal(flat.dir, 'flat');
});

test('pctDelta 极端增长改用倍数，避免撑破布局', () => {
  // 1.28B vs 512K 基线 ≈ +250512%，位数太多会溢出右边界
  const huge = pctDelta(1284000000, 512345.67);
  assert.equal(huge.text, '↑2505x');
  assert.ok(huge.text.length <= 8, `太长会溢出: ${huge.text}`);
  // 阈值边界：刚好 1000% 用倍数，999% 仍用百分比
  assert.equal(pctDelta(11, 1).text, '↑10x');
  assert.equal(pctDelta(10.9, 1).text, '↑990%');
});

test('pctClass 按剩余额度分档（入参是剩余量，不是已用量）', () => {
  assert.equal(pctClass(100), 'ok', '满格应为安全色');
  assert.equal(pctClass(73), 'ok');
  assert.equal(pctClass(36), 'ok');
  assert.equal(pctClass(35), 'warn', '35% 就该预警，别等跌到 10% 才变色');
  assert.equal(pctClass(11), 'warn');
  assert.equal(pctClass(10), 'bad');
  assert.equal(pctClass(0), 'bad', '额度用光必须是警示色');
  // 未知额度不能显示成"安全"
  assert.equal(pctClass(null), 'warn');
  assert.equal(pctClass(undefined), 'warn');
});

test('fmtIdle 输出人类可读的时间差', () => {
  assert.equal(fmtIdle(27), '27s 前');
  assert.equal(fmtIdle(300), '5m 前');
  assert.equal(fmtIdle(7200), '2h 前');
  assert.equal(fmtIdle(86400 * 3), '3d 前');
  assert.equal(fmtIdle(undefined), '');
});

test('trayTitle 优先显示今日用量', () => {
  assert.equal(trayTitle({ today: { total_tokens: 2159238 } }), '2.16M');
  // 有用量就用用量，哪怕额度也在（额度靠通知和面板，不占标题栏）
  assert.equal(
    trayTitle({ min_session_percent: 73, today: { total_tokens: 172270000 } }),
    '172.27M',
  );
  // 今天还没跑请求时不写 0：退回额度，至少证明挂件连得上代理
  assert.equal(trayTitle({ min_session_percent: 73, today: { total_tokens: 0 } }), '73%');
  assert.equal(trayTitle({ min_session_percent: 0 }), '0%');
  assert.equal(trayTitle({}), '–');
  assert.equal(trayTitle(null), '–');
});

// 轴刻度选择逻辑：与 render.js 里 renderSpark 的规则保持一致。
// 提取成同一份逻辑做测试，避免"当前小时紧邻固定刻度"时两个数字挤成乱码。
function axisTicks(nowHour) {
  const fixed = [0, 6, 12, 18, 23].filter(
    (h) => nowHour === undefined || Math.abs(h - nowHour) > 1
  );
  const ticks = new Set(fixed);
  if (nowHour >= 0 && nowHour < 24) ticks.add(nowHour);
  return [...ticks].sort((a, b) => a - b);
}

test('轴刻度：相邻固定刻度让位给当前小时，避免数字重叠', () => {
  assert.deepEqual(axisTicks(11), [0, 6, 11, 18, 23]);
  // 13 紧邻 12 → 同理
  assert.deepEqual(axisTicks(13), [0, 6, 13, 18, 23]);
  // 当前小时正好是固定刻度，不重复也不丢
  assert.deepEqual(axisTicks(12), [0, 6, 12, 18, 23]);
  assert.deepEqual(axisTicks(0), [0, 6, 12, 18, 23]);
  assert.deepEqual(axisTicks(23), [0, 6, 12, 18, 23]);
  // 22 紧邻 23
  assert.deepEqual(axisTicks(22), [0, 6, 12, 18, 22]);
  // 不相邻时全部保留 + 当前小时
  assert.deepEqual(axisTicks(9), [0, 6, 9, 12, 18, 23]);
  // 任何情况下相邻两个刻度之间至少隔一格
  for (let h = 0; h < 24; h++) {
    const t = axisTicks(h);
    for (let i = 1; i < t.length; i++) {
      assert.ok(t[i] - t[i - 1] >= 2, `nowHour=${h} 刻度 ${t[i - 1]} 与 ${t[i]} 距离过近`);
    }
  }
});

// ---------- alertFor ----------
// 告警判定是这个工具的核心价值：判错就是"额度用光了但没提醒"。
// app.js 和离线预览共用这一份，所以这里锁住的行为就是实际行为。

const acct = (o) => ({
  provider: 'claude', email: 'a@example.com', status: 'active',
  has_real_data: true, session_percent: 80, weekly_percent: 90, ...o,
});

test('alertFor 额度充足时不告警', () => {
  assert.equal(alertFor({ accounts: [acct({ session_percent: 80 })] }, 20), null);
  assert.equal(alertFor({ accounts: [] }, 20), null);
  assert.equal(alertFor({}, 20), null);
  assert.equal(alertFor(null, 20), null);
});

test('alertFor 跌破阈值才告警，边界含等于', () => {
  assert.equal(alertFor({ accounts: [acct({ session_percent: 21 })] }, 20), null);
  const at = alertFor({ accounts: [acct({ session_percent: 20 })] }, 20);
  assert.ok(at, '等于阈值必须告警');
  assert.match(at.body, /仅剩 20%/);
  assert.match(at.title, /额度偏低/);
});

test('alertFor 去重键按 5% 分档：同档不重复，跌档再提醒', () => {
  const k = (p) => alertFor({ accounts: [acct({ session_percent: p })] }, 20).key;
  assert.equal(k(19), k(16), '同一 5% 档位内应产生相同的键');
  assert.notEqual(k(19), k(14), '跌到下一档必须换键，否则不会再提醒');
  assert.notEqual(k(9), k(4));
});

test('alertFor 限流优先于低额度，且不逐个列举账号', () => {
  const three = {
    accounts: [
      acct({ email: 'x@a.com', status: 'rate_limited', rate_limited_until: '21:20', session_percent: 0 }),
      acct({ email: 'y@a.com', status: 'rate_limited', rate_limited_until: '20:00', session_percent: 0 }),
      acct({ email: 'z@a.com', status: 'rate_limited', rate_limited_until: '22:40', session_percent: 0 }),
    ],
  };
  const a = alertFor(three, 20);
  assert.match(a.title, /被限流/);
  // 三个账号不能全列出来——320px 宽的面板会被撑爆
  assert.match(a.body, /3 个账号被限流/);
  assert.match(a.body, /最早 20:00 恢复/, '应报最早恢复时间');
  assert.ok(!a.body.includes('x@a.com'), '不该逐个列举邮箱');
  assert.ok(a.body.length < 40, `文案过长会撑破横幅: ${a.body}`);
});

test('alertFor 单个限流账号仍显示具体信息', () => {
  const one = {
    accounts: [
      acct({ email: 'solo@a.com', status: 'rate_limited', rate_limited_until: '21:20', session_percent: 0 }),
      acct({ email: 'ok@a.com', session_percent: 90 }),
    ],
  };
  const a = alertFor(one, 20);
  assert.match(a.body, /solo 至 21:20/);
});

test('alertFor 去重键与账号顺序无关', () => {
  const mk = (emails) => ({
    accounts: emails.map((e) =>
      acct({ email: e, status: 'rate_limited', rate_limited_until: '21:00', session_percent: 0 })),
  });
  assert.equal(
    alertFor(mk(['a@x.com', 'b@x.com']), 20).key,
    alertFor(mk(['b@x.com', 'a@x.com']), 20).key,
    '顺序变化不该被当成新告警重复推送'
  );
});

test('alertFor 忽略停用账号和无额度数据的账号', () => {
  // 停用账号的残余额度没有意义，不该拉低判定
  assert.equal(
    alertFor({ accounts: [acct({ status: 'disabled', session_percent: 0 }), acct({ session_percent: 90 })] }, 20),
    null
  );
  // 没抓到真实额度时不能凭 0 值误报
  assert.equal(
    alertFor({ accounts: [acct({ has_real_data: false, session_percent: 0 })] }, 20),
    null
  );
});

test('alertFor 取最紧张的账号而非第一个', () => {
  const a = alertFor({
    accounts: [
      acct({ email: 'high@a.com', session_percent: 90 }),
      acct({ email: 'low@a.com', session_percent: 7 }),
      acct({ email: 'mid@a.com', session_percent: 50 }),
    ],
  }, 20);
  assert.match(a.body, /low 5h 额度仅剩 7%/);
});

test('alertFor 7d 见底也告警，且文案点明窗口', () => {
  // 只盯 5h 会漏掉这种：小时窗口很宽裕，但周额度已经快用光
  const a = alertFor({ accounts: [acct({ email: 'k@a.com', session_percent: 80, weekly_percent: 8 })] }, 20);
  assert.ok(a, '7d 跌破阈值必须告警');
  assert.match(a.body, /k 7d 额度仅剩 8%/);
});

test('alertFor 两个窗口都低时报更紧张的那个', () => {
  const w = alertFor({ accounts: [acct({ session_percent: 18, weekly_percent: 6 })] }, 20);
  assert.match(w.body, /7d 额度仅剩 6%/);
  const s = alertFor({ accounts: [acct({ session_percent: 6, weekly_percent: 18 })] }, 20);
  assert.match(s.body, /5h 额度仅剩 6%/);
  // 打平时报 5h，与 bindingWindow 的取舍一致
  const tie = alertFor({ accounts: [acct({ session_percent: 9, weekly_percent: 9 })] }, 20);
  assert.match(tie.body, /5h 额度仅剩 9%/);
});

test('alertFor 去重键区分窗口：5h 报过之后 7d 还能再响', () => {
  const k = (o) => alertFor({ accounts: [acct(o)] }, 20).key;
  assert.notEqual(
    k({ session_percent: 12, weekly_percent: 90 }),
    k({ session_percent: 90, weekly_percent: 12 }),
    '同档位但不同窗口是两件事，键不能相同'
  );
});

test('alertFor 缺 weekly 数据时只按 5h 判定', () => {
  const a = alertFor({ accounts: [acct({ session_percent: 12, weekly_percent: null })] }, 20);
  assert.match(a.body, /5h 额度仅剩 12%/);
  // 反过来也一样：缺 5h 不能让账号逃过判定
  const b = alertFor({ accounts: [acct({ session_percent: null, weekly_percent: 12 })] }, 20);
  assert.match(b.body, /7d 额度仅剩 12%/);
});

test('alertFor 缺少恢复时间也不产出 undefined 文案', () => {
  const a = alertFor({
    accounts: [
      acct({ email: 'p@a.com', status: 'rate_limited', session_percent: 0 }),
      acct({ email: 'q@a.com', status: 'rate_limited', session_percent: 0 }),
    ],
  }, 20);
  assert.ok(!/undefined|null/.test(a.body), `文案不该出现 undefined: ${a.body}`);
  assert.match(a.body, /2 个账号被限流/);
});

test('shortReset 当天只留时间，跨天保留日期', () => {
  // 当天：日期是冗余的，5h 窗口绝大多数情况都落在今天
  assert.equal(shortReset('07/31 16:00', '2026-07-31'), '16:00');
  // 跨天：必须留日期，否则凌晨 4 点会被读成"已经过去了"
  assert.equal(shortReset('08/01 04:00', '2026-07-31'), '08/01 04:00');
  // 月份/日子的补零要严格匹配，别把 08/01 误判成 8/1
  assert.equal(shortReset('08/01 04:00', '2026-08-01'), '04:00');
  // 缺任一侧信息时原样返回：宁可啰嗦，也不能显示错的时间
  assert.equal(shortReset('07/31 16:00', undefined), '07/31 16:00');
  assert.equal(shortReset('', '2026-07-31'), '');
  assert.equal(shortReset(undefined, '2026-07-31'), '');
  // 日期前缀相同但没有空格分隔时不能截（防 "07/311" 这类脏数据被切成 "1"）
  assert.equal(shortReset('07/3116:00', '2026-07-31'), '07/3116:00');
});

test('pctDelta 不把接近 100% 的降幅舍成 100%', () => {
  // 这条是真实踩到的 bug：今天 257.5K、昨天 12.6M，真实降幅 -97.96%
  assert.equal(pctDelta(257_500, 12_600_000).text, '↓98%');
  // -99.6% 舍成 ↓100% 会被读成"今天一个 token 都没花"
  assert.equal(pctDelta(40_000, 10_000_000).text, '↓99.6%');
  // 99.96% 用 toFixed(1) 会变回 "100.0"，必须 floor
  assert.equal(pctDelta(4_000, 10_000_000).text, '↓99.9%');
  // 真的降到 0 时，100% 是准确的，不该被改掉
  assert.equal(pctDelta(0, 10_000_000).text, '↓100%');
  // 增长侧超过 100% 不歧义，照常显示
  assert.equal(pctDelta(25_000, 10_000).text, '↑150%');
  // 极小变化也不能显示成 0%（会被读成"持平"）
  assert.equal(pctDelta(9_996, 10_000).text, '↓<1%');
  assert.equal(pctDelta(10_004, 10_000).text, '↑<1%');
  // 边界另一侧：1% 及以上仍是整数
  assert.equal(pctDelta(9_900, 10_000).text, '↓1%');
});

test('bindingWindow 取 5h 与周里更紧的那个', () => {
  assert.deepEqual(bindingWindow({ session_percent: 100, weekly_percent: 71 }), { win: '周', pct: 71 });
  assert.deepEqual(bindingWindow({ session_percent: 12, weekly_percent: 80 }), { win: '5h', pct: 12 });
  // 只有一个窗口有数据时用它，别把缺失当 0
  assert.deepEqual(bindingWindow({ session_percent: 40 }), { win: '5h', pct: 40 });
  assert.deepEqual(bindingWindow({ weekly_percent: 40 }), { win: '周', pct: 40 });
  // 0% 是有效值（用光了），不能被当成"没数据"
  assert.deepEqual(bindingWindow({ session_percent: 0, weekly_percent: 90 }), { win: '5h', pct: 0 });
  assert.equal(bindingWindow({}), null);
  assert.equal(bindingWindow(null), null);
});



test('poolWindow 把各账号余量相加：窗口是独立结算的', () => {
  const acct = (o) => ({ has_real_data: true, status: 'active', email: 'x@y.com', ...o });
  // 用户的真实场景：三个账号 5h 分别剩 100/98/51
  const s = poolWindow([
    acct({ session_percent: 100 }), acct({ session_percent: 98 }), acct({ session_percent: 51 }),
  ], 'session_percent');
  assert.equal(s.sum, 249);
  assert.equal(s.cap, 300);
  assert.equal(s.count, 3);
  assert.equal(Math.round(s.pct), 83);

  // 被限流的账号要计入：它此刻的余量就是接近 0，掩盖它会让总量虚高
  const withLimited = poolWindow([
    acct({ status: 'rate_limited', session_percent: 0 }), acct({ session_percent: 60 }),
  ], 'session_percent');
  assert.equal(withLimited.sum, 60);
  assert.equal(withLimited.cap, 200);

  // 停用的不计入，连 cap 也不占——它服务不了流量
  const withDisabled = poolWindow([
    acct({ status: 'disabled', session_percent: 100 }), acct({ session_percent: 60 }),
  ], 'session_percent');
  assert.equal(withDisabled.cap, 100);
  assert.equal(withDisabled.sum, 60);

  // 缺这个窗口数据的账号不占 cap，否则百分比会被无端压低
  const partial = poolWindow([acct({ session_percent: 80 }), acct({ weekly_percent: 50 })], 'session_percent');
  assert.equal(partial.cap, 100);

  assert.equal(poolWindow([], 'session_percent'), null);
  assert.equal(poolWindow(undefined, 'session_percent'), null);
});

test('nextReset 取最早的未来重置，并给出能补回多少', () => {
  const now = 1_800_000_000_000; // 固定时刻，避免测试随时间漂移
  const acct = (o) => ({ has_real_data: true, status: 'active', ...o });
  const r = nextReset([
    acct({ email: 'a@x.com', session_percent: 98, session_reset_unix: now / 1000 + 2520,
           session_reset_at: '07/31 16:50', weekly_percent: 74, weekly_reset_unix: now / 1000 + 400000 }),
    acct({ email: 'b@x.com', session_percent: 51, session_reset_unix: now / 1000 + 7200 }),
  ], now);
  assert.equal(r.email, 'a@x.com');
  assert.equal(r.win, '5h');
  assert.equal(r.at, '07/31 16:50');
  // 98% 的窗口重置只补 2%——不给出 gain 的话，用户会以为等 42 分钟就宽裕了
  assert.equal(r.gain, 2);

  // 已经过去的重置时间是过期快照，必须忽略，否则倒计时会显示负数
  assert.equal(nextReset([acct({ session_reset_unix: now / 1000 - 60 })], now), null);
  // 没有时间戳时返回 null，而不是拿 0 当 1970 年
  assert.equal(nextReset([acct({ session_percent: 50 })], now), null);
  assert.equal(nextReset([], now), null);
});

test('fmtEta 分钟/小时/天，且不出现 0m', () => {
  assert.equal(fmtEta(42 * 60000), '42m');
  assert.equal(fmtEta(30000), '即将');       // 不到 1 分钟别显示 0m
  assert.equal(fmtEta(60 * 60000), '1h');    // 整点小时不带 0m
  assert.equal(fmtEta(200 * 60000), '3h20m');
  assert.equal(fmtEta(50 * 3600000), '2d');
  assert.equal(fmtEta(0), '');
  assert.equal(fmtEta(-5000), '');           // 时间已过就什么都不显示
  assert.equal(fmtEta(null), '');
});

// ---------- 历史用量热力图 ----------

const { heatLevel, buildHeatGrid } = mod;

test('heatLevel 有流量就至少 1 档，不和"完全没用"同色', () => {
  assert.equal(heatLevel(0, 1000), 0);
  assert.equal(heatLevel(1, 1000000), 1, '一天只跑了 1 个请求也该看得见');
  assert.equal(heatLevel(1000, 1000), 4, '最大值落在最深一档');
});

test('heatLevel 开平方压缩：一个尖峰不该把其余日子全压进第 1 档', () => {
  // 实测分布：峰值 2405、常态 200 上下。线性分档下 200/2405≈8% 会掉到第 1 档，
  // 和一天只有 1 个请求的日子同色；开平方后能区分开。
  const peak = 2405;
  assert.ok(heatLevel(200, peak) > heatLevel(5, peak),
    '常态日必须比几乎没用的日子更深');
  assert.equal(heatLevel(peak, peak), 4);
  // 单调不降
  let prev = 0;
  for (const v of [0, 1, 50, 200, 600, 1200, 2405]) {
    const l = heatLevel(v, peak);
    assert.ok(l >= prev, `heatLevel(${v}) = ${l} 比前一档还小`);
    prev = l;
  }
});

test('heatLevel 边界输入不炸', () => {
  assert.equal(heatLevel(0, 0), 0);
  assert.equal(heatLevel(-5, 100), 0, '负数按无流量处理');
  assert.equal(heatLevel(100, 0), 0, 'max 为 0 时不做除法');
  assert.equal(heatLevel(500, 100), 4, '超过 max 也封顶在最深一档，不越界');
});

test('buildHeatGrid 铺成整周网格：列=周、行=星期固定', () => {
  const today = new Date(2026, 7, 4); // 2026-08-04，周二
  const cells = buildHeatGrid([], 25, today);
  assert.equal(cells.length, 25 * 7, '25 列 × 7 行');
  assert.equal(cells[0].date.getDay(), 0, '第一格必须是周日，否则星期会错行');
  assert.equal(cells[cells.length - 1].date.getDay(), 6, '最后一格是周六');
});

test('buildHeatGrid 把稀疏的接口数据按日期对齐，缺的天补零', () => {
  const today = new Date(2026, 7, 4);
  const cells = buildHeatGrid([{ d: '2026-08-04', t: 45139347, r: 193, c: 45006866 }], 25, today);
  const hit = cells.find((c) => c.key === '2026-08-04');
  assert.ok(hit, '今天必须在网格里');
  assert.equal(hit.requests, 193);
  assert.equal(hit.tokens, 45139347);
  assert.equal(hit.isToday, true);
  // 没有数据的那天补零，而不是 undefined（否则 heatLevel 会拿到 NaN）
  const miss = cells.find((c) => c.key === '2026-08-01');
  assert.equal(miss.tokens, 0);
  assert.equal(miss.requests, 0);
});

test('buildHeatGrid 标出未来的格子：那不是"没用量"，是还没到', () => {
  const today = new Date(2026, 7, 4); // 周二，本周还剩周三~周六
  const cells = buildHeatGrid([], 25, today);
  const future = cells.filter((c) => c.future);
  assert.equal(future.length, 4, '08-05 到 08-08 共 4 天');
  assert.equal(cells.filter((c) => c.isToday).length, 1, '有且只有一个"今天"');
  // 今天不能被算成未来
  assert.equal(cells.find((c) => c.isToday).future, false);
});

test('热力图列数与 CSS 格子尺寸必须对得上，否则会溢出卡片', () => {
  // HEAT_WEEKS 在 render.js、格子尺寸在 style.css，是一处真实的跨文件耦合：
  // 只调其中一个，网格就会顶出卡片右边缘——而这种事只有截图才看得出来。
  // 这里把它变成一条会红的断言。
  const css = readFileSync(resolve(__dirname, '../src/style.css'), 'utf8');
  const grid = css.match(/\.heat-grid\s*\{[^}]*\}/s);
  assert.ok(grid, '找不到 .heat-grid 规则');
  const cell = Number(grid[0].match(/grid-auto-columns:\s*(\d+)px/)[1]);
  const gap = Number(grid[0].match(/gap:\s*(\d+)px/)[1]);
  const rows = Number(grid[0].match(/repeat\(7,\s*(\d+)px\)/)[1]);
  assert.equal(rows, cell, '格子必须是正方形，否则行列不等宽会歪');

  const weeks = Number(readFileSync(resolve(__dirname, '../src/render.js'), 'utf8')
    .match(/const HEAT_WEEKS = (\d+)/)[1]);
  // 卡内可用宽度，在真实 320px 面板里实测得来（不是按 padding 算的 276——
  // 差那 2px 刚好能让一列悄悄溢出）。preview.mjs 已把 #app 钉死在 320px，
  // 想重新量就跑一次 --flip 预览、读 .flip-back 的 clientWidth 减 padding。
  const CARD_INNER = 274;
  const width = weeks * cell + (weeks - 1) * gap;
  assert.ok(width <= CARD_INNER,
    `${weeks} 列 × ${cell}px（间隙 ${gap}）= ${width}px，超过卡内可用宽度 ${CARD_INNER}px`);
  // 也不该浪费太多：留白超过一格就说明还能多放一周
  assert.ok(CARD_INNER - width < cell + gap,
    `只用了 ${width}px / ${CARD_INNER}px，还能再放一列`);
});

test('heatDayLines 有请求但没 token 记录时不谎报"无用量"', () => {
  const { heatDayLines } = mod;
  // 早期未采集 token 的日子：有 2405 个请求，token 记 0
  const real = heatDayLines({ key: '2026-06-05', requests: 2405, tokens: 0, cache: 0 });
  assert.equal(real.date, '06/05');
  assert.deepEqual(real.rows[0], ['请求', '2,405']);
  assert.deepEqual(real.rows[1], ['tokens', '无记录']);

  // 完全没用的日子才说"无"
  const idle = heatDayLines({ key: '2026-06-06', requests: 0, tokens: 0, cache: 0 });
  assert.deepEqual(idle.rows, [['用量', '无']]);

  // 正常的一天：缓存占比按 token 算
  // token 走 fmtCompact（1000 → 1K），请求数走千分位——两者格式化方式不同是刻意的：
  // 请求数是可数的次数，压成 "1.2K 请求" 反而丢了精度感
  const busy = heatDayLines({ key: '2026-08-04', requests: 779, tokens: 1000, cache: 900 });
  assert.deepEqual(busy.rows, [['请求', '779'], ['tokens', '1K'], ['缓存', '90%']]);
});
