// 应用层：配置持久化、轮询、Tauri 托盘/通知桥接。
// 渲染逻辑全在 render.js 里（纯函数，可离线预览）。

import { render, renderFloat, trayTitle, alertFor, paintHistory, HEAT_WEEKS } from './render.js';

const DEFAULTS = {
  base: '',
  // 代理的 tray token（server.tray_token），不是 Keys 页面那种 sk- API Key：
  // 后者属于流量平面、能花额度，/api/tray 已经不再接受它。
  token: '',
  interval: 60,
  threshold: 20,
  notify: true,
};

const LS_KEY = 'llm-proxy-tray-config';

function loadConfig() {
  try {
    return { ...DEFAULTS, ...JSON.parse(localStorage.getItem(LS_KEY) || '{}') };
  } catch {
    return { ...DEFAULTS };
  }
}

function saveConfig(cfg) {
  localStorage.setItem(LS_KEY, JSON.stringify(cfg));
}

/**
 * 代理根地址：去掉尾斜杠，也去掉尾部的 /v1。
 *
 * /v1 那一步是给自动探测用的：ANTHROPIC_BASE_URL 有人会写成 https://host/v1，
 * 而我们要拼的是 {base}/api/tray，不处理就会变成 /v1/api/tray 打到 404。
 */
function normalizeBase(raw) {
  return (raw || '').trim().replace(/\/+$/, '').replace(/\/v1$/, '');
}

let config = loadConfig();
let timer = null;
let lastAlertKey = ''; // 同一个额度窗口只提醒一次，避免每轮轮询都弹通知

// 悬浮模式：窗口自己就是挂件，不需要"打开悬浮窗"按钮，而是需要置顶开关。
const IS_FLOAT = new URLSearchParams(location.search).get('mode') === 'float';
let floatOnTop = true;

/**
 * 解析 invoke 函数。
 *
 * Tauri v2 的全局对象只导出 {app, core, dpi, event, image, menu, path, tray,
 * webview, webviewWindow, window} —— invoke 在 `__TAURI__.core.invoke`，**没有**
 * 顶层的 `__TAURI__.invoke`（那是 v1 的形状）。写死任何一种都会让所有命令静默
 * 失效：`T.invoke is not a function` 会被 catch 吞掉，于是托盘标题不更新、悬浮窗
 * 按钮没反应、Esc 关不掉面板，而控制台之外毫无提示。两种都探测。
 */
function resolveInvoke() {
  const T = window.__TAURI__;
  if (!T) return null; // 浏览器离线预览，属正常情况
  const fn = (T.core && T.core.invoke) || T.invoke;
  return typeof fn === 'function' ? fn : null;
}

/** Tauri 不存在时软失败（浏览器预览）；存在但桥接坏了要吵，不能静默。 */
let bridgeWarned = false;
async function invoke(cmd, args) {
  const fn = resolveInvoke();
  if (!fn) {
    // 有 __TAURI__ 却拿不到 invoke = 真的坏了，不是预览环境。
    if (window.__TAURI__ && !bridgeWarned) {
      bridgeWarned = true;
      console.error('Tauri invoke 不可用，托盘标题与窗口控制将失效');
      showError('Tauri 桥接不可用：托盘标题和悬浮窗按钮无法工作');
    }
    return null;
  }
  try {
    return await fn(cmd, args);
  } catch (e) {
    console.warn(`invoke ${cmd} failed`, e);
    return null;
  }
}

/**
 * 取通知 API。
 *
 * withGlobalTauri 保证核心 API 挂在 window.__TAURI__ 上，但插件挂载的位置
 * 在不同版本间不完全一致（有的是 __TAURI__.notification，有的挂在
 * __TAURI_PLUGIN_NOTIFICATION__）。这里逐个探测而不是假定某一种，
 * 因为猜错的后果是告警永久静默——而告警恰好是这个工具的核心价值。
 */
function notificationAPI() {
  const T = window.__TAURI__;
  const candidates = [
    T && T.notification,
    window.__TAURI_PLUGIN_NOTIFICATION__,
    T && T.plugins && T.plugins.notification,
  ];
  for (const api of candidates) {
    if (api && typeof api.sendNotification === 'function') return api;
  }
  return null;
}

