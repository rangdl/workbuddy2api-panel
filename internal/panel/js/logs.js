/* ── 日志（频道：全部/任务/对话/系统） ─────────────────────────────── */
/* 日志分页模型：
   - 默认只拉最新 LOG_PAGE_SIZE 条（不再一次渲染 500 行，移动端尤其吃不住）；
   - 轮询只刷新「最新一页」，已往回翻的历史页保持不动；
   - 点「更早」用 next_cursor 游标往前翻，追加到列表顶部；
   - 频道与关键词筛选下推到服务端，避免「本地筛已拉取的几十条」导致漏结果。 */
const LOG_PAGE_SIZE = 50;
let logCh = 'all';
let logQ = '';
let logCursor = 0;        // 0 = 最新一页；非 0 = 正在看历史页
let logNextCursor = 0;    // 服务端返回的「更早」游标，0 = 没有更多
let logEntries = [];      // 当前已加载的日志（时间升序）
let logQTimer = null;

// logQuery 组装日志接口参数（筛选与分页都下推到服务端）。
function logQuery(pageLimit) {
  const p = new URLSearchParams();
  p.set('limit', String(pageLimit || LOG_PAGE_SIZE));
  if (logCh !== 'all') p.set('ch', logCh);
  if (logQ.trim()) p.set('q', logQ.trim());
  if (logCursor) p.set('cursor', String(logCursor));
  return p.toString();
}

// logRowHtml 单行日志。频道标签在「全部」视图下显示，单频道视图下省略。
function logRowHtml(e) {
  const lvl = /error|失败|错误/.test(e.text) ? ' e' : /warn|冷却|熔断/.test(e.text) ? ' w' : '';
  const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
  const ch = logCh === 'all'
    ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: '任务', chat: '对话', sys: '系统' }[e.ch] || esc(e.ch)) + '</i>'
    : '';
  return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
}

// renderLogBox 渲染日志区。
// mode='append' 表示「更早」翻页：保持当前视口位置不动，新内容出现在上方。
function renderLogBox(mode) {
  const box = $('logBox');
  if (!box) return;
  // 刷新前记录锚点：只有「本来就在底部 + 看的是最新页」才自动跟随到底。
  // 无条件滚到底会让用户翻历史或向上滚动查看时，被每 5 秒一次的轮询拽走。
  const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  const before = box.scrollHeight;
  box.innerHTML = logEntries.length
    ? logEntries.map(logRowHtml).join('')
    : '<span style="color:var(--ink-3)">' + (logQ || logCh !== 'all' ? '没有匹配的日志' : '暂无日志') + '</span>';
  if (mode === 'append') {
    // 内容往上长了多少，就把视口往下推多少 —— 用户正在看的那一行原地不动。
    box.scrollTop += box.scrollHeight - before;
  } else if (logPin && atEnd && !logCursor) {
    box.scrollTop = box.scrollHeight;
  }
  const older = $('btnLogOlder');
  if (older) { older.disabled = !logNextCursor; older.hidden = !logNextCursor; }
}

// renderLogNote 底部状态：命中总数 + 已加载条数。
function renderLogNote(total) {
  const el = $('logNote');
  if (!el) return;
  const loaded = logEntries.length;
  const cond = [];
  if (logCh !== 'all') cond.push({ task: '任务', chat: '对话', sys: '系统' }[logCh] || logCh);
  if (logQ) cond.push('“' + logQ + '”');
  const scope = cond.length ? cond.join(' · ') : '全部';
  el.textContent = total == null
    ? scope + ' · 已加载 ' + loaded + ' 条'
    : scope + ' · 命中 ' + total + ' 条 · 已加载 ' + loaded + ' 条';
}

$('logChips').addEventListener('click', ev => {
  const b = ev.target.closest('button[data-ch]');
  if (!b) return;
  logCh = b.dataset.ch;
  document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
  logCursor = 0;
  loadLogs();
});
if ($('logQ')) $('logQ').oninput = () => {
  clearTimeout(logQTimer);
  logQTimer = setTimeout(() => { logQ = $('logQ').value.trim(); logCursor = 0; loadLogs(); }, 200);
};
if ($('btnLogOlder')) $('btnLogOlder').onclick = () => {
  if (!logNextCursor) return;
  logCursor = logNextCursor;
  loadLogs(true);
};
if ($('btnLogLatest')) $('btnLogLatest').onclick = () => {
  logCursor = 0;
  loadLogs();
};

