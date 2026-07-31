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

额度跌破阈值或账号被限流时发系统通知（同一档位只提醒一次，不会反复打扰）。通知插件不可用或未授权时，会降级为面板内的琥珀色横幅，**不会静默吞掉告警**。

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

先在服务端生成令牌：**Dashboard → Config → Admin → Tray token → Generate → Save Config**（也可以直接写 `config.yaml` 的 `server.tray_token`）。

然后二选一：

**A. 零手填（推荐）** — 把令牌 export 到 shell 配置里，挂件启动时自动读：

```bash
# ~/.zshrc
export LLM_PROXY_TRAY_TOKEN="tray-xxxx"
# 地址复用你已有的 ANTHROPIC_BASE_URL；想分开配就设 LLM_PROXY_BASE_URL
```

**B. 手填** — 面板右上角 ⚙，填代理地址 + Tray Token。

另外两项默认值够用：**刷新间隔 / 告警阈值** = 60 秒 / 20%。

配置存在 WebView 的 localStorage 里，只在本机与你的代理之间传输。手填过的值优先级高于环境变量 —— 自动探测只补空缺的字段，不会覆盖你的设置。

> **为什么是 Tray Token，而不是 API Key，也不是登录密码？**
>
> 三者对应三种权限，llm-proxy 刻意分开：
>
> | 凭证 | 平面 | 能做什么 |
> |---|---|---|
> | `sk-...`（Keys 页面） | 流量 | 打 `/v1/*`，**消耗你的额度**，受日限额约束 |
> | admin 用户名/密码 | 管理 | 登录 dashboard，改配置、删账号 |
> | `tray-...`（本项目） | 管理（只读） | 仅 `/api/tray`，看不到也改不了别的 |
>
> 挂件要的只是最后一种。用 API Key 会让一个每 60 秒发请求、明文存在 localStorage 里的桌面程序握着能花钱的钥匙；用登录密码则不行——dashboard session 存在内存里，服务一重启（`./deploy.sh`）就全失效，而挂件需要能跨重启存活的凭证。
>
> `/api/tray` 已不再接受 `sk-` API Key（`TrayAuth` 中间件），旧版挂件升级后会 401，重新配一次 tray token 即可。

## 悬浮挂件

托盘菜单 →「桌面悬浮挂件」召出一个常驻桌面的球（56px）。它只回答两个问题：

- **在下一次重置之前，我总共还能用多少** —— 球上的百分比和环
- **下一次回血是什么时候、能补多少** —— 球上第二行的倒计时

鼠标移上去，卡片浮在球的**上方**，球本身留在原地不动：

```
额度池                今日 535K
──────────────────────────────
5h  ▓▓▓▓▓▓▓░░  82%   18:10  +17%
7d  ▓▓▓▓▓░░░░  68%      1d  +14%
──────────────────────────────
     ╭──────╮
    │  82%   │   ← 球一直在，不会被卡片顶掉
    │ 1h14m  │
     ╰──────╯
```

- 每行右侧是**这个窗口最近一次重置**：当天报时刻（`18:10`，你会照它安排接下来做什么），
  跨天报倒计时（`1d`）。后面的蓝色数字是那次重置会给**池子**补回多少（悬停看账号级原始值）
- 球不消失是刻意的：整片替换会让人失去位置感，鼠标落点也会突然指向别的东西
- 拖拽球或卡片头部换位置；没有置顶开关——挂件常驻本来就该压在最上面，56px 挡不住东西
- 取不到数据时倒计时那行变成红色的「离线」——上面的百分比此刻是旧值，必须看得出来

### 为什么是"求和"而不是取最大或最小

各账号的额度窗口是**独立结算**的，所以"还能用多少"是各账号剩余百分比之**和**：三个
账号分别剩 100/98/48，5h 池就是 246%，约两个半账号的量。`246/300` 这个原始形式比
`82%` 更能说明这件事，所以两个都显示。

取最大只能回答"最宽裕的那个账号剩多少"，取最小是"最先干涸的那个"——都不是"我还能用
多少"。菜单栏标题仍用最小值，那是预警口径（`alertFor` 也用它），两者分工不同。

被停用的账号不计入（连 cap 也不占）；被限流的**要**计入——它此刻的余量本来就接近 0，
而它的重置时间正是"多久后回血"的答案。

### 为什么要显示补回多少

最早的那次重置常常毫无价值：曾经有一次 23 分钟后就重置，但那个 5h 窗口已经剩 98%，
等它只补 2%。只报时间会让人白等，所以那一行必须带上增量。

增量的口径是**对池子的贡献**，跟同一行的百分比一致：某个账号的 5h 从 48% 补满是 +52
个"账号百分点"，但池子是 240/300，它只让池子涨 52/300 ≈ 17 点。两个口径混在一行会
被读成 `80% + 52% = 132%`。账号级的原始数字放在悬停提示里。