function setDot(state) {
  const dot = document.getElementById('live-dot');
  if (dot) dot.className = `dot ${state}`;
  if (!IS_FLOAT) return;
  // 悬浮球第二行平时是"下次回血"的倒计时；取不到数据时改写成状态——上面那个百分比
  // 已经是旧值了，必须让人看出来，而倒计时此刻也不再有意义。
  const ball = document.getElementById('ball');
  const eta = document.getElementById('ball-eta');
  const offline = state !== 'live';
  if (ball) ball.classList.toggle('offline', offline);
  if (eta && offline) eta.textContent = state === 'dead' ? '离线' : '过期';
}

/* 悬浮卡只有一行状态位，而面板有两条（错误条 + 告警条）。这两个变量把"当前该
   显示什么"集中在一处：取不到数据比额度偏低更紧急，所以错误优先。 */
let lastErrMsg = '';
let lastAlertMsg = '';

/** 把状态画到悬浮卡那一行。折叠态（56px）放不下文字，靠球上的状态灯表达。 */
function paintFloatStatus() {
  const box = document.getElementById('fcard-err');
  if (!box) return;
  const msg = lastErrMsg || lastAlertMsg;
  box.textContent = msg;
  box.className = `fcard-err${lastErrMsg ? '' : ' warn'}${msg ? '' : ' hidden'}`;
  // 多/少一行会改变卡片高度，展开着的话得同步窗口，否则这行会被切掉
  syncFloatSize();
}

function showError(msg) {
  lastErrMsg = msg || '';
  const box = document.getElementById('error');
  if (box) {
    box.textContent = msg || '';
    box.classList.toggle('hidden', !msg);
  }
  paintFloatStatus();
}

/**
 * 告警横幅。跟 showError 分开是必要的：refresh 成功时会清掉 error，
 * 而告警按额度分档去重（同一档只提醒一次），如果共用一个容器，
 * 提示会在下一轮刷新时被抹掉且不再重现。
 */
function showAlert(msg) {
  lastAlertMsg = msg || '';
  paintFloatStatus();
  const box = document.getElementById('alert');
  if (!msg) {
    box.classList.add('hidden');
    box.textContent = '';
    box.removeAttribute('title');
    return;
  }
  box.textContent = msg;
  // 横幅是 nowrap + ellipsis，长文案会被截断；悬停能看全文
  box.title = msg;
  box.classList.remove('hidden');
}

/** 额度跌破阈值时发系统通知；恢复后重置，以便下次再触发。 */
async function maybeNotify(d) {
  if (!config.notify) {
    showAlert('');
    return;
  }

  // 判定逻辑在 render.js 里，跟离线预览共用，避免两处走样
  const alert = alertFor(d, config.threshold);

  if (!alert) {
    // 已恢复：清掉去重键，也把横幅收起来
    lastAlertKey = '';
    showAlert('');
    return;
  }
  if (alert.key === lastAlertKey) return; // 同一档位不重复打扰
  lastAlertKey = alert.key;

  const { title, body } = alert;
  // 面板横幅只用 body：title 是给系统通知的（通知需要独立标题），
  // 拼在一起会变成"账号被限流：2 个账号被限流"这种同义重复，还会撑到换行。
  const banner = `⚠ ${body}`;

  const api = notificationAPI();
  if (!api) {
    // 不静默：告警发不出去必须让用户知道，否则会误以为"一直没超阈值"。
    // 面板里仍然有颜色和数字，所以降级后信息没丢，只是不会主动推送。
    console.warn('通知插件不可用，改为仅在面板内提示');
    showAlert(`${banner}（系统通知不可用）`);
    return;
  }
  try {
    let granted = await api.isPermissionGranted();
    if (!granted) granted = (await api.requestPermission()) === 'granted';
    if (granted) {
      await api.sendNotification({ title, body });
      // 面板里也留一条，方便回看刚才为什么弹了通知
      showAlert(banner);
    } else {
      showAlert(`${banner}（未授予通知权限）`);
    }
  } catch (e) {
    console.warn('sendNotification failed', e);
    showAlert(banner);
  }
}

// ---------- 历史用量（卡片背面） ----------
// 走独立接口，只在第一次翻到背面时拉。缓存 5 分钟：热力图的最小粒度是一天，
// 翻来翻去重复请求几百天的数据毫无意义。
const HISTORY_TTL_MS = 5 * 60 * 1000;
let historyCache = null;      // { at, days }
let historyInFlight = null;

