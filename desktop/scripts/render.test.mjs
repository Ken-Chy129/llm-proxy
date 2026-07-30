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
const { fmtNum, fmtCompact, pctDelta, pctClass, fmtIdle, trayTitle, alertFor } = mod;

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

test('trayTitle 优先显示最紧张的额度', () => {
  assert.equal(trayTitle({ min_session_percent: 73 }), '73%');
  // 额度用光时显示 0% 而不是退回用量——0 是最该看见的数字
  assert.equal(trayTitle({ min_session_percent: 0 }), '0%');
  // 没有额度数据时退回今日用量
  assert.equal(trayTitle({ today: { total_tokens: 2159238 } }), '2.16M');
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
  assert.match(a.body, /low 会话额度仅剩 7%/);
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
