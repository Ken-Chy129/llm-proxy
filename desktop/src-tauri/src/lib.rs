// LLM Proxy 桌面小工具
//
// Rust 侧刻意保持极薄：只负责托盘图标、窗口显隐、悬浮模式开关。
// 所有数据获取、渲染、告警判断都在前端（src/*.js），因为那部分能在
// 任意浏览器里离线验证，而 Rust 每改一行都要重新编译。

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::sync::Mutex;

use tauri::{
    menu::{Menu, MenuItem},
    tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent},
    AppHandle, Manager, WebviewWindow, WebviewWindowBuilder, WebviewUrl,
};
// 定位能力来自这个 trait；插件里的 `move_window` 是私有 command，只给 JS 用。
use tauri_plugin_positioner::{Position, WindowExt};

const PANEL: &str = "panel";
const FLOAT: &str = "float";

/// 前缀哨兵：shell 的 rc 文件里可能有 neofetch / fortune 之类往 stdout 打东西的
/// 东西，所以不能假定 stdout 只有我们的输出，得把自己那行捞出来。
const SENTINEL: &str = "__LLM_PROXY_TRAY__";

#[derive(serde::Serialize)]
struct DetectedConfig {
    base: String,
    token: String,
}

/// 从用户的登录 shell 环境里探测代理地址和 tray token，省掉手填。
///
/// 为什么必须真起一个 shell：从 Finder / Dock 启动的 GUI 应用**不继承任何 shell
/// 环境**，`std::env::var` 拿到的永远是空——export 写在 rc 文件里，只有跑一遍
/// rc 才看得见。`-i` 让 zsh/bash 读 ~/.zshrc（export 通常在那儿），`-l` 让它读
/// ~/.zprofile，两个都给才能覆盖两种写法。
///
/// stdin 接 null 是防挂的关键：rc 里若有 `read` 之类，拿到 EOF 会立刻返回而不是
/// 无限等；再加超时兜底。超时后子进程可能短暂残留，但 stdin 已关，它自己会退。
///
/// 超时给 5 秒：实测这台机器上 `zsh -ilc true` 约 1.0–1.5 秒（nvm/conda 那类
/// 初始化会更慢），留 3 倍余量。反正只在配置为空时跑一次。
///
/// 刻意是同步命令而非 `async fn`：这里会阻塞等子进程，而 async 命令跑在 tokio
/// runtime 上，阻塞它会连带卡住其它 IPC；同步命令 Tauri 会丢到线程池执行。
#[tauri::command]
fn detect_env_config() -> Option<DetectedConfig> {
    use std::process::{Command, Stdio};

    let shell = std::env::var("SHELL").unwrap_or_else(|_| "/bin/zsh".to_string());
    // LLM_PROXY_BASE_URL 优先，退回 ANTHROPIC_BASE_URL——后者本来就指向这个代理，
    // 是绝大多数人已经配好的那一个。
    let script = format!(
        r#"printf '{SENTINEL}%s\t%s\n' "${{LLM_PROXY_BASE_URL:-$ANTHROPIC_BASE_URL}}" "$LLM_PROXY_TRAY_TOKEN""#
    );

    let child = Command::new(&shell)
        .args(["-ilc", &script])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null()) // 交互式 shell 常在无 tty 时抱怨作业控制，与我们无关
        .spawn()
        .ok()?;

    // wait_with_output 在独立线程里跑：它会边等边读管道，避免 rc 输出撑满管道缓冲
    // 区导致死锁；主线程只按超时取结果。
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(child.wait_with_output());
    });
    let out = rx
        .recv_timeout(std::time::Duration::from_secs(5))
        .ok()?
        .ok()?;

    let stdout = String::from_utf8_lossy(&out.stdout);
    let line = stdout.lines().find_map(|l| l.strip_prefix(SENTINEL))?;
    let mut parts = line.splitn(2, '\t');
    let base = parts.next().unwrap_or("").trim().to_string();
    let token = parts.next().unwrap_or("").trim().to_string();
    if base.is_empty() && token.is_empty() {
        return None;
    }
    Some(DetectedConfig { base, token })
}

/// 前端每次轮询后回调，把最紧张的额度写到菜单栏文字上。
#[tauri::command]
fn set_tray_title(app: AppHandle, title: String) {
    if let Some(tray) = app.tray_by_id("main") {
        let _ = tray.set_title(Some(&title));
    }
}

/// 悬浮球距屏幕右下角的内缩量。完全贴边会压在 Dock 的圆角上、也不好抓住拖动。
const FLOAT_MARGIN: f64 = 16.0;

