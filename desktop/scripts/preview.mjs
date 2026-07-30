// 离线预览：把捕获的真实 /api/tray 响应喂给 render.js，产出一个静态 HTML。
// 用途是在没有 Rust 工具链的机器上验证渲染逻辑和视觉效果——渲染层是纯函数，
// 所以这里看到的就是托盘里会看到的。
//
// 用法: node scripts/preview.mjs <tray.json> <out.html> [--float] [--hour N]

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SRC = resolve(__dirname, '../src');

const [dataPath, outPath] = process.argv.slice(2);
if (!dataPath || !outPath) {
  console.error('usage: node scripts/preview.mjs <tray.json> <out.html> [--float] [--hour N]');
  process.exit(1);
}
const isFloat = process.argv.includes('--float');
const isDark = process.argv.includes('--dark');
const hourArg = process.argv.indexOf('--hour');
const nowHour = hourArg > -1 ? Number(process.argv[hourArg + 1]) : new Date().getHours();
// 告警阈值，跟 app.js 的默认值保持一致；只影响预览里横幅是否出现
const thArg = process.argv.indexOf('--threshold');
const threshold = thArg > -1 ? Number(process.argv[thArg + 1]) : 20;

const data = JSON.parse(readFileSync(dataPath, 'utf8'));
const html = readFileSync(`${SRC}/index.html`, 'utf8');
let css = readFileSync(`${SRC}/style.css`, 'utf8');
const renderJs = readFileSync(`${SRC}/render.js`, 'utf8');

// 样式默认是暗色，浅色靠 prefers-color-scheme 媒体查询覆盖。无头 Chrome 默认
// 报告 light，所以要预览暗色就把那段媒体查询摘掉。
if (isDark) {
  css = css.replace(/@media \(prefers-color-scheme: light\) \{[\s\S]*?\n\}\n/, '');
}

// 取出 <div id="app"> ... </div>，丢掉外壳和真实 app.js（它会去 fetch）
const bodyMatch = html.match(/<body>([\s\S]*?)<script/);
if (!bodyMatch) throw new Error('无法从 index.html 提取 body');
const body = bodyMatch[1];

// render.js 是 ES module，把 export 关键字去掉就能内联进 <script>
const inlineRender = renderJs.replace(/^export /gm, '');

const out = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="UTF-8"/>
<title>预览</title>
<style>
${css}
/* 预览专用：给截图一点留白，并去掉窗口透明假设 */
body { margin: 0; padding: 14px; background: #0e1014; }
</style>
</head><body class="${isFloat ? 'float' : ''}">
${body}
<script>
${inlineRender}
const DATA = ${JSON.stringify(data)};
document.getElementById('btn-pin').classList.add('hidden');
render(DATA, { nowHour: ${nowHour} });
document.getElementById('live-dot').className = 'dot live';
// 复现告警横幅：预览里没有 Tauri，走 app.js 的降级路径。判定逻辑用
// render.js 导出的 alertFor，跟实际运行时是同一份代码。
(() => {
  const a = alertFor(DATA, ${threshold});
  const box = document.getElementById('alert');
  if (a && box) {
    // 与 app.js 一致：横幅只用 body，不拼 title（否则同义重复且会换行）
    box.textContent = '⚠ ' + a.body;
    box.classList.remove('hidden');
  }
})();
</script>
</body></html>`;

writeFileSync(outPath, out);
console.log(`wrote ${outPath} (${out.length} bytes)`);
