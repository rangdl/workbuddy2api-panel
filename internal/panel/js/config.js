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
  cross_domain_fallback: ['pool', 'cross_domain_fallback'],
  prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
  soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
  breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
  idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
  ttl: ['session_sticky', 'ttl'],
  // 超时属于**传输层**，两个上游共用（对话都是 SSE 长连接），故留在主配置。
  // UA / 版本 / 域 base / 脱敏是上游专有，走 codebuddy.json（见
  // collectCodeBuddyConfig / loadCodeBuddyConfig）。
  timeout_seconds: ['upstream', 'timeout_seconds'],
  header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
  idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'],
  prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
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

/* ── 分上游自动签到勾选 ────────────────────────────────────────────────
   语义（与后端 schedule.checkin_backends 一一对应）：
   - 总开关 checkin_enabled 关掉时整类任务不跑，本区勾选仍保留（只是无效）；
   - 每个上游一个勾选，不勾 = 该上游不参与自动签到；
   - **配置里没有该上游的键 = 启用**。这里把缺省值也渲染成勾选状态，让显示与
     实际行为一致（否则用户会看到"能签到却显示关着"）。

   上游列表来自后端返回的「账号池里实际存在的上游」，不是硬编码：
   新增上游、或某上游账号全删光，列表自动跟着变。 */
const BACKEND_LABEL_CFG = { codebuddy: 'WorkBuddy', trae: 'Trae' };
let checkinBackendsState = null; // 加载时缓存，collect 时读它

