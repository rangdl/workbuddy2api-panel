/* ── 配置 ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
  strategy_domestic: ['pool', 'strategy', 'domestic'],
  strategy_international: ['pool', 'strategy', 'international'],
  domestic_account_order: ['pool', 'account_order', 'domestic'],
  international_account_order: ['pool', 'account_order', 'international'],
  listen: ['listen'], api_key: ['api_key'],
  package_detail_limit: ['panel', 'package_detail_limit'],
  checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
  travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
  travel_blindbox_enabled: ['schedule', 'travel_blindbox_enabled'],
  travel_blindbox_max_per_run: ['schedule', 'travel_blindbox_max_per_run'],
  activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
  keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
  balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
  task_sweep_enabled: ['schedule', 'task_sweep_enabled'], task_sweep_minutes: ['schedule', 'task_sweep_minutes'],
  include_disabled_in_tasks: ['schedule', 'include_disabled_in_tasks'],
  max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
  breaker_threshold: ['pool', 'breaker_threshold'],
  degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
  degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
  cost_explore_interval: ['pool', 'cost_explore_interval'],
  credit_floor: ['pool', 'credit_floor'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
  sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
  session_sticky_enabled: ['session_sticky', 'enabled'],
  request_client_info: ['logging', 'request_client_info'],
};
/* 「覆盖型」文本字段：空串本身是有意义的取值（= 回落到内置默认），必须照发。
 *
 * 其余文本字段保持「空 = 不下发」的既有语义——那是防误清空的保护，不是 bug：
 * 表单里某个框没填，通常意味着"没改"，把它当成"请清空"会静默抹掉配置。
 *
 * 但覆盖型字段正好相反：清空 = 明确要求回到默认。漏发它们会让面板显示"已保存"
 * 而值其实没变（issue #102 附带发现 2：user_agent 清空后 config.json 里仍是旧值）。
 *
 * 刻意不含 api_key：清空它 = 关闭整个鉴权，误触代价是网关变成无鉴权公开服务。
 * 该字段（以及提示文案"留空 = 不鉴权"与现状不符的问题）单独处理。
 */
const CLEARABLE_CFG = new Set(['user_agent', 'prompt_file']);

function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
  let o = obj;
  for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
  o[path[path.length - 1]] = val;
}

async function loadConfig() {
  try {
    const d = await api('config');
    cfgLoaded = d.config;
    $('cfgPath').textContent = d.path || '';
    const f = $('cfgForm');
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = f.elements[name];
      if (!el) continue;
      const v = dig(cfgLoaded, path);
      if (el.type === 'checkbox') el.checked = !!v;
      else if (Array.isArray(v)) el.value = v.join(', ');
      else el.value = v == null ? (name.startsWith('strategy_') ? 'weighted_random' : '') : v;
    }
    markDurationFields(); // 回填后重置校验态（清掉残留红框；现值来自后端必然合法）
    $('cfgNote').textContent = '';
  } catch (e) { toast('读取配置失败：' + e.message, 'err'); }
}
function collectConfig() {
  const f = $('cfgForm'), out = {};
  for (const [name, path] of Object.entries(CFG_MAP)) {
    const el = f.elements[name];
    if (!el) continue;
    let v;
    if (el.type === 'checkbox') v = el.checked;
    else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
    else {
      const raw = el.value.trim();
      // 覆盖型字段空串照发（见 CLEARABLE_CFG）；其余空 = 不下发。
      if (name.endsWith('_account_order')) v = raw.split(/[,，\s]+/).filter(Boolean);
      else if (raw === '') v = CLEARABLE_CFG.has(name) ? '' : undefined;
      else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
      else v = raw;
    }
    if (v !== undefined) put(out, path, v);
  }
  return out;
}
/* Go 时长字段即时校验：空 = 沿用现值（collectConfig 跳过发送）；非空必须是
   ParseDuration 语法（30m / 2h / 600s / 1h30m，可组合可带小数）。与后端
   config.go normalize() 的 time.ParseDuration 同口径，脏值在前端就地标红，
   不再等到保存被拒。 */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
  'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';