async function loadLogs(append) {
  const limit = ($('reqLimit') && $('reqLimit').value) || 100;
  // 时间范围由归档侧过滤（不是前端筛已拉取的条目）：区间落在更早的时间段时，
  // 「最近 N 条」里根本不会有那些记录，必须让服务端按时间取。
  const rq = trangeQuery('reqRange', false);
  rq.set('limit', limit);
  try {
    const [d, metrics, requestRows, credit] = await Promise.all([
      api('logs?' + logQuery()),
      api('request_metrics').catch(() => ({})),
      api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
      api('credit_history?limit=' + creditLimitValue()).catch(() => null),
    ]);
    // 归档开启时以归档为准——「区间内没有记录」是一个真实结果，不能回落成内存里
    // 的最近 100 条（那会把筛选条件之外、时间范围之外的请求显示出来）。
    // 只有归档关闭时才回落到内存指标，保证没有归档的部署仍能看到最近请求。
    const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
    const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
    renderRequestMetrics(metrics, recent);

    const page = d.entries || [];
    // 翻历史页：拼到已加载列表前面。轮询刷新最新页：整体替换。
    logEntries = append ? page.concat(logEntries) : page;
    logNextCursor = d.next_cursor || 0;
    renderLogBox(append ? 'append' : 'replace');
    renderLogNote(d.total);
    renderCreditHistory(credit);
  } catch (e) { /* 概览已提示 */ }
}

function renderRequestMetrics(m, entries) {
  m = m || {};
  const a = m.archive || {};
  $('reqSummary').textContent =
    '已完成 ' + fmtTok(m.completed) +
    ' · 成功 ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
    ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
    ' · 平均 ' + fmtMs(m.avg_duration_ms) +
    ' · 进行中 ' + String(m.in_flight || 0);
  $('reqNote').textContent = a.enabled
    ? 'JSONL 归档 ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · 丢弃 ' + a.dropped_writes + ' 条' : '') +
      (a.last_error ? ' · 错误：' + a.last_error : '')
    : '仅内存指标，JSONL 归档已关闭';

  reqEntries = entries || [];
  renderRequestTable();
}

/* ── 积分历史（每次真实查到余额与上次比对，变动即留痕）─────────────────
   纯格式化函数集中在此，便于前端测试按切片断言；变动列 +N / −N（U+2212，
   与请求记录一致），余额用千分位。 */
function creditNum(n) {
  return Number(n || 0).toLocaleString('zh-CN');
}
function creditDeltaText(delta) {
  const d = Number(delta || 0);
  if (d > 0) return '+' + creditNum(d);
  if (d < 0) return '−' + creditNum(-d);
  return '0';
}
function creditTimeText(e) {
  if (!e || !e.time) return '—';
  const d = new Date(e.time);
  // 正常链路 time 由 Go 的 time.Time 序列化，恒为 RFC3339；这里挡的是手工改文件/
  // 将来换格式等脏数据——toLocaleTimeString 遇到非法日期不抛异常，只会渲染出
  // "Invalid Date" 这种没意义的字样。
  return isNaN(d.getTime()) ? '—' : d.toLocaleTimeString('zh-CN', { hour12: false });
}
function creditAccountText(e) {
  e = e || {};
  return e.account ? String(e.account) : (e.uid || '—');
}
function creditBalanceText(e) {
  return creditNum(e && e.after);
}
function creditDescText(e) {
  return '余额 ' + creditNum(e && e.before) + ' → ' + creditNum(e && e.after);
}
function creditEntryText(e) {
  return creditTimeText(e) + ' | ' + creditAccountText(e) + ' | ' + creditDeltaText(e && e.delta) +
    ' | ' + creditBalanceText(e) + ' | ' + creditDescText(e);
}
function creditHistoryNote(entries) {
  entries = entries || [];
  if (!entries.length) return '暂无积分变动记录';
  let net = 0;
  for (const e of entries) net += Number(e && e.delta || 0);
  return entries.length + ' 条 · 净 ' + (net > 0 ? '+' : net < 0 ? '−' : '') + creditNum(Math.abs(net));
}
function renderCreditHistory(d) {
  const body = $('creditBody'), note = $('creditNote');
  if (!body) return;
  if (!d) {
    if (note) note.textContent = '积分历史不可用';
    body.innerHTML = '<tr><td colspan="5" style="color:var(--ink-3)">积分历史不可用</td></tr>';
    return;
  }
  const entries = d.entries || [];
  if (note) note.textContent = creditHistoryNote(entries);
  body.innerHTML = entries.length
    ? entries.map(e => '<tr>' +
      '<td>' + esc(creditTimeText(e)) + '</td>' +
      '<td>' + esc(creditAccountText(e)) + '</td>' +
      '<td class="num">' + esc(creditDeltaText(e && e.delta)) + '</td>' +
      '<td class="num">' + esc(creditBalanceText(e)) + '</td>' +
      '<td>' + esc(creditDescText(e)) + '</td>' +
      '</tr>').join('')
    : '<tr><td colspan="5" style="color:var(--ink-3)">暂无积分变动记录</td></tr>';
}
function creditLimitValue() {
  const el = $('creditLimit');
  const n = el ? Number(el.value) : 100;
  return n === 300 || n === 1000 ? n : 100;
}
async function loadCreditHistory() {
  let d = null;
  try { d = await api('credit_history?limit=' + creditLimitValue()); } catch (e) { d = null; }
  renderCreditHistory(d);
}