/// 创建悬浮球窗口（不显示）。启动时和首次 toggle 都走这里，避免两处各写一份
/// builder ——那些参数每一条都是撞出来的，抄两遍必然会漂。
fn build_float(app: &AppHandle) -> Result<WebviewWindow, String> {
    WebviewWindowBuilder::new(app, FLOAT, WebviewUrl::App("index.html?mode=float".into()))
        .title("LLM Proxy")
        // 折叠态就是一个 56px 的球，窗口与内容严格等大。展开由前端 hover 时调
        // expand_float 撑开。
        //
        // 不留"阴影余量"：阴影由 macOS 画在窗口**外面**，留了边距反而制造一圈看不
        // 见、却会拦截桌面点击的死区。
        .inner_size(BALL, BALL)
        .resizable(false) // 尺寸由展开/折叠状态决定，手动拉扯只会拉出一片透明
        .decorations(false)      // 无边框，才像挂件而不是浏览器
        .always_on_top(true)
        .skip_taskbar(true)
        .transparent(true)
        // 先不显示：位置要在 show 之前设好，否则会先在系统给的默认位置闪一下再跳走
        .visible(false)
        // 阴影必须交给 macOS：透明窗口的原生阴影是按内容 alpha 形状生成的（面板的
        // 圆角阴影就是这么来的），而 CSS box-shadow 会被窗口边界裁成一条硬边灰带
        // ——窗口与内容等大，22px 的模糊根本画不下。实测过：4px 边距里的阴影压在
        // 白底文档上就是一块灰方块。
        .shadow(true)
        .build()
        .map_err(|e| e.to_string())
}

/// 把球摆到主屏右下角。
///
/// 用 work_area 而不是 monitor.size：前者已经排除菜单栏和 Dock，否则球会被 Dock
/// 压在下面。用 primary_monitor 而不是 current_monitor：窗口还没显示时后者不保证
/// 返回有意义的屏幕。
fn place_float_bottom_right(w: &WebviewWindow) {
    let Ok(scale) = w.scale_factor() else { return };
    let mon = w
        .primary_monitor()
        .ok()
        .flatten()
        .or_else(|| w.current_monitor().ok().flatten());
    let Some(mon) = mon else { return };

    let area = mon.work_area();
    let pos = area.position.to_logical::<f64>(scale);
    let size = area.size.to_logical::<f64>(scale);
    let x = pos.x + size.width - BALL - FLOAT_MARGIN;
    let y = pos.y + size.height - BALL - FLOAT_MARGIN;
    let _ = w.set_position(tauri::LogicalPosition::new(x, y));
}

/// 切换桌面悬浮挂件。窗口通常已在启动时建好，这里只管显隐；
/// 万一没有（创建失败过）就补建一次。
#[tauri::command]
fn toggle_float(app: AppHandle) -> Result<bool, String> {
    if let Some(w) = app.get_webview_window(FLOAT) {
        if w.is_visible().unwrap_or(false) {
            w.hide().map_err(|e| e.to_string())?;
            return Ok(false);
        }
        w.show().map_err(|e| e.to_string())?;
        return Ok(true);
    }

    let w = build_float(&app)?;
    place_float_bottom_right(&w);
    let _ = w.show();
    Ok(true)
}

/// 球折叠态的边长；展开/收回都以这个为锚。
const BALL: f64 = 56.0;

/// 球在展开态窗口里的偏移（dx, dy）。收回时按它把窗口挪回球的位置，否则球会跳到
/// 卡片原来占的那个角。
///
/// 存偏移而不是绝对坐标：展开状态下卡片头部还能拖动，绝对坐标会立刻过期。
static FLOAT_SHIFT: Mutex<(f64, f64)> = Mutex::new((0.0, 0.0));

/// 展开后球在窗口的哪个角。前端据此把卡片摆到球的另一侧。
#[derive(serde::Serialize)]
struct ExpandLayout {
    /// true = 球贴窗口底边（卡片在球**上方**），false = 球贴顶边（卡片在下方）
    ball_bottom: bool,
    /// true = 球贴窗口左边（卡片向右铺开），false = 球贴右边
    ball_left: bool,
}