function durationBad(name) {
  const el = $('cfgForm').elements[name];
  if (!el) return false;
  const v = el.value.trim();
  return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
  for (const name of DURATION_FIELDS) {
    const el = $('cfgForm').elements[name];
    if (!el) continue;
    const bad = durationBad(name);
    el.classList.toggle('invalid', bad);
    el.title = bad ? DURATION_TIP : '';
  }
}
$('cfgForm').addEventListener('input', ev => {
  if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
  const el = $('cfgKey');
  const show = el.type === 'password';
  el.type = show ? 'text' : 'password';
  $('btnEye').textContent = show ? '隐藏' : '显示';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
  ev.preventDefault();
  // 时长字段脏值拦截：标红 + toast 点名，不发保存请求（后端同样会拒，这里前置）。
  markDurationFields();
  const firstBad = DURATION_FIELDS.find(durationBad);
  if (firstBad) {
    const el = $('cfgForm').elements[firstBad];
    el.focus();
    toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
    return;
  }
  const btn = $('btnCfgSave');
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
    const n = (r.restart_required || []).length;
    toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
    // 密钥可能已改：本次会话沿用新值，避免下一次轮询被 401。
    const k = $('cfgKey').value.trim();
    if (k) localStorage.setItem(LS_KEY, k);
    loadConfig();
    loadOverview(true);
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存配置'; }
};

/* ── Responses / Codex 接入配置（独立 responses.json） ────────────── */
async function loadResponsesConfig() {
  try {
    const d = await api('responses_config');
    const c = d.config || {};
    $('respPath').textContent = d.path || '';
    $('respEnabled').checked = c.enabled !== false;
    $('respDefaultModel').value = c.default_model || '';
    $('respMaxCache').value = c.max_cached_responses == null ? '' : c.max_cached_responses;
    renderRespMapRows(c.model_map || {}, d.codex_models || []);
    $('respNote').textContent = '';
  } catch (e) { /* 后端未提供该接口时静默 */ }
}
/* 模型映射：每行「拖拽手柄 + 左输入框 → 右输入框 + 删除」。
   拖拽手柄用 HTML5 drag&drop 调整行顺序；保存时按 DOM 顺序写盘。 */
let respDragRow = null;
function respMapRow(key, val) {
  const row = document.createElement('div');
  row.className = 'resp-map-row';
  row.style.cssText = 'display:flex;align-items:center;gap:8px';
  const handle = document.createElement('span');
  handle.className = 'resp-map-handle';
  handle.textContent = '⠿';
  handle.title = '拖拽排序';
  handle.style.cssText = 'cursor:grab;user-select:none;opacity:0.55;padding:0 2px';
  handle.draggable = true;
  handle.addEventListener('dragstart', e => {
    respDragRow = row;
    row.style.opacity = '0.4';
    if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move';
  });
  handle.addEventListener('dragend', () => { respDragRow = null; row.style.opacity = ''; });
  row.addEventListener('dragover', e => {
    e.preventDefault();
    if (!respDragRow || respDragRow === row) return;
    const rect = row.getBoundingClientRect();
    const after = e.clientY > rect.top + rect.height / 2;
    row.parentNode.insertBefore(respDragRow, after ? row.nextSibling : row);
  });
  const k = document.createElement('input');
  k.className = 'resp-map-key';
  k.placeholder = '客户端模型名（如 gpt-5.6-sol）';
  k.value = key || '';
  k.style.flex = '1';
  const arrow = document.createElement('span');
  arrow.textContent = '→';
  arrow.style.opacity = '0.5';
  const v = document.createElement('input');
  v.className = 'resp-map-val';
  v.placeholder = '上游模型名（如 glm-5.2）';
  v.value = val || '';
  v.style.flex = '1';
  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'xs';
  del.textContent = '删除';
  del.onclick = () => row.remove();
  row.append(handle, k, arrow, v, del);
  return row;
}
function renderRespMapRows(modelMap, codexModels) {
  const box = $('respMapRows');
  box.innerHTML = '';
  const entries = Object.entries(modelMap);
  if (entries.length === 0) {
    // 未配置：展示默认模型列表（左列，右列留空待填）。
    for (const m of codexModels) box.append(respMapRow(m, ''));
  } else {
    // 已配置：只展示已保存的映射（不再叠加默认列表）。
    for (const [k, v] of entries) box.append(respMapRow(k, v));
  }
}
function collectRespModelMap() {
  const map = {};
  for (const row of $('respMapRows').children) {
    const k = row.querySelector('.resp-map-key').value.trim();
    const v = row.querySelector('.resp-map-val').value.trim();
    if (k && v) map[k] = v; // 右侧留空 = 不映射
  }
  return map;
}
function collectResponsesConfig() {
  const maxRaw = $('respMaxCache').value.trim();
  const cfg = { enabled: $('respEnabled').checked, model_map: collectRespModelMap(), default_model: $('respDefaultModel').value.trim() };
  if (maxRaw !== '') cfg.max_cached_responses = Number(maxRaw);
  return cfg;
}
$('btnRespAddRow').onclick = () => $('respMapRows').append(respMapRow('', ''));
$('btnRespReload').onclick = loadResponsesConfig;
$('btnRespSave').onclick = async () => {
  const btn = $('btnRespSave');
  let cfg;
  try { cfg = collectResponsesConfig(); }
  catch (e) { toast(e.message, 'err'); return; }
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('responses_config', { method: 'POST', body: JSON.stringify(cfg) });
    const n = (r.restart_required || []).length;
    toast(n ? 'Responses 配置已保存，部分项需重启生效' : 'Responses 配置已保存并立即生效', 'ok');
    loadResponsesConfig();
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存 Responses 配置'; }
};

/* ── 上游接入：第三方 OpenAI 兼容上游（独立 third_party.json） ────── */
/* 每个上游一行：名称 / 接口地址 / API Key / 模型列表 / 超时 + 测试连接 + 删除。
   models 逗号分隔；留空 = 兜底上游（承接未命中其他上游的模型名）。 */
function tpInput(cls, ph, val, flex) {
  const i = document.createElement('input');
  i.className = cls;
  i.placeholder = ph;
  i.value = val == null ? '' : val;
  if (flex) i.style.flex = flex;
  return i;
}
function tpProviderRow(p) {
  p = p || {};
  const row = document.createElement('div');
  row.className = 'tp-row';
  row.style.cssText = 'border:1px solid var(--line);border-radius:8px;padding:10px;display:flex;flex-direction:column;gap:8px';

  const r1 = document.createElement('div');
  r1.style.cssText = 'display:flex;gap:8px;align-items:center';
  const name = tpInput('tp-name', '名称（如 deepseek）', p.name, '0 0 180px');
  const base = tpInput('tp-base', '接口地址（如 https://api.deepseek.com/v1）', p.base_url, '1');
  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'xs';
  del.textContent = '删除';
  del.onclick = () => row.remove();
  r1.append(name, base, del);

  const r2 = document.createElement('div');
  r2.style.cssText = 'display:flex;gap:8px;align-items:center';
  const key = tpInput('tp-key', 'API Key（留空 = 不发鉴权头）', p.api_key, '1');
  key.type = 'password';
  const models = tpInput('tp-models', '模型名，逗号分隔（留空 = 兜底）', (p.models || []).join(', '), '1');
  const timeout = tpInput('tp-timeout', '超时秒', p.timeout_seconds, '0 0 90px');
  timeout.type = 'number';
  const test = document.createElement('button');
  test.type = 'button';
  test.className = 'xs';
  test.textContent = '测试连接';
  test.onclick = async () => {
    let provider;
    try { provider = tpRowProvider(row); }
    catch (e) { toast(e.message, 'err'); return; }
    if (!provider.base_url) { toast('请先填写接口地址', 'err'); return; }
    test.disabled = true;
    test.textContent = '测试中…';
    try {
      const r = await api('thirdparty_test', { method: 'POST', body: JSON.stringify({ provider }) });
      if (r.ok) toast(r.message || '连通成功', 'ok');
      else toast('连接失败：' + (r.error || '未知错误'), 'err');
    } catch (e) { toast('测试失败：' + e.message, 'err'); }
    finally { test.disabled = false; test.textContent = '测试连接'; }
  };
  r2.append(key, models, timeout, test);

  row.append(r1, r2);
  return row;
}
/* tpRowProvider 收集一行；名称为空视为未填写（调用方据此跳过整行）。 */
function tpRowProvider(row) {
  const txt = s => row.querySelector(s).value.trim();
  const models = txt('.tp-models').split(',').map(s => s.trim()).filter(Boolean);
  const p = {
    name: txt('.tp-name'),
    protocol: 'openai',
    base_url: txt('.tp-base'),
    api_key: row.querySelector('.tp-key').value,
    models: models,
  };
  const t = txt('.tp-timeout');
  if (t !== '') p.timeout_seconds = Number(t);
  return p;
}
function renderTpProviders(list) {
  const box = $('tpRows');
  box.innerHTML = '';
  if (!list || !list.length) { box.append(tpProviderRow({})); return; }
  for (const p of list) box.append(tpProviderRow(p));
}
function collectThirdPartyConfig() {
  const providers = [];
  for (const row of $('tpRows').children) {
    const p = tpRowProvider(row);
    if (!p.name && !p.base_url) continue; // 整行空白 = 跳过
    if (!p.name) throw new Error('每个上游都要填名称');
    if (!p.base_url) throw new Error('上游「' + p.name + '」缺少接口地址');
    providers.push(p);
  }
  return { enabled: $('tpEnabled').checked, providers: providers };
}
async function loadThirdPartyConfig() {
  try {
    const d = await api('thirdparty_config');
    const c = d.config || {};
    $('tpPath').textContent = d.path || '';
    $('tpEnabled').checked = !!c.enabled;
    renderTpProviders(c.providers || []);
    $('tpNote').textContent = '';
  } catch (e) { /* 后端未提供该接口时静默 */ }
}
$('btnTpAddRow').onclick = () => $('tpRows').append(tpProviderRow({}));
$('btnTpReload').onclick = loadThirdPartyConfig;
$('btnTpSave').onclick = async () => {
  const btn = $('btnTpSave');
  let cfg;
  try { cfg = collectThirdPartyConfig(); }
  catch (e) { toast(e.message, 'err'); return; }
  btn.disabled = true;
  btn.textContent = '保存中…';
  try {
    const r = await api('thirdparty_config', { method: 'POST', body: JSON.stringify(cfg) });
    const n = (r.restart_required || []).length;
    toast(n ? '上游配置已保存，部分项需重启生效' : '上游配置已保存并立即生效', 'ok');
    loadThirdPartyConfig();
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存上游配置'; }
};

/* ── 上游接入：Trae（独立 trae.json） ─────────────────────────────── */
/* 账号表与「账号池」同构（9 列）：健康色条 / 账号 / 状态 / 积分 / 成功失败 /
   在途 / 用量 / 最近成功 / 操作。凭证永不回显（后端只给 has_* 布尔与尾号），
   故本页只维护"保留哪些账号 / 顺序 / 启用态"。

   与 WorkBuddy 账号池的差异只在数据来源：那边来自 pool（落盘 + Redis），
   这边来自 trae.Stats（进程内，重启清零）+ 签到状态缓存 + 积分查询。 */

/* traeCreditsHtml 积分列：有上限时画进度条，否则只显示剩余。
   数据优先级：权威查询（有 limit）> notify_usage 余额 > 未知。 */
function traeCreditsHtml(st) {
  if (!st || (!st.credits && !st.credits_limit)) {
    return '<div class="n" style="color:var(--ink-3)">—</div>';
  }
  const limit = st.credits_limit || 0;
  const remain = st.credits || 0;
  const pct = limit > 0 ? Math.min(100, Math.round(remain / limit * 100)) : 0;
  const num = v => Math.round(v).toLocaleString();
  const tip = '通用 ' + num(st.credits_ide || 0) + ' / Work ' + num(st.credits_work || 0) +
    (limit > 0 ? '（额度 ' + num(limit) + '，' + pct + '%）' : '（额度未知）') +
    (st.credits_at ? '\n更新于 ' + ago(st.credits_at) : '');
  return '<div class="n" title="' + esc(tip) + '">' + num(remain) +
    (limit > 0 ? '<span class="of">/' + num(limit) + '</span>' : '') + '</div>' +
    (limit > 0 ? '<div class="bar"><i style="width:' + pct + '%"></i></div>' : '');
}

/* traeUsageHtml 用量列：与账号池同一套 formatTokenCount / formatLatency / formatRate。 */
function traeUsageHtml(st) {
  const tu = (st && st.token_usage) || {};
  const req = tu.request_count || 0;
  const totalTok = formatTokenCount(tu.total_tokens);
  const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
  const latency = formatLatency(st ? st.last_latency_ms : 0);
  const rate = formatRate(tu.last_tokens_per_second);
  const title = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate +
    (tu.last_model ? '\n模型：' + tu.last_model : '');
  return '<span class="usage-line" aria-label="' + esc(title) + '">' +
    '<span class="usage-item usage-count" title="' + esc(title) + '"><b>' + req + '</b><em>次</em></span>' +
    '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
    '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
    '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
  '</span>';
}

/* traeStatusHtml 状态列：冷却 > 禁用 > 凭证缺失 > 可用；附签到状态。
   返回的是内层容器：不能把 display:flex 直接设在 <td> 上——那会让单元格脱离
   表格布局（变成普通 flex 容器），整行列宽错位。账号池的 rate-limits 同理。 */
function traeStatusHtml(a, cool) {
  const parts = [];
  if (a.disabled) parts.push('<span class="tag bad">已禁用</span>');
  else if (cool) {
    const rem = Math.max(1, Math.round(cool.remaining_sec / 60));
    parts.push('<span class="tag warn" title="' + esc(cool.reason || '') + '">冷却 · ' + rem + ' 分钟</span>');
  } else if (!a.has_token) parts.push('<span class="tag bad">无 token</span>');
  else parts.push('<span class="tag ok">可用</span>');

  if (!a.has_refresh) parts.push('<span class="tag warn">无 refresh</span>');
  if (!a.has_device_key) parts.push('<span class="tag warn" title="设备私钥缺失：token 续期需 DeviceProof 签名">无设备密钥</span>');
  if (a.has_token && a.token_tail) {
    parts.push('<span class="tag mute" title="access token 尾号（仅用于核对是哪一份凭证）">token ' + esc(a.token_tail) + '</span>');
  }

  // 签到状态：Done=账号已签；DeviceAlready=该设备已签（限额按设备算）。
  const done = a.checkin_done === true;
  const devDone = a.checkin_device_already === true;
  if (done) parts.push('<span class="tag ok">今日已签</span>');
  else if (devDone) parts.push('<span class="tag mute" title="签到限额按设备算，本账号今日无法再签">设备已签</span>');
  else if (a.checkin_error) {
    parts.push('<span class="tag warn" title="' + esc(a.checkin_error) + '">签到状态未知</span>');
  }
  return parts.join('');
}

function traeAccountRow(a, cool) {
  const tr = document.createElement('tr');
  tr.dataset.uid = a.uid;
  const st = a.stats || {};

  // 1. 健康色条：与账号池同色系（禁用/冷却=warn-bad）。
  const tdMark = document.createElement('td');
  tdMark.className = 'mark';
  tdMark.setAttribute('aria-hidden', 'true');
  const bar = document.createElement('i');
  tdMark.append(bar);

  // 2. 账号列：勾选框（= 是否参与轮转）+ uid + 名称。
  const tdWho = document.createElement('td');
  tdWho.className = 'who';
  const nm = document.createElement('div');
  nm.className = 'nm';
  const on = document.createElement('input');
  on.type = 'checkbox';
  on.className = 'trae-acc-on';
  on.checked = !a.disabled;
  on.title = '取消勾选 = 该账号不参与轮转（保存后生效）';
  on.style.cssText = 'margin-right:6px;vertical-align:middle';
  const nmText = document.createElement('span');
  nmText.className = 'mid';
  nmText.textContent = a.name || '未命名';
  nm.append(on, nmText);
  const id = document.createElement('div');
  id.className = 'id';
  id.title = 'uid: ' + a.uid;
  id.textContent = a.uid;
  tdWho.append(nm, id);

  // 3. 状态列：flex 设在**内层 div** 上，<td> 保持表格单元格语义。
  const tdStatus = document.createElement('td');
  tdStatus.innerHTML = '<div class="trae-tags">' + traeStatusHtml(a, cool) + '</div>';

  // 4. 积分列。
  const tdCred = document.createElement('td');
  tdCred.className = 'cred';
  tdCred.innerHTML = traeCreditsHtml(st);

  // 5. 成功 / 失败。
  const tdOk = document.createElement('td');
  tdOk.className = 'num';
  tdOk.innerHTML = (st.success_count || 0) +
    ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (st.err_total || 0) + '</span>';

  // 6. 在途。
  const tdFlight = document.createElement('td');
  tdFlight.className = 'num';
  tdFlight.textContent = String(st.in_flight || 0);

  // 7. 用量。
  const tdUsage = document.createElement('td');
  tdUsage.className = 'num usage-cell';
  tdUsage.innerHTML = traeUsageHtml(st);

  // 8. 最近成功。
  const tdLast = document.createElement('td');
  tdLast.className = 'num';
  tdLast.style.color = 'var(--ink-3)';
  tdLast.textContent = ago(st.last_success);

  // 9. 操作。
  const tdActs = document.createElement('td');
  tdActs.className = 'acts';

  const ck = document.createElement('button');
  ck.type = 'button';
  ck.className = 'xs ghost';
  const done = a.checkin_done === true;
  const devDone = a.checkin_device_already === true;
  ck.textContent = done ? '已签' : (devDone ? '设备已签' : '签到');
  if (done) ck.title = '今日已签到；点一下可重新查询并刷新状态';
  else if (devDone) ck.title = '该设备今日已签（签到限额按设备算，本账号今日无法再签）；点一下可重新查询';
  else if (a.checkin_error) ck.title = '签到状态查询失败：' + a.checkin_error;
  ck.onclick = async () => {
    ck.disabled = true;
    ck.textContent = '签到中…';
    try {
      const d = await api('trae/accounts/' + encodeURIComponent(a.uid) + '/checkin', { method: 'POST' });
      if (d.ok && d.device_already) toast(a.uid + ' 该设备今日已签到（签到限额按设备算，不是按账号）', 'warn');
      else if (d.ok && d.unverified) toast(a.uid + ' 领取返回成功但回查未确认，下轮自动重试', 'warn');
      else if (d.ok) toast(a.uid + (d.already ? ' 今日已签到' : ' 签到成功' + (d.reward > 0 ? ' +' + d.reward : '')), 'ok');
      else toast('签到失败：' + (d.message || '未知错误'), 'err');
      loadTraeConfig();
    } catch (e) { toast('签到失败：' + e.message, 'err'); }
    finally { ck.disabled = false; ck.textContent = done ? '已签' : '签到'; }
  };

  const cr = document.createElement('button');
  cr.type = 'button';
  cr.className = 'xs ghost';
  cr.textContent = '积分';
  cr.onclick = async () => {
    cr.disabled = true;
    cr.textContent = '查询中…';
    try {
      const d = await api('trae/accounts/' + encodeURIComponent(a.uid) + '/credits', { method: 'POST' });
      if (d.ok && d.credits) {
        // 后端字段是小写（general/work/total/limit）；用 num() 兜底，
        // 避免任何缺失字段把整个提示变成 NaN。
        const c = d.credits;
        const num = v => (typeof v === 'number' && isFinite(v)) ? Math.round(v).toLocaleString() : '—';
        toast('通用 ' + num(c.general) + ' / Work ' + num(c.work) +
          '（合计 ' + num(c.total) + '，额度 ' + num(c.limit) + '）', 'ok');
      } else {
        toast('积分查询失败：' + (d.message || '未知错误'), 'err');
      }
      loadTraeConfig();
    } catch (e) { toast('积分查询失败：' + e.message, 'err'); }
    finally { cr.disabled = false; cr.textContent = '积分'; }
  };

  const del = document.createElement('button');
  del.type = 'button';
  del.className = 'xs ghost danger';
  del.textContent = '删除';
  del.onclick = async () => {
    if (!confirm('删除 Trae 账号 ' + a.uid + '？其凭证（token / 设备私钥）一并删除，不可恢复。')) return;
    try {
      await api('trae/accounts/' + encodeURIComponent(a.uid) + '/remove', { method: 'POST' });
      toast('已删除', 'ok');
      loadTraeConfig();
    } catch (e) { toast('删除失败：' + e.message, 'err'); }
  };

  tdActs.append(ck, cr, del);
  tr.append(tdMark, tdWho, tdStatus, tdCred, tdOk, tdFlight, tdUsage, tdLast, tdActs);
  return tr;
}

function renderTraeAccounts(list, cooldowns) {
  const tb = $('traeAccBody');
  if (!tb) return;
  const coolMap = {};
  for (const c of (cooldowns || [])) coolMap[c.uid] = c;
  if (!list || !list.length) {
    tb.innerHTML = '<tr><td colspan="9"><div class="empty">还没有 Trae 账号 —— 用下方的「生成授权链接」完成一次 OAuth 登录</div></td></tr>';
    return;
  }
  // 先建片段再一次性替换：逐个 append 会让浏览器多次重排，账号多时可见抖动。
  const frag = document.createDocumentFragment();
  for (const a of list) frag.append(traeAccountRow(a, coolMap[a.uid]));
  tb.replaceChildren(frag);
}
function collectTraeConfig() {
  const accounts = [];
  const disabled = [];
  for (const row of $('traeAccBody').children) {
    const uid = row.dataset.uid;
    if (!uid) continue;
    accounts.push(uid);
    const on = row.querySelector('.trae-acc-on');
    if (on && !on.checked) disabled.push(uid);
  }
  return {
    enabled: $('traeEnabled').checked,
    chat_base: $('traeChatBase').value.trim(),
    ide_version: $('traeIdeVersion').value.trim(),
    timeout_seconds: 120,
    accounts: accounts,
    disabled: disabled,
    schedule: {
      checkin_enabled: $('traeCheckinEnabled').checked,
      checkin_hours: parseHours($('traeCheckinHours').value),
    },
  };
}
/* parseHours 解析 "9, 21" → [9,21]（非法项丢弃；空串 → []）。 */
function parseHours(s) {
  return String(s || '').split(',').map(x => parseInt(x.trim(), 10))
    .filter(n => Number.isInteger(n) && n >= 0 && n <= 23);
}
async function loadTraeConfig() {
  try {
    const d = await api('trae_config');
    const c = d.config || {};
    $('traePath').textContent = d.path || '';
    $('traeEnabled').checked = !!c.enabled;
    $('traeChatBase').value = c.chat_base || '';
    $('traeIdeVersion').value = c.ide_version || '';
    renderTraeAccounts(c.accounts || [], c.cooldowns || []);
    $('traeUsable').textContent = '可参与轮转：' + (c.usable || 0) + ' / ' + ((c.accounts || []).length) + ' 个账号';
    const sch = c.schedule || {};
    $('traeCheckinEnabled').checked = !!sch.checkin_enabled;
    $('traeCheckinHours').value = (sch.checkin_hours || []).join(', ');
    $('traeNote').textContent = '';
  } catch (e) { /* 后端未提供该接口时静默 */ }
}
$('btnTraeSyncProfiles').onclick = async () => {
  const btn = $('btnTraeSyncProfiles');
  btn.disabled = true; btn.textContent = '同步中…';
  try {
    const d = await api('trae/sync_profiles', { method: 'POST' });
    toast('昵称同步完成：更新 ' + (d.updated || 0) + ' 个' +
      (d.failed ? '，失败 ' + d.failed + ' 个' : ''), d.failed ? 'warn' : 'ok');
    loadTraeConfig();
  } catch (e) { toast('同步失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '同步昵称'; }
};
$('btnTraeReload').onclick = loadTraeConfig;
$('btnTraeSave').onclick = async () => {
  const btn = $('btnTraeSave');
  btn.disabled = true;
  btn.textContent = '保存中…';
  try {
    await api('trae_config', { method: 'POST', body: JSON.stringify(collectTraeConfig()) });
    toast('Trae 配置已保存并立即生效', 'ok');
    loadTraeConfig();
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存 Trae 配置'; }
};
/* traeLoginBody 收集登录发起参数：storage.json 优先（它自带设备号）。 */
function traeLoginBody() {
  const body = {};
  const storage = ($('traeLoginStorage').value || '').trim();
  const devId = ($('traeLoginDeviceId').value || '').trim();
  if (storage) body.storage_json = storage;
  if (devId) body.device_id = devId;
  return body;
}
$('btnTraeLogin').onclick = async () => {
  const btn = $('btnTraeLogin');
  btn.disabled = true;
  try {
    const d = await api('trae/login/start', { method: 'POST', body: JSON.stringify(traeLoginBody()) });
    const src = {
      storage: '来自 storage.json',
      explicit: '手动指定',
      imported: '沿用已导入设备',
      shared: '与已有账号共用，一天只有一个能签成',
      generated: '自动生成，登录会被拒（20405）',
    }[d.device_source] || d.device_source;
    $('traeLoginHint').innerHTML = '授权链接已生成（设备 ' + esc(d.device_id || '') +
      ' · ' + esc(src) + '）：' +
      '<a href="' + esc(d.url) + '" target="_blank" rel="noopener">打开授权页</a>。' +
      '完成授权后浏览器会跳转到回调地址，把**地址栏完整 URL** 复制到右侧输入框。';
    window.open(d.url, '_blank', 'noopener');
    toast(d.warning || '授权链接已生成，请在浏览器完成授权', d.warning ? 'warn' : 'ok');
  } catch (e) { toast('生成失败：' + e.message, 'err'); }
  finally { btn.disabled = false; }
};
$('btnTraeFinish').onclick = async () => {
  const cb = $('traeCallback').value.trim();
  if (!cb) { toast('请先粘贴浏览器地址栏的完整回调 URL', 'err'); return; }
  const btn = $('btnTraeFinish');
  btn.disabled = true;
  btn.textContent = '登录中…';
  try {
    const body = { callback_url: cb };
    const devId = ($('traeLoginDeviceId').value || '').trim();
    if (devId) body.device_id = devId;
    const d = await api('trae/login/finish', { method: 'POST', body: JSON.stringify(body) });
    toast('登录成功：' + d.uid + (d.has_device_key ? '（含设备签名能力）' : '（无设备密钥，续期可能受限）'), 'ok');
    $('traeCallback').value = '';
    $('traeLoginStorage').value = '';
    loadTraeConfig();
  } catch (e) { toast('登录失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '完成登录'; }
};
$('btnTraeImport').onclick = async () => {
  const raw = $('traeStorage').value.trim();
  const uid = $('traeStorageUid').value.trim();
  if (!raw) { toast('请粘贴 storage.json 内容或单条 icube 密文', 'err'); return; }
  // 两种形态：整文件（{ 开头）或单条 "deviceId:密文"。
  let body;
  if (raw.startsWith('{')) {
    body = { uid: uid, storage_json: raw };
  } else if (raw.indexOf(':') > 0) {
    const i = raw.indexOf(':');
    body = { uid: uid, device_id: raw.slice(0, i).trim(), icube_value: raw.slice(i + 1).trim() };
  } else {
    toast('单条模式请按 deviceId:密文 格式粘贴，或直接粘贴整个 storage.json', 'err');
    return;
  }
  const btn = $('btnTraeImport');
  btn.disabled = true;
  try {
    const d = await api('trae/import', { method: 'POST', body: JSON.stringify(body) });
    if (d.matched_uids && !d.uid) {
      // 未能自动确定账号：把候选展示出来让人确认（但不自动写入）。
      const ids = (d.device_ids || []).join('、');
      const cand = (d.matched_uids || []).join('、');
      toast('已解析设备 ' + (ids || '?') + '；' + (cand ? '可绑定账号：' + cand : '没有可匹配的账号') +
        '。请填写 uid 后重试', 'warn');
      return;
    }
    toast(d.message || '设备凭证已导入', 'ok');
    $('traeStorage').value = '';
    loadTraeConfig();
  } catch (e) { toast('导入失败：' + e.message, 'err'); }
  finally { btn.disabled = false; }
};
/* renderTraeCheckinNote 把逐账号签到结果压成一行摘要（失败项直接列出原因）。 */
function renderTraeCheckinNote(results, lastRun) {
  const parts = [];
  for (const r of results) {
    let tag = '✗';
    if (r.status === 'success') tag = '✓';
    else if (r.status === 'already') tag = '=';
    else if (r.status === 'device_already') tag = '☑';
    else if (r.status === 'unverified') tag = '?';
    else if (r.status === 'skipped') tag = '-';
    let s = tag + ' ' + (r.uid || '');
    if (r.status === 'success' && r.reward > 0) s += ' +' + r.reward;
    if (r.status === 'fail' || r.status === 'unverified') s += ' ' + (r.message || '');
    parts.push(s);
  }
  $('traeCheckinNote').textContent = (lastRun ? '[' + lastRun + '] ' : '') + parts.join('　');
}
$('btnTraeCheckinAll').onclick = async () => {
  const btn = $('btnTraeCheckinAll');
  if (!confirm('对全部可用 Trae 账号执行一轮签到？（账号间限速，账号多时耗时较长）')) return;
  btn.disabled = true;
  btn.textContent = '签到中…';
  try {
    const d = await api('trae/checkin_all', { method: 'POST' });
    const rs = d.results || [];
    let ok = 0, already = 0, devAlready = 0, unverified = 0, fail = 0;
    for (const r of rs) {
      if (r.status === 'success') ok++;
      else if (r.status === 'already') already++;
      else if (r.status === 'device_already') devAlready++;
      else if (r.status === 'unverified') unverified++;
      else if (r.status === 'fail') fail++;
    }
    let msg = '签到完成：成功 ' + ok + '，已签 ' + already;
    if (devAlready) msg += '，设备今日已签 ' + devAlready;
    if (unverified) msg += '，待确认 ' + unverified;
    msg += '，失败 ' + fail;
    toast(msg, (fail || unverified) ? 'err' : 'ok');
    renderTraeCheckinNote(rs);
    loadTraeConfig();
  } catch (e) { toast('签到失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '全部签到'; }
};
$('btnTraeCheckinStatus').onclick = async () => {
  try {
    const d = await api('trae/checkin_status');
    const rs = d.results || [];
    if (!rs.length) { toast('还没有签到记录', ''); return; }
    renderTraeCheckinNote(rs, d.last_run);
  } catch (e) { toast('查询失败：' + e.message, 'err'); }
};

/* ── Anthropic / Claude Code 接入（独立 anthropic.json，模式同 Responses 配置） ── */
async function loadAnthropicConfig() {
  try {
    const d = await api('anthropic_config');
    const c = d.config || {};
    $('anthPath').textContent = d.path || '';
    $('anthEnabled').checked = c.enabled === true; // 默认禁用（与 Responses 相反）
    $('anthDefaultModel').value = c.default_model || '';
    renderAnthMapRows(c.model_map || {}, d.claude_models || []);
    $('anthNote').textContent = '';
  } catch (e) { /* 后端未提供该接口时静默 */ }
}
/* 模型映射行：与 Responses 页同一套拖拽行组件（respMapRow），仅容器与占位符不同。 */
function renderAnthMapRows(modelMap, claudeModels) {
  const box = $('anthMapRows');
  box.innerHTML = '';
  const entries = Object.entries(modelMap);
  if (entries.length === 0) {
    for (const m of claudeModels) box.append(respMapRow(m, ''));
  } else {
    for (const [k, v] of entries) box.append(respMapRow(k, v));
  }
  // respMapRow 的占位符面向 codex 场景；Anthropic 行覆写占位提示。
  for (const row of box.children) {
    const k = row.querySelector('.resp-map-key');
    const v = row.querySelector('.resp-map-val');
    if (k) k.placeholder = 'Claude 模型名（如 claude-sonnet-4-5）';
    if (v) v.placeholder = '上游模型名（如 glm-5.2）';
  }
}
function collectAnthModelMap() {
  const map = {};
  for (const row of $('anthMapRows').children) {
    const k = row.querySelector('.resp-map-key').value.trim();
    const v = row.querySelector('.resp-map-val').value.trim();
    if (k && v) map[k] = v;
  }
  return map;
}
$('btnAnthAddRow').onclick = () => $('anthMapRows').append(respMapRow('', ''));
$('btnAnthReload').onclick = loadAnthropicConfig;
$('btnAnthSave').onclick = async () => {
  const btn = $('btnAnthSave');
  const cfg = { enabled: $('anthEnabled').checked, model_map: collectAnthModelMap(), default_model: $('anthDefaultModel').value.trim() };
  btn.disabled = true; btn.textContent = '保存中…';
  try {
    const r = await api('anthropic_config', { method: 'POST', body: JSON.stringify(cfg) });
    const n = (r.restart_required || []).length;
    toast(n ? 'Anthropic 配置已保存，部分项需重启生效' : 'Anthropic 配置已保存并立即生效', 'ok');
    loadAnthropicConfig();
  } catch (e) { toast('保存失败：' + e.message, 'err'); }
  finally { btn.disabled = false; btn.textContent = '保存 Anthropic 配置'; }
};
/* ══ 账号备注（只写本机 localStorage；账号池 / 用量两个视图共用同一份映射）══════
   键取 uid 前 8 位——与 renderUsageDim 账号维度的 key 和积分扣除表的 uid 前缀同源，
   三个表格因此都能直接 join。展示层只替换文本并挂 .rm-ghost 虚体，账号标识本身
   仍由服务端下发，不写回后端；备注是纯展示层信息，不参与选号 / 计费 / 鉴权。 */
function remarksAll() {
  if (remarksCache) return remarksCache;
  try { remarksCache = JSON.parse(localStorage.getItem(LS_REMARKS)) || {}; }
  catch (e) { remarksCache = {}; }
  return remarksCache;
}
function remarkKey(uid) { return String(uid == null ? '' : uid).slice(0, 8); }
function remarkOf(uid) {
  const k = remarkKey(uid);
  return k ? String(remarksAll()[k] || '') : '';
}
/* nameHtml 必须是已转义的 HTML；有备注时返回「备注 + 原标识（浅色虚体）」 */
function remarkName(uid, nameHtml) {
  const r = remarkOf(uid);
  return r ? esc(r) + '<span class="rm-ghost">' + nameHtml + '</span>' : nameHtml;
}
function storeRemarks(map) {
  remarksCache = map;
  try { localStorage.setItem(LS_REMARKS, JSON.stringify(map)); } catch (e) {}
}

/* 编辑器的行来自 overview 缓存（账号池加载过就有），另外补上「已有备注但当前列表里
   没有」的键，避免账号被移除后备注看起来凭空消失。 */
function remarkFormRows() {
  const rows = ((overviewData && overviewData.accounts) || [])
    .map(s => ({ uid: remarkKey(s.uid), who: s.nickname || remarkKey(s.uid) }))
    .filter(r => r.uid);
  for (const k of Object.keys(remarksAll())) {
    if (!rows.some(r => r.uid === k)) rows.push({ uid: k, who: k });
  }
  return rows;
}
function renderRemarkBody() {
  const box = $('remarkBody');
  if (!box) return;
  const rows = remarkFormRows();
  box.innerHTML = rows.length
    ? rows.map(r => '<div class="rm-row"><span class="rm-who" title="' + esc(r.who) + '">' + esc(r.who) + '</span>' +
        '<input type="text" data-uid="' + esc(r.uid) + '" value="' + esc(remarksAll()[r.uid] || '') + '" placeholder="备注（留空即清除）" maxlength="24"></div>').join('')
    : '<div class="empty">暂无账号：先在「账号池」加载一次账号列表。</div>';
}
function openRemarks() {
  renderRemarkBody();
  if ($('remarkVeil')) $('remarkVeil').classList.add('on');
}
function closeRemarks() { if ($('remarkVeil')) $('remarkVeil').classList.remove('on'); }
function saveRemarkForm() {
  const map = {};
  document.querySelectorAll('#remarkBody input[data-uid]').forEach(inp => {
    const v = inp.value.trim();
    if (v) map[inp.getAttribute('data-uid')] = v;
  });
  storeRemarks(map);
  closeRemarks();
  // 备注只影响本机展示：重渲染当前视图即可，无需后端参与。
  if (view === 'accounts') loadOverview(true);
  else if (view === 'usage' && usageData) renderUsage(usageData);
  toast('备注已保存（仅本机浏览器）');
}
if ($('btnRemarks')) $('btnRemarks').onclick = openRemarks;
if ($('btnRemarkClose')) $('btnRemarkClose').onclick = closeRemarks;
if ($('btnRemarkClear')) $('btnRemarkClear').onclick = () => { storeRemarks({}); renderRemarkBody(); };
if ($('btnRemarkSave')) $('btnRemarkSave').onclick = saveRemarkForm;

/* ══ 首页「积分到期提醒」折叠（选择记在本机，默认展开）═════════════════════
   展开态是原行为：点「检查」就要能立刻看到结果，所以默认不折叠；用户收起一次后
   由 localStorage 记住。折叠只作用于列表容器，卡片标题与「检查」按钮始终可见。 */
function expFolded() { return localStorage.getItem(LS_EXP_FOLD) === '1'; }
function applyExpFold() {
  const pad = $('expPad'), btn = $('btnExpFold');
  if (!pad || !btn) return;
  const folded = expFolded();
  pad.hidden = folded;
  btn.textContent = folded ? '展开' : '收起';
  btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
}
if ($('btnExpFold')) {
  $('btnExpFold').onclick = () => {
    try { localStorage.setItem(LS_EXP_FOLD, expFolded() ? '0' : '1'); } catch (e) {}
    applyExpFold();
  };
}
applyExpFold();