function histMsg(text) {
  const box = document.getElementById('history');
  if (!box) return;
  box.textContent = '';
  const p = document.createElement('div');
  p.className = 'hist-msg';
  p.textContent = text;
  box.appendChild(p);
}

async function loadHistory() {
  if (historyCache && Date.now() - historyCache.at < HISTORY_TTL_MS) {
    paintHistory(document, historyCache.days);
    return;
  }
  // 连点会打出多个并发请求，共用同一个 promise
  if (historyInFlight) return historyInFlight;

  histMsg('加载中…');
  const tz = -new Date().getTimezoneOffset();
  // 只取热力图真正画得出的天数：多拉的部分既不显示、也会让汇总行的"活跃天数/总量"
  // 和眼前的格子对不上。
  const url = `${config.base.replace(/\/+$/, '')}/api/tray/history?tz=${tz}&days=${HEAT_WEEKS * 7}`;
  historyInFlight = (async () => {
    try {
      const res = await fetch(url, {
        headers: { Authorization: `Bearer ${config.token}` },
        cache: 'no-store',
      });
      if (!res.ok) throw new Error(`代理返回 ${res.status}`);
      const data = await res.json();
      historyCache = { at: Date.now(), days: data.days || [] };
      paintHistory(document, historyCache.days);
    } catch (e) {
      // 只在背面内联报错，不弹全局 showError——那是给"整个挂件取不到数"用的，
      // 而正面此刻显示的今日数据是好的，不该被一并标成失败。
      histMsg(`历史数据加载失败：${e.message}`);
    } finally {
      historyInFlight = null;
    }
  })();
  return historyInFlight;
}

function toggleUsageFlip() {
  const card = document.getElementById('usage-flip');
  // 刻意不设 card.title：原生 tooltip 会用祖先的标题盖住后代的，卡片上挂一条
  // "点击返回今日用量"就再也看不到热力图每一格自己的日用量了。⇄ 图标已够自解释。
  if (card.classList.toggle('flipped')) loadHistory();
}

async function refresh(manual = false) {
  if (!config.base || !config.token) {
    // 悬浮球里没有设置页——56px 装不下表单，也不该在桌面挂件上配凭证
    showError(IS_FLOAT ? '未配置，去菜单栏面板里设置' : '还没配置代理地址和 Tray Token，点右上角 ⚙ 设置。');
    setDot('dead');
    if (!IS_FLOAT) openSettings();
    return;
  }

  const btn = document.getElementById('btn-refresh');
  if (manual) btn.classList.add('spin');

  // 后端按这个偏移算日历天，必须传浏览器本地时区，否则“今日”会错几小时。
  const tz = -new Date().getTimezoneOffset();
  const url = `${config.base.replace(/\/+$/, '')}/api/tray?tz=${tz}`;

  try {
    const res = await fetch(url, {
      headers: { Authorization: `Bearer ${config.token}` },
      cache: 'no-store',
    });
    if (res.status === 401 || res.status === 403) {
      // 最常见的原因是拿旧的 sk- API Key 来打：/api/tray 现在只认 tray token。
      throw new Error('Tray Token 无效（401/403），去 Dashboard → Config → Admin 取');
    }
    if (!res.ok) {
      throw new Error(`代理返回 ${res.status}`);
    }
    const data = await res.json();

    showError('');
    if (IS_FLOAT) {
      lastData = data;
      renderFloat(data);
      // 账号数变了展开卡就会变高，折叠态量不到就没法正确撑开窗口
      syncFloatSize();
    } else {
      render(data);
    }
    setDot('live');

    const title = trayTitle(data);
    await invoke('set_tray_title', { title });
    await maybeNotify(data);
  } catch (e) {
    setDot('dead');
    showError(`刷新失败：${e.message}`);
  } finally {
    if (manual) setTimeout(() => btn.classList.remove('spin'), 400);
  }
}