/// hover 展开：撑开窗口，但**球本身一动不动**。
///
/// 窗口此时同时装着球和卡片，所以要算的是"把球留在原处的话，窗口该挪到哪"。默认卡片
/// 浮在球的上方、与球左对齐；上方放不下就翻到下面，右边放不下就靠右对齐。
///
/// 用 work_area 而不是 monitor.size：前者排除了菜单栏和 Dock，否则卡片会钻到 Dock
/// 底下或被菜单栏盖住。
///
/// 走 Rust 而不是前端调 `window.setSize`：后者属于 core 插件命令，要在 capabilities
/// 里显式放行 `core:window:allow-set-size`，少一处能静默失效的配置。
#[tauri::command]
fn expand_float(app: AppHandle, width: f64, height: f64) -> ExpandLayout {
    let mut layout = ExpandLayout {
        ball_bottom: true,
        ball_left: true,
    };
    let Some(w) = app.get_webview_window(FLOAT) else {
        return layout;
    };
    let scale = w.scale_factor().unwrap_or(1.0);
    let Ok(outer) = w.outer_position() else {
        return layout;
    };
    // 球当前的屏幕位置——展开前后它必须落在同一个像素上
    let ball = outer.to_logical::<f64>(scale);

    if let Ok(Some(mon)) = w.current_monitor() {
        let area = mon.work_area();
        let a_pos = area.position.to_logical::<f64>(scale);
        let a_size = area.size.to_logical::<f64>(scale);
        // 卡片往上放需要 height - BALL 的空间；不够就翻到球下面
        layout.ball_bottom = ball.y - (height - BALL) >= a_pos.y;
        // 卡片向右铺开需要 width 的宽度；不够就靠右对齐（卡片向左铺）
        layout.ball_left = ball.x + width <= a_pos.x + a_size.width;
    }

    let dx = if layout.ball_left { 0.0 } else { width - BALL };
    let dy = if layout.ball_bottom { height - BALL } else { 0.0 };
    // 收回时把窗口挪回球的位置
    *FLOAT_SHIFT.lock().unwrap() = (dx, dy);
    let _ = w.set_size(tauri::LogicalSize::new(width, height));
    let _ = w.set_position(tauri::LogicalPosition::new(ball.x - dx, ball.y - dy));
    layout
}

/// 开始拖拽悬浮球：先收回成球，再交给系统拖窗口。
///
/// 为什么不用 `data-tauri-drag-region`：那个属性一 mousedown 就让系统接管窗口，而我
/// 们必须**先**把窗口缩回 56x56 再开始拖。理由有两条，都是实际撞出来的：
///   - 228px 宽的展开态一碰到屏幕右边界就推不过去了，表现为"球贴不到最右边"；
///   - 拖拽期间窗口一直在动，hover 事件被反复触发，展开逻辑和系统拖拽抢同一个窗口，
///     手感一顿一顿的。
/// 把"收回 + 开始拖"合成一条命令，顺序就由 Rust 保证，不会被 IPC 往返切开。
#[tauri::command]
fn start_float_drag(app: AppHandle) {
    collapse_float(app.clone());
    if let Some(w) = app.get_webview_window(FLOAT) {
        let _ = w.start_dragging();
    }
}

/// 鼠标此刻是否落在悬浮窗的矩形内。
///
/// 展开态需要这个看门狗：mouseleave 在 macOS 上不保证送达——窗口刚被 setSize /
/// setPosition 挪过之后尤其容易漏，而漏一次的后果是挂件永久摊开着，鼠标移开也不收。
/// 每隔几百毫秒主动问一次，比信任事件可靠。
#[tauri::command]
fn cursor_over_float(app: AppHandle) -> bool {
    let Some(w) = app.get_webview_window(FLOAT) else {
        return false;
    };
    let (Ok(cursor), Ok(pos), Ok(size)) = (app.cursor_position(), w.outer_position(), w.outer_size())
    else {
        // 查不到就报"在里面"，宁可少收一次也不要在鼠标还悬着的时候把卡片抽走
        return true;
    };
    cursor.x >= pos.x as f64
        && cursor.x <= (pos.x + size.width as i32) as f64
        && cursor.y >= pos.y as f64
        && cursor.y <= (pos.y + size.height as i32) as f64
}

/// 收回成球，并把它放回展开前那个角。
#[tauri::command]
fn collapse_float(app: AppHandle) {
    let Some(w) = app.get_webview_window(FLOAT) else {
        return;
    };
    let (dx, dy) = std::mem::replace(&mut *FLOAT_SHIFT.lock().unwrap(), (0.0, 0.0));
    let scale = w.scale_factor().unwrap_or(1.0);
    let _ = w.set_size(tauri::LogicalSize::new(BALL, BALL));
    if let Ok(outer) = w.outer_position() {
        let pos = outer.to_logical::<f64>(scale);
        let _ = w.set_position(tauri::LogicalPosition::new(pos.x + dx, pos.y + dy));
    }
}

#[tauri::command]
fn hide_panel(app: AppHandle) {
    if let Some(w) = app.get_webview_window(PANEL) {
        let _ = w.hide();
    }
}

