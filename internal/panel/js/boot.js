/* boot.js — 启动引导。必须最后加载。
 *
 * 为什么单独成文件：拆分前 app.js 是单个文件，顶层 `start()` 调用时所有函数都已
 * 定义好。拆成多文件后，core.js 里的启动调用会早于 accounts.js / usage.js 等加载，
 * 触发 ReferenceError（`loadOverview is not defined`）或 TDZ。
 *
 * 这里集中放三类东西：
 *   1) 依赖其他模块函数的顶层事件绑定（btnAdd → openAdd）；
 *   2) 首屏路由跳转（go() 会同步触发各视图的数据加载）；
 *   3) start() 及其变体。
 *
 * 加载顺序见 index.html：core → accounts → models → logs → usage → packages
 * → tasks → config → boot → mobile.js
 */

/* ── 顶部动作 ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;

/* ── 导航与首屏路由 ───────────────────────────────────────────────── */
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => {
  e.preventDefault();
  go(a.dataset.view);
  history.replaceState(null, '', '#' + a.dataset.view);
});
/* 首次进入延到本轮脚本求值之后再 go()。
   原因：go() 会同步触发视图的数据加载（loadUsage/loadLogs/loadPackages…），而这些
   函数读到的模块级 let/const（usageRateWarmAt、PK_* 等）依赖各自文件已求值完毕。
   直接深链 #usage / #packages 打开页面时会踩 TDZ（"Cannot access 'x' before
   initialization"），表现为该页永远显示"读取失败"。
   延迟 0ms 让全部脚本先求值完，是修这一类问题最省事也最不容易再犯的办法。 */
setTimeout(() => {
  const hash = (location.hash || '#accounts').slice(1);
  go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── 启动 ─────────────────────────────────────────────────────────── */
start();
