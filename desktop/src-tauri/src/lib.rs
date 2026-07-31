// LLM Proxy 桌面小工具
//
// Rust 侧刻意保持极薄：只负责托盘图标、窗口显隐、悬浮模式开关。
// 所有数据获取、渲染、告警判断都在前端（src/*.js），因为那部分能在
// 任意浏览器里离线验证，而 Rust 每改一行都要重新编译。

#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

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

/// 切换桌面悬浮挂件。首次调用时按需创建窗口——避免不用悬浮模式的人
/// 也常驻一个隐藏 webview。
#[tauri::command]
fn toggle_float(app: AppHandle) -> Result<bool, String> {
    if let Some(w) = app.get_webview_window(FLOAT) {
        let visible = w.is_visible().unwrap_or(false);
        if visible {
            w.hide().map_err(|e| e.to_string())?;
            return Ok(false);
        }
        w.show().map_err(|e| e.to_string())?;
        return Ok(true);
    }

    let w = WebviewWindowBuilder::new(&app, FLOAT, WebviewUrl::App("index.html?mode=float".into()))
        .title("LLM Proxy")
        .inner_size(320.0, 460.0)
        .resizable(true)
        .decorations(false)      // 无边框，才像挂件而不是浏览器
        .always_on_top(true)
        .skip_taskbar(true)
        .transparent(true)
        .shadow(true)
        .build()
        .map_err(|e| e.to_string())?;
    let _ = w.show();
    Ok(true)
}

/// 悬浮窗默认置顶；允许前端临时取消，方便用户临时让它退到后面。
#[tauri::command]
fn set_float_on_top(app: AppHandle, on_top: bool) {
    if let Some(w) = app.get_webview_window(FLOAT) {
        let _ = w.set_always_on_top(on_top);
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
            set_float_on_top,
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
