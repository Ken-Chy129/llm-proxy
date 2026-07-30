# LLM Proxy 桌面小工具

macOS 菜单栏常驻 + 弹出面板 + 桌面悬浮挂件，一眼看到账号额度和今日用量。

```
┌─ 菜单栏 ────────── 🔵 73% ──┐   ← 常年显示最紧张的额度，不用点开
└─────────────────────────────┘
     ↓ 左键点击弹出面板
┌─────────────────────────┐
│ 今日用量  2.16M tokens  │  ← 今日/昨日/7日均对比 + 24h 柱状图
│ 账号额度  3 个账号进度条 │  ← 5h 会话 + 周额度，低于阈值变黄/红
│ Key 分摊  按 token 排序 │  ← 哪个 key 吃掉了额度
└─────────────────────────┘
```

## 三种形态

| 形态 | 怎么用 |
|---|---|
| **菜单栏文字** | 常驻显示最低剩余额度（如 `73%`），额度未知时显示今日 token 量 |
| **弹出面板** | 左键点菜单栏图标弹出，失焦自动收起，Esc 关闭 |
| **悬浮挂件** | 面板里点 `▣`，或托盘右键菜单 →「桌面悬浮挂件」。无边框置顶小窗，可拖动、可取消置顶 |

额度跌破阈值或账号被限流时发系统通知（同一档位只提醒一次，不会反复打扰）。

## 构建

需要 macOS + Xcode 命令行工具 + Rust 工具链：

```bash
# 装 Rust（如果还没有）
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh

cd desktop
npm install
npm run tauri dev     # 开发模式，热重载
npm run tauri build   # 出 .app 和 .dmg（在 src-tauri/target/release/bundle/）
```

构建产物没有签名，首次打开会被 Gatekeeper 拦。右键 →「打开」，或者：

```bash
xattr -dr com.apple.quarantine "/Applications/LLM Proxy.app"
```

## 首次配置

启动后面板会自动打开设置页，填三项：

- **代理地址** — 你的 llm-proxy 地址，例如 `https://proxy.example.com`（不要带尾斜杠，带了也会自动去掉）
- **API Key** — 在 llm-proxy Dashboard → Keys 页面创建或复制一个（`sk-...`）
- **刷新间隔 / 告警阈值** — 默认 60 秒 / 20%

配置存在 WebView 的 localStorage 里，只在本机与你的代理之间传输。

> **为什么用 API Key 而不是登录密码？**
> llm-proxy 的 dashboard session 存在内存里，服务一重启（比如你 `./deploy.sh`）所有 cookie 就失效了。托盘每分钟轮询一次，靠 cookie 会动不动掉线；API Key 则能一直用。

## 后端接口

依赖 llm-proxy 的 `GET /api/tray?tz=<分钟偏移>`，认 `Authorization: Bearer <key>` 或 dashboard session cookie。

响应是刻意压小的（托盘要每分钟拉一次），只含：

```jsonc
{
  "min_session_percent": 73,        // 所有可用账号里最紧张的 5h 剩余额度
  "min_weekly_percent": 90,
  "accounts": [{                    // 每个账号的额度、限流状态、重置时间
    "email": "...", "plan_type": "Max 5x", "status": "active",
    "session_percent": 73, "weekly_percent": 91,
    "weekly_reset_at": "08/01 22:00", "has_real_data": true
  }],
  "today":     { "date": "2026-07-30", "request_count": 68,
                 "total_tokens": 2159238, "by_key": [...] },
  "yesterday": { ... },             // 同口径的昨日，用于对比
  "avg_7d_requests": 450.9,         // 不含今天，避免半天数据拉低基线
  "avg_7d_tokens": 3990464.3,
  "hourly_tokens": [0, 0, ...],     // 24 个槽位，本地时区
  "backends_active": 2, "backends_total": 4,
  "idle_seconds": 27
}
```