function renderCheckinBackends(backends, enabled) {
  const box = $('checkinBackends');
  if (!box) return;
  const list = backends || [];
  checkinBackendsState = {};
  if (!list.length) {
    box.innerHTML = '<span class="hint">账号池里还没有账号，添加后这里会列出对应上游</span>';
    return;
  }
  const frag = document.createDocumentFragment();
  for (const be of list) {
    // 缺键 = 启用（后端 normalize 已补齐，但前端仍按"未给即启用"兜底，
    // 保证接口未返回该表时不会把全部上游显示成关闭）。
    const on = enabled == null || enabled[be] === undefined ? true : !!enabled[be];
    checkinBackendsState[be] = on;
    const lb = document.createElement('label');
    lb.className = 'realm-opt';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = on;
    cb.onchange = () => { checkinBackendsState[be] = cb.checked; };
    lb.append(cb, document.createTextNode(' ' + (BACKEND_LABEL_CFG[be] || be)));
    frag.append(lb);
  }
  box.replaceChildren(frag);
}

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
    // 分上游签到：上游列表由后端给（账号池里实际存在的），勾选状态取自
    // schedule.checkin_backends（缺键 = 启用，见 renderCheckinBackends）。
    renderCheckinBackends(
      d.checkin_backends_available || [],
      dig(cfgLoaded, ['schedule', 'checkin_backends']) || null);
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
  // 分上游签到勾选：整张表照发。不下发会让"用户取消勾选"这个动作丢失——
  // 后端见不到键就按缺省启用处理，保存后又被悄悄勾回来。
  if (checkinBackendsState && Object.keys(checkinBackendsState).length) {
    put(out, ['schedule', 'checkin_backends'], { ...checkinBackendsState });
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
    // Trae 的两个上游特有字段（IDE 版本 / 签到 UA）落在独立的 trae.json 里，
    // 不能塞进 config.json。配置页既然把它们画出来了，保存时必须一并提交，
    // 否则用户改完"保存成功"却没生效——这是最难排查的一类假成功。
    const traeBody = collectTraeConfig();
    if (Object.keys(traeBody).length) {
      try { await api('trae_config', { method: 'POST', body: JSON.stringify(traeBody) }); }
      catch (e) { toast('Trae 配置保存失败：' + e.message, 'err'); }
    }
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

function collectTraeConfig() {
  // 账号列表**不在这里提交**：Trae 账号与 CodeBuddy 同在账号池，增删/启停由池
  // 管理。省略 accounts 字段（而非传空数组）才会走后端的「保留全部账号」分支；
  // 传 [] 会删除全部账号的文件。
  //
  // 「上游接入」页删除后，这里只剩搬到配置页的两个字段（IDE 版本 / 签到 UA）。
  // 对话 base 不再暴露配置：它是上游路径，一个上游不该有可配的路径。
  const ideVer = ($('traeIdeVersion') ? $('traeIdeVersion').value || '' : '').trim();
  const ua = ($('traeUserAgent') ? $('traeUserAgent').value || '' : '').trim();
  const out = {};
  if (ideVer) out.ide_version = ideVer;
  if (ua) out.checkin_user_agent = ua;
  return out;
}
/* parseHours 解析 "9, 21" → [9,21]（非法项丢弃；空串 → []）。 */
function parseHours(s) {
  return String(s || '').split(',').map(x => parseInt(x.trim(), 10))
    .filter(n => Number.isInteger(n) && n >= 0 && n <= 23);
}
/* ── WorkBuddy 上游（codebuddy.json）─────────────────────────────────
   上游专有配置各自独立成文件，本区块只读写 CodeBuddy 那一份。
   面板按文件分区展示，字段归属不会看错——这是「一上游一文件」的直接收益。

   这里不做"空串 = 清除"的特殊处理：user_agent 空串本来就是有意义的
   （= 用内置三段式默认），照发即可。 */
async function loadCodeBuddyConfig() {
  try {
    const d = await api('codebuddy_config');
    const c = d.config || {};
    const set = (id, v) => { const el = $(id); if (el) el.value = v == null ? '' : v; };
    const chk = (id, v) => { const el = $(id); if (el) el.checked = !!v; };
    set('cbUserAgent', c.user_agent || '');
    set('cbClientVersion', c.client_version || '');
    set('cbCliVersion', c.cli_version || '');
    set('cbChatBase', (c.cn || {}).chat_base || '');
    set('cbGlobalChatBase', (c.global || {}).chat_base || '');
    chk('cbGlobalEnabled', (c.global || {}).enabled);
    chk('cbSanitize', c.sanitize_blacklist_fingerprints);
  } catch (e) { /* 后端未提供该接口时静默 */ }
}

function collectCodeBuddyConfig() {
  const num = id => {
    const el = $(id);
    if (!el) return undefined;
    const v = el.value.trim();
    return v === '' ? undefined : Number(v);
  };
  const str = id => { const el = $(id); return el ? el.value.trim() : undefined; };
  const bool = id => { const el = $(id); return el ? el.checked : undefined; };

  const out = {};
  const cnBase = str('cbChatBase');
  if (cnBase !== undefined) out.cn = { chat_base: cnBase };
  const gl = {};
  const glBase = str('cbGlobalChatBase');
  if (glBase !== undefined) gl.chat_base = glBase;
  if (bool('cbGlobalEnabled') !== undefined) gl.enabled = bool('cbGlobalEnabled');
  if (Object.keys(gl).length) out.global = gl;
  for (const [k, id] of [
    ['user_agent', 'cbUserAgent'], ['client_version', 'cbClientVersion'],
    ['cli_version', 'cbCliVersion'],
  ]) {
    const v = str(id);
    if (v !== undefined) out[k] = v;
  }
  const san = bool('cbSanitize');
  if (san !== undefined) out.sanitize_blacklist_fingerprints = san;
  return out;
}

/* loadTraeConfig 把 Trae 上游的两个可配项填进配置页（IDE 版本 / 签到 UA）。
   上游接入页删除后，其余字段（对话 base / 启用开关 / 签到排程）都不再暴露：
   - base 是上游路径，不该可配；
   - 启用开关随「平权」移除——加了账号就启用；
   - 签到排程并入 config.json 的 schedule（总开关 + 按上游勾选）。 */
async function loadTraeConfig() {
  try {
    const d = await api('trae_config');
    const c = d.config || {};
    if ($('traeIdeVersion')) $('traeIdeVersion').value = c.ide_version || '';
    if ($('traeUserAgent')) $('traeUserAgent').value = c.checkin_user_agent || '';
  } catch (e) { /* 后端未提供该接口时静默 */ }
}
/* syncTraeNicknames 从上游 GetUserInfo 拉昵称（账号池页与 Trae 页共用）。 */
async function syncTraeNicknames(btn, idleText) {
  if (btn) { btn.disabled = true; btn.textContent = '同步中…'; }
  try {
    const d = await api('trae/sync_profiles', { method: 'POST' });
    toast('昵称同步完成：更新 ' + (d.updated || 0) + ' 个' +
      (d.failed ? '，失败 ' + d.failed + ' 个' : ''), d.failed ? 'warn' : 'ok');
    loadTraeConfig();
    if (typeof loadOverview === 'function') loadOverview(true);
  } catch (e) { toast('同步失败：' + e.message, 'err'); }
  finally { if (btn) { btn.disabled = false; btn.textContent = idleText; } }
}
/* showTraeCheckinStatus 展示最近一次 Trae 批量签到结果（两处入口共用）。 */
async function showTraeCheckinStatus() {
  try {
    const d = await api('trae/checkin_status');
    const rs = d.results || [];
    if (!rs.length) { toast('还没有签到记录', ''); return; }
    renderTraeCheckinNote(rs, d.last_run);
  } catch (e) { toast('查询失败：' + e.message, 'err'); }
}
if ($('btnTraeSyncProfiles')) $('btnTraeSyncProfiles').onclick = () =>
  syncTraeNicknames($('btnTraeSyncProfiles'), 'Trae 昵称');
if ($('btnTraeCheckinStatus')) $('btnTraeCheckinStatus').onclick = showTraeCheckinStatus;
/* traeLoginBody 收集登录发起参数：storage.json 优先（它自带设备号）。
   Trae 登录入口已并入「添加账号」弹窗，这里读的是弹窗里的字段。 */
function traeLoginBody() {
  const body = {};
  const storage = ($('addTraeStorage') ? $('addTraeStorage').value || '' : '').trim();
  const devId = ($('addTraeDeviceId') ? $('addTraeDeviceId').value || '' : '').trim();
  if (storage) body.storage_json = storage;
  if (devId) body.device_id = devId;
  return body;
}
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
  const line = (lastRun ? '[' + lastRun + '] ' : '') + parts.join('　');
  // 上游接入页删除后没有常驻的展示位了：签到结果改用 toast 弹出。
  // 账号池页的「Trae 签到结果」按钮仍会调到这里，不能让它抛空引用。
  const el = $('traeCheckinNote');
  if (el) el.textContent = line;
  else toast(line, '');
}

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