/* reqMatch 请求记录筛选：q 对 IP/UA/模型/账号/请求 ID 做空格分词的 AND 包含匹配，
   outcome 精确匹配。两者都在已拉取的条目上做（最多 1000 条），不发新请求。 */
function reqMatch(e, f) {
  f = f || reqFilter;
  if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
  if (f.q) {
    const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
      .filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  return true;
}

function reqOutcomeTag(e) {
  const outcome = String(e && e.outcome || '');
  const label = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' }[outcome] || outcome || '—';
  const cls = outcome === 'success' ? 'ok'
    : outcome === 'interrupted' ? 'warn'
    : outcome ? 'bad' : 'mute';
  return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
  const total = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
  if (!e || !e.credit_known) return '<span class="muted">—</span>';
  const v = Number(e.credit);
  return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable 渲染请求记录表。来源列是这一版的重点：IP 用等宽字体方便扫，
   UA 单行截断（完整值在 title 里，行本身用 requestLogText 作 tooltip）。 */
function renderRequestTable() {
  const list = reqEntries.filter(e => reqMatch(e));
  const tb = $('reqBody');
  if (!tb) return;
  setTableHTML(tb, list.map(e => {
    const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
    const ip = e && e.client_ip ? e.client_ip : '';
    const ua = e && e.user_agent ? e.user_agent : '';
    const rid = e && e.request_id ? e.request_id : '';
    return '<tr title="' + esc(requestLogText(e)) + '">' +
      '<td class="num">' + esc(when) + '</td>' +
      '<td>' + reqOutcomeTag(e) + '</td>' +
      '<td>' + esc(e && e.model || '—') + '</td>' +
      '<td>' + esc(e && e.account || '—') + '</td>' +
      '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
      '<td class="num">' + reqTokenCell(e) + '</td>' +
      '<td class="num">' + reqCreditCell(e) + '</td>' +
      '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
      '</tr>';
  }).join('') || '<tr><td colspan="10" class="empty">' +
      (reqEntries.length ? '没有符合当前筛选条件的请求记录' : '暂无请求记录') + '</td></tr>');

  const filtered = list.length !== reqEntries.length;
  // 归档里的旧条目没有来源字段（该功能上线前写入）：这时提示开关/历史原因，
  // 而不是让人以为筛选坏了。
  const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
  $('reqCount').textContent = !reqEntries.length ? ''
    : (filtered ? '命中 ' + list.length + ' / ' + reqEntries.length + ' 条' : reqEntries.length + ' 条') +
      (hasSource ? '' : ' · 来源未记录');
  $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

/* 请求记录筛选控件。搜索框防抖 150ms：最多 1000 行重渲染，不必每键一次。
   这段顶层绑定放在 requestLogText 之前，是为了让"纯函数切片"式前端测试
   （slice requestLogText → fmtBytes）只拿到无副作用的格式化函数。 */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
  clearTimeout(reqQTimer);
  reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
  reqFilter.outcome = $('reqOutcome').value;
  renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// 时间范围：默认「全部历史」——请求记录页的历史行为就是"取最近 N 条"，
// 加一个默认收窄的区间会让打开页面时看到的条数凭空变少。
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');
if ($('creditLimit')) $('creditLimit').onchange = loadCreditHistory;
if ($('btnCreditReload')) $('btnCreditReload').onclick = loadCreditHistory;

function requestLogText(e) {
  const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
  const outcomeLabel = { success: '成功', http_error: 'HTTP 错误', stream_error: '流错误', interrupted: '中断' };
  const token = Number(e && e.total_tokens || 0) ||
    (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
  let credit = 'credit —';
  if (e && e.credit_known) {
    const value = Number(e.credit);
    if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
  }
  return [
    when,
    String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
    e && e.model || '—',
    e && e.account || '—',
    e && e.client_ip || '—',
    e && e.user_agent || '—',
    fmtMs(e && e.duration_ms),
    fmtTok(token) + ' tok',
    credit,
    cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens) === '—' ? '' : '命中 ' + cacheRateText(e && e.cache_hit_tokens, e && e.cache_miss_tokens),
    e && e.request_id || '—',
  ].filter(Boolean).join(' | ');
}

/* 缓存命中率纯文本（issue #92）：requestLogText 与积分表/kpi 卡共用。
   自包含（不依赖 trimFixed）：前端纯函数切片测试只截取本段。 */
function cacheRateText(hit, miss) {
  const h = Number(hit || 0), m = Number(miss || 0), total = h + m;
  if (!total) return '—';
  return String(Math.round(h / total * 1000) / 10) + '%';
}

function fmtBytes(bytes) {
  const n = Number(bytes || 0);
  if (n < 1024) return n + ' B';
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
  logPin = !logPin;
  $('btnLogPin').textContent = '自动滚动：' + (logPin ? '开' : '关');
};