**`tz` 必须传**（浏览器 `-new Date().getTimezoneOffset()`）。后端用它算日历天；不传就按 UTC 算，对 UTC+8 会错 8 小时。

注意这里的「今日」是**日历天**，跟 dashboard 的 `/api/stats?range=today`（滚动 24 小时窗口）口径不同 —— 后者会把昨天傍晚的流量算进"今天"。

### CORS

打包后的 Tauri 应用运行在 `tauri://localhost`（macOS/Linux）或 `https://tauri.localhost`（Windows），因此访问远程代理属于跨源请求，浏览器会要求 CORS 头。`/api/tray` 挂了 `DesktopCORS()` 中间件放行这几个来源。

两点刻意的设计：

- **白名单而非回显 Origin。** 回显任意来源等于让任何网站都能读你的用量数据。
- **绝不发 `Allow-Credentials`。** 挂件用 Bearer key 认证，不需要 cookie；一旦允许携带凭据，白名单里的来源就能盗用你已登录的 dashboard session。

预检（OPTIONS）在鉴权之前响应 —— 预检请求不带 `Authorization` 头，鉴权在前会导致永远 401。这几条行为都有测试锁住（`internal/server/cors_test.go`），改动前先看测试。

如果你的代理挂在反向代理后面，确认它没有吞掉或覆盖这些响应头（Caddy 默认不会）。

## 目录结构

```
desktop/
├── src/                    前端（无构建步骤，原生 ES module）
│   ├── index.html
│   ├── style.css           暗色为主，跟随系统亮暗
│   ├── render.js           纯渲染函数：JSON → DOM，不依赖 Tauri
│   └── app.js              轮询、配置、Tauri 桥接、告警
├── src-tauri/
│   ├── src/lib.rs          托盘图标、窗口显隐、悬浮模式（刻意保持极薄）
│   ├── tauri.conf.json
│   └── capabilities/       权限白名单
└── scripts/
    ├── preview.mjs         离线预览：真实 JSON → 静态 HTML，可截图验证
    └── render.test.mjs     渲染层单元测试
```

**为什么 render.js 和 app.js 分开？** `render.js` 是纯函数，不碰 fetch 也不碰 Tauri，所以能在任何浏览器里用捕获的真实数据离线渲染 —— 改样式不用等 Rust 编译：

```bash
# 抓一份真实数据
curl -H "Authorization: Bearer sk-..." \
     "https://proxy.example.com/api/tray?tz=480" -o /tmp/tray.json

node scripts/preview.mjs /tmp/tray.json /tmp/pv.html --dark --hour 11
node scripts/preview.mjs /tmp/tray.json /tmp/pv.html --dark --float  # 悬浮挂件样式
open /tmp/pv.html
```

`--dark` 强制暗色（无头 Chrome 默认报告 light），`--float` 用悬浮挂件样式，`--hour N` 指定"当前小时"以便测试标记位置。

## 测试

```bash
node --test scripts/                  # 渲染层：数字格式化、额度分档、刻度防重叠
cd .. && go test ./internal/stats/    # 后端：日历天口径、时区边界、7日均值
cd .. && go test ./internal/server/   # CORS：来源白名单、拒绝凭据、预检绕过鉴权
```

后端测试里有一条专门锁住「日历天 ≠ 滚动 24 小时」这个差异（`TestDayUsageIsCalendarDayNotRolling24h`），别删。

## 已知限制

- **不是 macOS 原生 Widget。** 桌面小组件（通知中心那种）必须用 Swift + WidgetKit，Tauri 做不到。这里的"悬浮挂件"是个无边框置顶窗口，视觉接近但本质是窗口。
- **16px 图标下指针退化成一个点。** 16px 只出现在少数系统 UI 位置；菜单栏用的是独立的 `trayTemplate.png`（单色模板图），不受影响。
- 需要能访问到代理地址。走公网就配好 HTTPS 反代（Caddy/Nginx 均可）；只在内网用则填 `http://<内网IP>:9090`。
