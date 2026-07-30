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

const PANEL: &str = "panel";
const FLOAT: &str = "float";

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
fn show_panel_near_tray(win: &WebviewWindow) {
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
            hide_panel
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
                                let _ = tauri_plugin_positioner::move_window(
                                    &w,
                                    tauri_plugin_positioner::Position::TrayCenter,
                                );
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
            .inner_size(320.0, 520.0)
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
