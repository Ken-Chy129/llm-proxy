// 应用层：配置持久化、轮询、Tauri 托盘/通知桥接。
// 渲染逻辑全在 render.js 里（纯函数，可离线预览）。

import { render, trayTitle } from './render.js';

const DEFAULTS = {
  base: '',
  key: '',
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

let config = loadConfig();
let timer = null;
let lastAlertKey = ''; // 同一个额度窗口只提醒一次，避免每轮轮询都弹通知

// 悬浮模式：窗口自己就是挂件，不需要"打开悬浮窗"按钮，而是需要置顶开关。
const IS_FLOAT = new URLSearchParams(location.search).get('mode') === 'float';
let floatOnTop = true;

/** Tauri API 在浏览器预览里不存在，全部调用都要软失败。 */
async function tauri(fn) {
  try {
    if (!window.__TAURI__) return null;
    return await fn(window.__TAURI__);
  } catch (e) {
    console.warn('tauri call failed', e);
    return null;
  }
}

function setDot(state) {
  const dot = document.getElementById('live-dot');
  dot.className = `dot ${state}`;
}

function showError(msg) {
  const box = document.getElementById('error');
  if (!msg) {
    box.classList.add('hidden');
    return;
  }
  box.textContent = msg;
  box.classList.remove('hidden');
}

/** 额度跌破阈值时发系统通知；恢复后重置，以便下次再触发。 */
async function maybeNotify(d) {
  if (!config.notify) return;
  const worst = (d.accounts || [])
    .filter((a) => a.has_real_data && a.status !== 'disabled' && a.session_percent !== null && a.session_percent !== undefined)
    .sort((a, b) => a.session_percent - b.session_percent)[0];
  if (!worst) return;

  const limited = (d.accounts || []).filter((a) => a.status === 'rate_limited');
  // 触发键包含账号和整档百分比，这样跌得更低时会再提醒一次，
  // 但同一档位内反复轮询不会重复打扰。
  const bucket = Math.floor(worst.session_percent / 5) * 5;
  const alertKey = limited.length
    ? `limited:${limited.map((a) => a.email).join(',')}`
    : worst.session_percent <= config.threshold
      ? `low:${worst.email}:${bucket}`
      : '';

  if (!alertKey || alertKey === lastAlertKey) {
    if (!alertKey) lastAlertKey = '';
    return;
  }
  lastAlertKey = alertKey;

  const title = limited.length ? 'LLM Proxy 账号被限流' : 'LLM Proxy 额度偏低';
  const body = limited.length
    ? `${limited.map((a) => `${a.email.split('@')[0]} 至 ${a.rate_limited_until || '?'}`).join('；')}`
    : `${worst.email.split('@')[0]} 会话额度仅剩 ${Math.round(worst.session_percent)}%`;

  await tauri(async (T) => {
    let granted = await T.notification.isPermissionGranted();
    if (!granted) granted = (await T.notification.requestPermission()) === 'granted';
    if (granted) T.notification.sendNotification({ title, body });
  });
}

async function refresh(manual = false) {
  if (!config.base || !config.key) {
    showError('还没配置代理地址和 API Key，点右上角 ⚙ 设置。');
    setDot('dead');
    openSettings();
    return;
  }

  const btn = document.getElementById('btn-refresh');
  if (manual) btn.classList.add('spin');

  // 后端按这个偏移算日历天，必须传浏览器本地时区，否则“今日”会错几小时。
  const tz = -new Date().getTimezoneOffset();
  const url = `${config.base.replace(/\/+$/, '')}/api/tray?tz=${tz}`;

  try {
    const res = await fetch(url, {
      headers: { Authorization: `Bearer ${config.key}` },
      cache: 'no-store',
    });
    if (res.status === 401 || res.status === 403) {
      throw new Error('API Key 无效或已停用（401/403）');
    }
    if (!res.ok) {
      throw new Error(`代理返回 ${res.status}`);
    }
    const data = await res.json();

    showError('');
    render(data);
    setDot('live');

    const title = trayTitle(data);
    await tauri((T) => T.invoke('set_tray_title', { title }));
    await maybeNotify(data);
  } catch (e) {
    setDot('dead');
    showError(`刷新失败：${e.message}`);
  } finally {
    if (manual) setTimeout(() => btn.classList.remove('spin'), 400);
  }
}

function restartTimer() {
  if (timer) clearInterval(timer);
  const sec = Math.max(10, Number(config.interval) || 60);
  timer = setInterval(() => refresh(false), sec * 1000);
}

function openSettings() {
  document.getElementById('in-base').value = config.base;
  document.getElementById('in-key').value = config.key;
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
  // 面板模式和悬浮模式的按钮职责不同：面板上的 ⧉ 用来召出挂件，
  // 挂件上则换成置顶开关（它本身就已经"浮"着了）。
  const btnFloat = document.getElementById('btn-float');
  const btnPin = document.getElementById('btn-pin');
  if (IS_FLOAT) {
    btnFloat.classList.add('hidden');
    btnPin.addEventListener('click', () => {
      floatOnTop = !floatOnTop;
      btnPin.style.opacity = floatOnTop ? '1' : '0.4';
      btnPin.title = floatOnTop ? '取消置顶' : '保持置顶';
      tauri((T) => T.invoke('set_float_on_top', { onTop: floatOnTop }));
    });
  } else {
    btnPin.classList.add('hidden');
    btnFloat.addEventListener('click', () => {
      tauri((T) => T.invoke('toggle_float'));
    });
  }

  document.getElementById('btn-refresh').addEventListener('click', () => refresh(true));
  document.getElementById('btn-settings').addEventListener('click', openSettings);
  document.getElementById('btn-cancel').addEventListener('click', closeSettings);
  document.getElementById('btn-save').addEventListener('click', () => {
    config = {
      base: document.getElementById('in-base').value.trim(),
      key: document.getElementById('in-key').value.trim(),
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
    if (e.key === 'Escape' && !IS_FLOAT) tauri((T) => T.invoke('hide_panel'));
  });

  // 面板每次弹出时立刻刷新一次，别让用户看着过期数字。
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refresh(false);
  });

  // 供托盘菜单“立即刷新”调用
  window.__refresh = () => refresh(true);
}

wire();
if (IS_FLOAT) document.body.classList.add('float');
restartTimer();
refresh(true);