/* ---------- 悬浮球的折叠/展开 ----------
   窗口尺寸就是交互本身：折叠态 56x56 只装得下球，鼠标移上去把窗口撑到"卡片 + 缝 +
   球"，球留在原地不动。

   这里最麻烦的不是展开，是**展开和拖拽会互相打架**：
   - 拖拽中窗口一直在动，hover 事件会被反复触发，展开/收回和 OS 的拖拽抢同一个窗口，
     手感就是一顿一顿的；
   - 展开态窗口有 228px 宽，靠屏幕右边缘拖时，窗口右侧撞到边界就再也推不过去，
     表现为"球没法贴到最右边"；
   - mouseleave 在 macOS 上不保证送达（窗口刚被挪过之后尤其容易漏），漏一次就永久
     摊开，鼠标移开也不收。
   所以规则定死两条：**按下鼠标立刻收回成球**（拖的永远是那个 56px 的小窗口），
   以及**展开期间用看门狗轮询鼠标位置**，不完全依赖事件。 */
const BALL_BOX = 56;    // 与球等大；阴影由原生窗口画在窗口外面
const CARD_BOX_W = 228; // 与卡等宽
const FLOAT_GAP = 6;    // 卡片和球之间透出桌面的缝，别让两块贴成一坨
let collapseTimer = null;
let watchdog = null;
let floatExpanded = false;
let dragging = false;
let etaTimer = null;
let lastData = null; // 倒计时自转时复用上一轮数据，不额外发请求

/** 量出展开卡的真实高度。卡片是 visibility:hidden（有布局）所以随时可测。 */
function floatCardBox() {
  const card = document.getElementById('fcard');
  const h = card ? Math.ceil(card.getBoundingClientRect().height) : 90;
  // 窗口要同时装下卡片和球：球留在原位，卡片浮在它上方，中间留 GAP 透出桌面
  return { w: CARD_BOX_W, h: h + FLOAT_GAP + BALL_BOX };
}

/** 展开态下账号数变化后同步窗口高度，否则新增的行会被窗口切掉。 */
function syncFloatSize() {
  if (!floatExpanded) return;
  const { w, h } = floatCardBox();
  invoke('expand_float', { width: w, height: h });
}

async function expandFloat() {
  clearTimeout(collapseTimer);
  if (floatExpanded || dragging) return;
  floatExpanded = true;
  const { w, h } = floatCardBox();
  // Rust 侧算出球该待在窗口的哪个角（球不动，卡片浮到它旁边），前端只把结果翻译成
  // 定位 class
  const layout = await invoke('expand_float', { width: w, height: h });
  document.body.classList.toggle('ball-bottom', layout ? layout.ball_bottom !== false : true);
  document.body.classList.toggle('ball-left', layout ? layout.ball_left !== false : true);
  document.body.classList.add('expanded');

  // 看门狗：每 300ms 问一次 Rust"鼠标还在窗口里吗"。漏事件的代价太大（永久摊开），
  // 而这个轮询只在展开期间跑。
  clearInterval(watchdog);
  watchdog = setInterval(async () => {
    if (!floatExpanded) return;
    const inside = await invoke('cursor_over_float');
    if (inside === false) collapseNow();
  }, 300);
}

/** 立即收回，不走 200ms 缓冲——按下鼠标要拖的时候必须马上变回小球。 */
async function collapseNow() {
  clearTimeout(collapseTimer);
  clearInterval(watchdog);
  watchdog = null;
  if (!floatExpanded) return;
  floatExpanded = false;
  document.body.classList.remove('expanded');
  await invoke('collapse_float');
}

function collapseFloat() {
  clearTimeout(collapseTimer);
  // 200ms 缓冲：鼠标从球滑到卡、或擦着边缘走过时不该来回抽搐
  collapseTimer = setTimeout(collapseNow, 200);
}

function restartTimer() {
  if (timer) clearInterval(timer);
  const sec = Math.max(10, Number(config.interval) || 60);
  timer = setInterval(() => refresh(false), sec * 1000);

  // 倒计时必须独立自转：轮询间隔可以被设成几分钟，那时球上的"42m"会长时间不动，
  // 而它恰好是挂件上唯一随时间变化的信息。30s 重画一次，不发请求。
  if (!IS_FLOAT) return;
  if (etaTimer) clearInterval(etaTimer);
  etaTimer = setInterval(() => {
    if (lastData) renderFloat(lastData);
  }, 30000);
}

function openSettings() {
  document.getElementById('in-base').value = config.base;
  document.getElementById('in-token').value = config.token;
  document.getElementById('in-interval').value = config.interval;
  document.getElementById('in-threshold').value = config.threshold;
  document.getElementById('in-notify').checked = !!config.notify;
  document.getElementById('panel').classList.add('hidden');
  document.getElementById('settings').classList.remove('hidden');
}