用 accent 蓝而不是绿/黄/红：那套是"当前水位"，而这个数字是"等一等会多出来多少"，
染成状态色会被误读成水位本身。

### 球上为什么没有 tooltip

鼠标停在球上本来就会展开卡片，再弹一个系统 tooltip 是同一份信息说两遍，而且它会盖住
球下方的桌面。

### 展开方向

球多半被摆在屏幕右下角，卡片一律往右下长会整片跑出屏幕。`expand_float` 读 `work_area`
（排除菜单栏和 Dock）算出球该待在窗口的哪个角，前端把结果翻译成定位 class；收回时按
记下的偏移把窗口挪回球的位置，所以球在整个过程中一动不动。

### 悬浮态的两个坑

- **不要用 `position: absolute` 定位球和卡**。absolute 会去找最近的定位祖先，找不到就
  用初始包含块——实测那个块比窗口高，球被摆到窗口外面，看着像"没渲染"。用 `fixed`，它
  永远相对视口，而这里视口就是窗口。
- **`preview.mjs` 里不能靠 `--window-size` 模拟悬浮窗**。headless Chrome 有最小窗口
  高度，比它矮的窗口会把底部裁掉，球同样"消失"。预览会造一个与真实窗口等大的定位容器
  代替视口。

## 后端接口

依赖 llm-proxy 的 `GET /api/tray?tz=<分钟偏移>`，认 `Authorization: Bearer <tray_token>`（或 `x-api-key`），也接受 dashboard session cookie 以便在浏览器里调试。`server.tray_token` 为空时一律 401 —— 空值等于关闭，不等于放行。

响应是刻意压小的（托盘要每分钟拉一次），只含：

```jsonc
{
  "min_session_percent": 73,        // 所有可用账号里最紧张的 5h 剩余额度
  "min_weekly_percent": 90,
  "accounts": [{                    // 每个账号的额度、限流状态、重置时间
    "email": "...", "plan_type": "Max 5x", "status": "active",
    "session_percent": 73, "weekly_percent": 91,
    // 两个窗口的重置时间都是服务端本地时区的 "MM/DD HH:MM"；面板里当天只显示
    // HH:MM，跨天才带日期（见 render.js 的 shortReset）
    "session_reset_at": "07/31 16:00", "weekly_reset_at": "08/01 22:00",
    "has_real_data": true
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
- **绝不发 `Allow-Credentials`。** 挂件用 Bearer 令牌认证，不需要 cookie；一旦允许携带凭据，白名单里的来源就能盗用你已登录的 dashboard session。

预检（OPTIONS）在鉴权之前响应 —— 预检请求不带 `Authorization` 头，鉴权在前会导致永远 401。这几条行为都有测试锁住（`internal/server/cors_test.go`），改动前先看测试。

如果你的代理挂在反向代理后面，确认它没有吞掉或覆盖这些响应头（Caddy 默认不会）。

## 目录结构

```
desktop/
├── src/                    前端（无构建步骤，原生 ES module）
│   ├── index.html
│   ├── style.css           暗色为主，跟随系统亮暗
│   ├── render.js           纯函数：JSON → DOM、告警判定（alertFor），不依赖 Tauri
│   └── app.js              轮询、配置、Tauri 桥接、通知
├── src-tauri/
│   ├── src/lib.rs          托盘图标、窗口显隐、悬浮模式（刻意保持极薄）
│   ├── tauri.conf.json
│   └── capabilities/       权限白名单
└── scripts/
    ├── preview.mjs         离线预览：真实 JSON → 静态 HTML，可截图验证
    └── render.test.mjs     渲染层单元测试
```

### 改完样式怎么看效果

`preview.mjs` 出的是静态 HTML，配 headless Chrome 就能直接截图，不用编译 Rust、
也不用手动点托盘：

```bash
CH="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

# 面板
node scripts/preview.mjs /tmp/tray.json /tmp/p.html --dark
"$CH" --headless --disable-gpu --force-device-scale-factor=2 \
      --window-size=340,700 --screenshot=/tmp/p.png /tmp/p.html

# 悬浮挂件：--float 展开卡，加 --ball 只画折叠的球，--dot dead 模拟取数失败
node scripts/preview.mjs /tmp/tray.json /tmp/f.html --float --ball --dark --dot dead
"$CH" --headless --disable-gpu --force-device-scale-factor=3 \
      --window-size=110,110 --screenshot=/tmp/f.png /tmp/f.html
```

改 JSON 里的百分比就能造出低额度、限流、无数据这些平时抓不到的状态。CSS 这类改动
**必须真的看一眼**——比如环的分档色曾经被一条兜底规则的特异性压掉，红环画成灰的，
只有截图能发现。

**为什么 render.js 和 app.js 分开？** `render.js` 是纯函数，不碰 fetch 也不碰 Tauri，所以能在任何浏览器里用捕获的真实数据离线渲染 —— 改样式不用等 Rust 编译：

```bash
# 抓一份真实数据
curl -H "Authorization: Bearer tray-..." \
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