/// 点击托盘图标：把面板定位到图标下方再显示，模拟原生菜单栏弹窗。
///
/// `move_window_constrained` 在图标靠屏幕边缘时会把窗口拉回屏内；托盘位置
/// 尚未被记录时（用户从未碰过图标）会返回 Err，此时保持窗口原位直接显示。
fn show_panel_near_tray(win: &WebviewWindow) {
    let _ = win.move_window_constrained(Position::TrayBottomCenter);
    let _ = win.show();
    let _ = win.set_focus();
}

pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_positioner::init())
        .invoke_handler(tauri::generate_handler![
            set_tray_title,
            toggle_float,
            expand_float,
            collapse_float,
            cursor_over_float,
            start_float_drag,
            hide_panel,
            detect_env_config
        ])
        .setup(|app| {
            let handle = app.handle().clone();

            let show_i = MenuItem::with_id(app, "show", "打开面板", true, None::<&str>)?;
            let float_i = MenuItem::with_id(app, "float", "桌面悬浮挂件", true, None::<&str>)?;
            let refresh_i = MenuItem::with_id(app, "refresh", "立即刷新", true, None::<&str>)?;
            let quit_i = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&show_i, &float_i, &refresh_i, &quit_i])?;

            TrayIconBuilder::with_id("main")
                .icon(app.default_window_icon().unwrap().clone())
                .icon_as_template(true) // macOS 菜单栏跟随亮/暗色
                .menu(&menu)
                .show_menu_on_left_click(false) // 左键弹面板，右键才出菜单
                .on_menu_event(move |app, event| match event.id.as_ref() {
                    "show" => {
                        if let Some(w) = app.get_webview_window(PANEL) {
                            show_panel_near_tray(&w);
                        }
                    }
                    "float" => {
                        let _ = toggle_float(app.clone());
                    }
                    "refresh" => {
                        if let Some(w) = app.get_webview_window(PANEL) {
                            let _ = w.eval("window.__refresh && window.__refresh()");
                        }
                    }
                    "quit" => app.exit(0),
                    _ => {}
                })
                .on_tray_icon_event(move |tray, event| {
                    tauri_plugin_positioner::on_tray_event(tray.app_handle(), &event);
                    if let TrayIconEvent::Click {
                        button: MouseButton::Left,
                        button_state: MouseButtonState::Up,
                        ..
                    } = event
                    {
                        if let Some(w) = tray.app_handle().get_webview_window(PANEL) {
                            if w.is_visible().unwrap_or(false) {
                                let _ = w.hide();
                            } else {
                                show_panel_near_tray(&w);
                            }
                        }
                    }
                })
                .build(app)?;

            // 面板窗口：无边框、不进任务栏，失焦自动隐藏（原生弹窗手感）。
            let panel = WebviewWindowBuilder::new(
                &handle,
                PANEL,
                WebviewUrl::App("index.html".into()),
            )
            .title("LLM Proxy")
            // 600 而非 520：三个账号的额度卡就已经把 520 撑破，弹窗一开就得滚。
            // 600 在最小的 MacBook 逻辑分辨率（~1512x982）上也放得下菜单栏弹窗。
            // 再高的内容交给 #panel 自己滚（见 style.css 的「高度与滚动」）。
            .inner_size(320.0, 600.0)
            .resizable(false)
            .decorations(false)
            .always_on_top(true)
            .skip_taskbar(true)
            .transparent(true)
            .visible(false)
            .build()?;

            let p = panel.clone();
            panel.on_window_event(move |event| {
                if let tauri::WindowEvent::Focused(false) = event {
                    // 悬浮挂件独立存在，面板失焦只隐藏面板自己。
                    let _ = p.hide();
                }
            });

            // macOS: 不在 Dock 显示图标，纯菜单栏应用。
            #[cfg(target_os = "macos")]
            let _ = app.set_activation_policy(tauri::ActivationPolicy::Accessory);

            // 悬浮球开机即在，落在主屏右下角。
            //
            // 位置必须在 show 之前设好：先显示再挪会先在系统给的默认位置（通常屏幕
            // 正中）闪一下。之后用户拖到哪儿就是哪儿，这里只管首次落点。
            match build_float(&handle) {
                Ok(ball) => {
                    place_float_bottom_right(&ball);
                    let _ = ball.show();
                }
                // 建不出来不该拖垮整个应用：菜单栏图标和面板仍然可用，
                // 菜单里的「悬浮挂件」还能再试一次。
                Err(e) => eprintln!("float window init failed: {e}"),
            }

            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("error building tauri app")
        .run(|_app, event| {
            // 关掉所有窗口不等于退出——菜单栏图标还在。
            if let tauri::RunEvent::ExitRequested { api, .. } = event {
                api.prevent_exit();
            }
        });
}