function closeSettings() {
  document.getElementById('settings').classList.add('hidden');
  document.getElementById('panel').classList.remove('hidden');
}

function wire() {
  if (IS_FLOAT) {
    // 悬浮模式没有顶栏，交互全在球身上：hover 展开、按下即拖。
    document.documentElement.addEventListener('mouseenter', expandFloat);
    document.documentElement.addEventListener('mouseleave', collapseFloat);

    // 只有球是拖拽把手。走 start_float_drag 而不是 data-tauri-drag-region：那个属性
    // 会在收回之前就让系统接管窗口，于是拖的是 228px 的展开态，推不到屏幕右边缘。
    document.getElementById('ball').addEventListener('mousedown', (e) => {
      if (e.button !== 0) return;
      dragging = true;
      clearTimeout(collapseTimer);
      clearInterval(watchdog);
      floatExpanded = false;
      document.body.classList.remove('expanded');
      invoke('start_float_drag');
    });
    // 拖拽由 OS 接管窗口，期间 webview 未必收得到 mouseup，所以再兜一层：只要看到
    // 一次"按键已经松开"的移动事件就复位。否则 dragging 卡在 true，hover 永久失效。
    document.addEventListener('mouseup', () => { dragging = false; });
    document.addEventListener('mousemove', (e) => {
      if (dragging && e.buttons === 0) dragging = false;
    });
    window.addEventListener('blur', () => { dragging = false; });

    // 置顶开关删掉了：挂件常驻桌面本来就该压在最上面，56px 的球几乎挡不住东西，
    // 那个按钮从没被按过却一直占着卡片右上角。真要临时收起来，托盘菜单里有开关。
    // 悬浮模式不需要下面这些面板专属的绑定，提前返回免得去找不存在的按钮
    window.__refresh = () => refresh(true);
    return;
  }

  document.getElementById('btn-pin').classList.add('hidden');
  document.getElementById('btn-float').addEventListener('click', () => {
    invoke('toggle_float');
  });

  document.getElementById('usage-flip').addEventListener('click', toggleUsageFlip);

  document.getElementById('btn-refresh').addEventListener('click', () => refresh(true));
  document.getElementById('btn-settings').addEventListener('click', openSettings);
  document.getElementById('btn-cancel').addEventListener('click', closeSettings);
  document.getElementById('btn-save').addEventListener('click', () => {
    config = {
      base: normalizeBase(document.getElementById('in-base').value),
      token: document.getElementById('in-token').value.trim(),
      interval: Math.max(10, Number(document.getElementById('in-interval').value) || 60),
      threshold: Math.min(99, Math.max(1, Number(document.getElementById('in-threshold').value) || 20)),
      notify: document.getElementById('in-notify').checked,
    };
    saveConfig(config);
    lastAlertKey = '';
    closeSettings();
    restartTimer();
    refresh(true);
  });

  // Esc 关闭面板（悬浮挂件不该被 Esc 关掉）
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !IS_FLOAT) invoke('hide_panel');
  });

  // 面板每次弹出时立刻刷新一次，别让用户看着过期数字。
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refresh(false);
  });

  // 供托盘菜单“立即刷新”调用
  window.__refresh = () => refresh(true);
}

/**
 * 首次启动时从 shell 环境补齐配置，省掉手填。
 *
 * 只填空缺的字段——手动填过的值永远优先，否则用户的覆盖会被环境变量默默抹掉。
 * 探测失败（浏览器预览、没设变量、shell 超时）就静默跳过，refresh 会照旧引导去
 * 设置页；这一步是省事，不是必要条件。
 */
async function autofillConfig() {
  if (config.base && config.token) return;
  const got = await invoke('detect_env_config');
  if (!got) return;

  const base = normalizeBase(got.base);
  const token = (got.token || '').trim();
  if (!config.base && base) config.base = base;
  if (!config.token && token) config.token = token;
  if (config.base || config.token) saveConfig(config);
}

wire();
if (IS_FLOAT) document.body.classList.add('float');
// 探测要起一个 shell（最多 3 秒），所以先 await 再首刷，免得白报一次"未配置"。
autofillConfig().finally(() => {
  restartTimer();
  refresh(true);
});
