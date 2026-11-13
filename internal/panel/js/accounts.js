/* ── 账号池 ───────────────────────────────────────────────────────── */
/* 表格重渲染的防抖：轮询每 5 秒拿一次 overview，绝大多数轮次的账号状态没有变化。
   但 innerHTML 整体替换会让浏览器丢弃并重建整棵子树——移动端每行是 12 列 grid
   卡片（mobile.css），重建一次就是整屏闪一下；桌面端也会丢失 hover / 焦点 / 文本选区。
   故先比对渲染结果字符串，一致就完全不碰 DOM。 */
const lastTableHTML = new Map();
function setTableHTML(el, html) {
  if (!el) return;
  const key = el.id || el;
  if (lastTableHTML.get(key) === html) return; // 内容没变 → 不重建，不闪
  lastTableHTML.set(key, html);
  el.innerHTML = html;
}

/* BACKEND_LABEL 上游标识 → 展示名。
   多上游合池后，账号表按上游分组：调度虽然是统一的，但凭证形态、积分口径、
   可用的运维动作都不一样，混在一起排会让人分不清哪一行能用哪些按钮。 */
const BACKEND_LABEL = { codebuddy: 'WorkBuddy', trae: 'Trae' };

/* groupByBackend 按上游分组，保持组内原有顺序。
   未知 backend（将来新增的）归到「其他」，不会被吞掉。 */
function groupByBackend(list) {
  const order = [], groups = {};
  for (const s of list) {
    const k = s.backend || 'codebuddy';
    if (!groups[k]) { groups[k] = []; order.push(k); }
    groups[k].push(s);
  }
  return order.map(k => ({ backend: k, rows: groups[k] }));
}

/* backendSectionRow 组头行：上游名 + 该组账号数与可用数。 */
function backendSectionRow(backend, rows) {
  const label = BACKEND_LABEL[backend] || backend;
  const usable = rows.filter(s => !s.disabled && !s.paused &&
    !(s.cool_remaining_sec > 0)).length;
  return '<tr class="acc-group"><td colspan="9">' +
    '<span class="acc-group-name">' + esc(label) + '</span>' +
    '<span class="acc-group-meta">' + rows.length + ' 个账号 · ' + usable + ' 可用</span>' +
    '</td></tr>';
}

function renderAccounts(list) {
  const tb = $('accBody');
  if (!list.length) {
    setTableHTML(tb, '<tr><td colspan="9"><div class="empty"><div class="big">账号池是空的</div>点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号</div></td></tr>');
    return;
  }
  // 有总额度（credits_total）→ 进度条按自身 剩余/总额 百分比；旧数据无总额 → 退回池内最高=100%
  const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
  const groups = groupByBackend(list);
  // 只有一个上游时不出组头：单组的分组标签是纯噪音，还会把表格撑高一行。
  const showGroups = groups.length > 1;
  const html = groups.map(g => {
    const head = showGroups ? backendSectionRow(g.backend, g.rows) : '';
    return head + g.rows.map(s => {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
    let cls = '', tag;
    if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">已禁用</span>'; }
    else if (s.paused) { cls = 'off'; tag = '<span class="tag warn">已暂停选号</span>'; }
    else if (cool > 0) {
      cls = 'cool';
      const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? '熔断'
        : (dg > (s.cool_remaining_sec || 0) ? '连败降权' : (s.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'));
      tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
    } else tag = '<span class="tag ok">可用</span>' + (s.in_flight ? '' : '');
    const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
    const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
    const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
    // 企业版不限量：上游 limitNum == -1，网关以 credits_total=-1 透出（见 upstream
    // enterpriseUnlimitedTotal）。此时剩余额度不参与展示，直接标「不限」。
    const unlimited = s.credits_total === -1;
    const cred = unlimited ? '不限'
      : (s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits)));
    const pct = unlimited ? 100
      : (s.credits_total > 0
        ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
        : Math.round((s.credits || 0) / maxCred * 100));
    // 成本台账 tooltip（model_costs）：每模型实测单价（≤0 = 实测免费），运维据此
    // 看「为什么总选它」——免费号垄断 / 单价排序一眼可见。
    let credTip;
    if (unlimited) credTip = '企业版不限量（上游 limitNum=-1）';
    else if (s.credits_total > 0) {
      credTip = (s.enterprise ? '企业版剩余额度 ' : '剩余 ')
        + s.credits + ' / ' + (s.enterprise ? '分配 ' : '总额 ') + s.credits_total + '（' + pct + '%）';
    } else credTip = '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter(c => c.model);
    if (costs.length) {
      credTip += '\n实测单价（credits/1K）：\n' + costs.map(c =>
        '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    const frozen = s.disabled || cool > 0;
    const tu = s.token_usage || {};
    const req = tu.request_count || 0;
    const totalTok = formatTokenCount(tu.total_tokens);
    const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
    const latency = formatLatency(tu.last_latency_ms);
    const rate = formatRate(tu.last_tokens_per_second);
    const usageTitle = '最近一次：' + req + ' 次 / ' + totalTok + ' / 延迟 ' + latency + ' / ' + rate;
    return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
      '<td class="mark" aria-hidden="true"><i></i></td>' +
      '<td class="who"><div class="nm">' + remarkName(s.uid, s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">未命名</span>') + (s.realm === 'global' ? ' <span class="realm-tag">国际版</span>' : '') + (s.enterprise ? ' <span class="realm-tag">企业版</span>' : '') +
        (s.backend === 'trae' ? ' <span class="realm-tag">Trae</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
      '<td>' + tag + note + rateLimits + '</td>' +
      '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
      '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
      '<td class="num">' + (s.in_flight || 0) + '</td>' +
      '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
        '<span class="usage-item usage-count"><b>' + req + '</b><em>次</em></span>' +
        '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
        '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
        '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
      '</span></td>' +
      '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
      '<td class="acts">' +
        // CodeBuddy 专属的三个按钮：签到 / 余额 / 任务。
        // 不上 Trae 的原因：它的签到走面板「Trae 签到」区块（限额按设备算，
        // 与账号维度不同），余额是 IDE/Work 双轨，任务体系完全不存在。
        // 企业版同样不渲染（上游不支持个人成长体系）。
        ((s.enterprise || s.backend === 'trae') ? '' :
          '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="今日已签到；点击可重新签到并刷新余额"' : '') + '>' + (s.checkin_done ? '已签' : '签到') + '</button>') +
        (s.backend === 'trae' ? '' :
          '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '"' + (s.enterprise ? ' title="刷新企业版已分配额度（上游 get-enterprise-user-usage）"' : '') + '>' + (s.enterprise ? '额度' : '余额') + '</button>') +
        ((s.enterprise || s.backend === 'trae') ? '' :
          '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">任务</button>') +
        (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">解冻</button>'
                : (s.paused ? '<button class="xs primary" data-a="resume" data-u="' + esc(s.uid) + '">恢复选号</button>'
                            : '<button class="xs ghost" data-a="pause" data-u="' + esc(s.uid) + '" title="' + (s.enterprise ? '退出选号，但照常保活 / 刷新额度' : '退出选号，但照常签到 / 活跃上报 / 保活 / 刷新余额') + '">暂停选号</button>')) +
        (s.disabled ? '' : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">禁用</button>') +
        '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">移除</button>' +
      '</td></tr>';
    }).join('');
  }).join('');
  setTableHTML(tb, html);
}

// renderModelLocks 模型锁池：哪些模型不能用、锁了几个号、还要锁多久。
// 后端 model_locks 已按「整池不可用 → 没号可用 → 部分限流」排好序，这里只做展示。
function renderModelLocks(rows) {
  const tb = $('mlBody');
  if (!tb) return;
  const note = $('mlNote');
  if (!rows || !rows.length) {
    setTableHTML(tb, '<tr><td colspan="8"><div class="empty">当前没有模型级限流 —— 所有模型均可选</div></td></tr>');
    if (note) note.textContent = '';
    return;
  }
  const STATE = { locked: ['bad', '整池不可用'], starved: ['warn', '没号可用'], partial: ['warn', '部分限流'] };
  const left = iso => {
    const ms = parseAPITime(iso);
    return ms ? dur(Math.max(0, Math.round((ms - Date.now()) / 1000))) : '—';
  };
  setTableHTML(tb, rows.map(r => {
    const st = STATE[r.state] || ['mute', r.state || '—'];
    const realm = r.realm === 'global' ? '国际版' : '国内版';
    return '<tr>' +
      '<td>' + esc(r.model) + '</td>' +
      '<td><span class="realm-tag">' + realm + '</span></td>' +
      '<td><span class="tag ' + st[0] + '">' + st[1] + '</span></td>' +
      '<td class="num">' + (r.servable || 0) + ' / ' + (r.total || 0) + '</td>' +
      '<td class="num">' + (r.locked || 0) + '</td>' +
      '<td class="num">' + left(r.unlock_at || r.fully_unlock_at) + '</td>' +
      '<td class="num">' + left(r.fully_unlock_at) + '</td>' +
      '<td>' + (r.reason ? '<div class="note">' + esc(r.reason) + '</div>' : '—') + '</td>' +
      '</tr>';
  }).join(''));
  if (note) {
    const bad = rows.filter(r => r.state === 'locked' || r.state === 'starved').length;
    note.textContent = bad ? bad + ' 个模型整池不可用' : rows.length + ' 个模型部分限流';
  }
}

async function loadOverview(quiet) {
  try {
    const d = await api('overview');
    overviewData = d;
    $('sTotal').textContent = d.total;
    $('sHealthy').textContent = d.healthy;
    $('sCooling').textContent = d.cooling;
    $('sDisabled').textContent = d.disabled;
    // 暂停选号单列（issue #125）：它只关选号、照常签到保活，与禁用是两种状态。
    if ($('sPaused')) $('sPaused').textContent = d.paused == null ? '-' : d.paused;
    const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
  const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
  $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
    $('sSticky').textContent = d.sticky_sessions;
    $('navSub').textContent = 'v' + d.version;
    $('navVer').textContent = 'v' + d.version;
    $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存';
    $('navState').textContent = d.healthy > 0 ? '服务正常' : (d.total ? '无可用账号' : '待添加账号');
    const p = $('navPulse');
    p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
    $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : '';
    const up = Math.floor(d.uptime_sec);
    $('subMeta').textContent = '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') + Math.floor(up % 86400 / 3600) + ' 时 ' + Math.floor(up % 3600 / 60) + ' 分';
    renderAccounts(d.accounts || []);
    renderModelLocks(d.model_locks);
    // 到期卡片跟随首页统计同步刷新（用同一份响应里的按日批次，不额外发请求）。
    // 2 分钟内的实时明细最新且更全，保留；过期后由 state 快照接管，避免点过一次
    // 「检查」之后卡片永远停在那一刻的旧数据。
    const liveFresh = lastPackages && (Date.now() - lastPackagesAt) < EXP_FRESH_MS;
    if ($('expBox') && !liveFresh) renderExpirySnapshot(d);
  } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', async ev => {
  const b = ev.target.closest('button[data-a]');
  if (!b) return;
  const u = b.dataset.u, a = b.dataset.a;
  if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
  if (a === 'disable' && !confirm('禁用后该账号不再参与选号（保号任务默认也跳过），需手动解冻才能恢复。若只是想临时让位、仍要保号，请改用「暂停选号」。确认禁用？')) return;
  b.disabled = true;
  try {
    if (a === 'checkin') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
      toast('签到完成' + (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
    } else if (a === 'balance') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
      toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
    } else if (a === 'revive') {
      await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
      toast('已解冻', 'ok');
    } else if (a === 'disable') {
      await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
      toast('已禁用', 'ok');
    } else if (a === 'pause') {
      await api('accounts/' + encodeURIComponent(u) + '/pause', { method: 'POST' });
      toast('已暂停选号（签到 / 保活照常）', 'ok');
    } else if (a === 'resume') {
      await api('accounts/' + encodeURIComponent(u) + '/resume', { method: 'POST' });
      toast('已恢复选号', 'ok');
    } else if (a === 'tasks') {
      openTasks(u);
    } else if (a === 'remove') {
      const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
      toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
    }
  } catch (e) { toast(e.message, 'err'); }
  finally { b.disabled = false; loadOverview(true); }
});

$('btnCheckinAll').onclick = async () => {
  try { await api('checkin_all', { method: 'POST' }); toast('全部签到已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
  try { await api('keepalive_all', { method: 'POST' }); toast('全部保活已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
  try { await api('travel_all', { method: 'POST' }); toast('旅行巡检已开始（含领养链路），结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};
/* 能量盲盒：两步确认。第一下 dry-run 同步返回逐账号结果（能量 / 可开次数 / 不能开的原因），
   直接弹表格给用户看；确认后才真正开盒，开完再弹一次结果（含开出的奖品）。
   结果不落日志——用户点按钮就该当场看到，而不是自己去翻「运行日志」。 */
$('btnBlindboxAll').onclick = async () => {
  const btn = $('btnBlindboxAll');
  btn.disabled = true;
  const restore = () => { btn.disabled = false; };
  try {
    toast('正在查询各账号能量配额…', 'ok');
    const d = await api('blindbox_all', { method: 'POST' });
    restore();
    showBlindboxDialog(d, false);
  } catch (e) { restore(); toast(e.message, 'err'); }
};

/* renderBlindboxRows 逐账号一行：昵称 / 状态 / 能量 / 可开次数 / 结论 */
function renderBlindboxRows(d) {
  const rows = d.results || [];
  if (!rows.length) return '<div class="state">没有可用账号</div>';
  return '<div class="tbl-wrap"><table class="acc md"><thead><tr>' +
    '<th>账号</th><th>旅行状态</th><th>能量</th>' +
    '<th>可开</th><th>结论</th></tr></thead><tbody>' +
    rows.map(r => {
      const stateMap = { idle: '空闲', traveling: '旅行中', arrived: '已到站', unknown: '未知' };
      const st = stateMap[r.travel_state] || r.travel_state || '—';
      const canOpen = r.will_open > 0;
      const cls = r.error ? 'c-err' : canOpen ? 'c-ok' : '';
      const tail = r.error ? esc(r.error)
        : r.opened ? '已开 ' + r.will_open + ' 次 → ' + esc(r.prize || '')
        : esc(r.decision || '');
      return '<tr>' +
        '<td>' + esc(r.nickname || r.uid.slice(0, 8)) + '</td>' +
        '<td>' + esc(st) + '</td>' +
        '<td>' + (r.energy || 0) + '</td>' +
        '<td>' + (r.will_open || 0) + '</td>' +
        '<td class="' + cls + '">' + tail + '</td>' +
        '</tr>';
    }).join('') + '</tbody></table></div>';
}

/* showBlindboxDialog 弹出盲盒结果；dry-run 时附带「确认开盒」按钮 */
function showBlindboxDialog(d, applied) {
  const canOpen = d.can_open || 0;
  const title = applied
    ? '已开盒 — ' + (d.opened || 0) + ' / ' + (d.total || 0) + ' 个账号成功'
    : '能量盲盒配额 — ' + canOpen + ' / ' + (d.total || 0) + ' 个账号可开';
  const veil = document.createElement('div');
  veil.className = 'veil';
  // 必须用 .on 类而不是 style.display='flex'：居中（align-items/justify-content）、
  // 内边距、移动端覆盖（mobile.css 的 .veil.on）全挂在这条规则上。直接设 display
  // 会让弹窗贴左上角、且 max-height 失效撑满整屏。
  veil.classList.add('on');
  veil.innerHTML = '<div class="dlg bb-dlg">' +
    '<header><h3>' + esc(title) + '</h3>' +
    '<div class="hint">' + (applied
      ? '开盒结果见下表。实物奖需自行到成长中心填写收件地址。'
      : '这是 dry-run，未消耗任何能量。确认无误后再点「确认开盒」。') + '</div></header>' +
    '<div class="body">' + renderBlindboxRows(d) + '</div>' +
    '<footer><span class="grow"></span>' +
    (applied || canOpen <= 0
      ? '<button data-act="close">关闭</button>'
      : '<button data-act="close">取消</button><button class="primary" data-act="apply">确认开盒（消耗能量，不可逆）</button>') +
    '</footer></div>';
  document.body.appendChild(veil);
  const close = () => veil.remove();
  veil.addEventListener('click', async (ev) => {
    const t = ev.target.closest('[data-act]');
    if (!t) { if (ev.target === veil) close(); return; }
    if (t.dataset.act === 'close') return close();
    if (t.dataset.act === 'apply') {
      t.disabled = true;
      t.textContent = '开盒中…';
      try {
        const r = await api('blindbox_all?apply=1', { method: 'POST' });
        close();
        showBlindboxDialog(r, true);
        toast('开盒完成：' + (r.opened || 0) + ' 个账号成功', 'ok');
      } catch (e) {
        t.disabled = false;
        t.textContent = '确认开盒（消耗能量，不可逆）';
        toast(e.message, 'err');
      }
    }
  });
}
$('btnActivityAll').onclick = async () => {
  try { await api('activity_all', { method: 'POST' }); toast('活跃上报已开始，结果见日志', 'ok'); }
  catch (e) { toast(e.message, 'err'); }
};

/* ── 添加账号 ─────────────────────────────────────────────────────── */
function openAdd() {
  $('addVeil').classList.add('on');
  // 重置到登录标签
  switchAddTab('login');
  $('addPick').hidden = false;
  $('addLoad').hidden = true; $('addReady').hidden = true;
  $('addDone').hidden = true; $('addErr').hidden = true;
  $('importDone').hidden = true; $('importErr').hidden = true;
  $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
  $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
  stopPoll();
}
function switchAddTab(tab) {
  document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
  $('addTabLogin').hidden = tab !== 'login';
  $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
  b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
  const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
  $('btnStartLogin').disabled = true;
  $('addLoad').hidden = false; $('addErr').hidden = true;
  api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
    loginState = r.state;
    $('addUrl').textContent = r.url;
    $('addPick').hidden = true; // 选域锁定（会话已按该域发起）
    $('addLoad').hidden = true; $('addReady').hidden = false;
    $('btnStartLogin').hidden = true;
    $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
    loginTimer = setInterval(pollLogin, 3000);
  }).catch(e => {
    $('addLoad').hidden = true;
    $('btnStartLogin').disabled = false;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message;
  });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
  if (!loginState) return;
  try {
    const r = await api('login/poll?state=' + encodeURIComponent(loginState));
    if (r.done) {
      stopPoll();
      $('addReady').hidden = true;
      $('addDone').hidden = false;
      $('addDone').textContent = '已添加 ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（国际版）' : '') + (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，账号已载入池中';
      setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
    }
  } catch (e) {
    stopPoll();
    $('addReady').hidden = true;
    $('addErr').hidden = false;
    $('addErr').textContent = e.message + '（关闭后重新添加）';
  }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
  .then(() => toast('链接已复制', 'ok'), () => toast('复制失败，请手动选择复制', 'err'));
$('importFile').onchange = async () => {
  const file = $('importFile').files[0];
  if (!file) return;
  $('importDone').hidden = true; $('importErr').hidden = true;
  const fd = new FormData();
  fd.append('file', file);
  const h = {};
  const k = localStorage.getItem(LS_KEY);
  if (k) h['Authorization'] = 'Bearer ' + k;
  try {
    const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
    const d = await r.json();
    if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
    $('importDone').hidden = false;
    $('importDone').textContent = '导入完成：成功 ' + d.imported + ' 个' + (d.skipped ? '，跳过 ' + d.skipped + ' 个' : '');
    if (d.errors && d.errors.length) {
      console.warn('import errors:', d.errors);
    }
    loadOverview(true);
  } catch (e) {
    $('importErr').hidden = false;
    $('importErr').textContent = '导入失败：' + e.message;
  }
  $('importFile').value = '';
};

/* ── 官方补偿（issue #189）─────────────────────────────────────────
   「查状态 → 待领才领」。

   取数与呈现刻意分开：
     - 取数只有手动触发（进页面一次 + 「检查」/「领取」后）。补偿状态没有
       overview 快照可吃，必须逐号打上游，挂进 5s 轮询就是每 5 秒轰炸上游。
     - 呈现走 compCache 的本地重渲染（refreshVisible 每轮调一次），只为让
       倒计时走动 —— 不发任何请求。
   之前这里有个「5 分钟新鲜窗口」分支，但它唯一的非强制调用点是 start()，
   而那里 compCache 必为 null，所以该分支永远不会命中：注释宣称「防轰炸」，
   实际是「根本不刷新」。现已删除，别再加回来。

   声明位置在本段（轮询区之前）是必需的：start() 会调用 loadCompensation()，
   若这些 let 写在 start() 之后，顶层求值会在 TDZ 上炸——
   TestAppJSTopLevelSmoke 正是为这类问题设的闸门。 */
let compCache = null;
let compInFlight = null;   // 在途请求的 promise（非布尔标志：布尔标志配 force 刷新会静默空转）

// compPending 挑出「可领未领」的账号。
// 纯函数：判据独立于渲染，node 侧单测单独钉住。
// 判据与后端 upstream.CompensationStatus.Pending() 对齐——两端各留一份实现是
// 有意为之（前端要在渲染前先算总数），但**必须同步**，改一处就要改另一处。
function compPending(rows) {
  return (rows || []).filter(r => r && !r.skip && !r.error && r.status &&
    r.status.active && !r.status.claimed && (r.status.credit_num || 0) > 0);
}

// compErrored 挑出「查询失败」的账号。
// 单独拎出来，是因为它们**必须显形**：上游 5xx / token 过期时所有账号都会落到
// 这里，若和「没有补偿」一起被过滤掉，卡片就会整块消失，用户看到的是「什么都没有」
// ——而真相是「一个都没查成」。这正是本 PR 要消灭的静默失败。
function compErrored(rows) {
  return (rows || []).filter(r => r && r.error);
}

// compTotalPending 待领总额。
function compTotalPending(rows) {
  return compPending(rows).reduce((s, r) => s + (r.status.credit_num || 0), 0);
}

// compDeadlineText 领取截止倒计时文案。
// 上游 end_time 形如 "2026-10-22 23:59:59" 且**不带时区**，按浏览器本地时区解析
// （与后端积分到期的处理同口径）。urgent=true 表示距截止 <=48h，前端据此染色告警。
function compDeadlineText(endTime, now) {
  const raw = String(endTime == null ? '' : endTime).trim();
  if (!raw) return { text: '无截止信息', urgent: false };
  const t = new Date(raw.replace(' ', 'T')).getTime();
  if (!isFinite(t)) return { text: raw, urgent: false };
  const ms = t - (now || Date.now());
  if (ms <= 0) return { text: '已截止', urgent: true };
  const d = Math.floor(ms / 86400000);
  const h = Math.floor(ms % 86400000 / 3600000);
  const text = d > 0 ? (d + ' 天 ' + h + ' 小时') : (Math.max(1, h) + ' 小时');
  return { text: '剩 ' + text, urgent: ms <= 2 * 86400000 };
}

function renderCompensation(d) {
  const rows = (d && d.accounts) || [];
  const box = $('compBox');
  if (!box) return;
  const active = rows.filter(r => r && !r.error && r.status && r.status.active);
  const failed = compErrored(rows);
  // 「有失败就必须显形」——哪怕一条有效数据都没有。
  if (!active.length && !failed.length) { box.hidden = true; return; }
  box.hidden = false;

  const now = Date.now();
  const okRows = active.map(r => {
    const s = r.status || {};
    const done = !!s.claimed;
    const dl = compDeadlineText(s.end_time, now);
    const name = esc(r.nickname || String(r.uid).slice(0, 8));
    return '<div class="comp-row"><span>' + name + '</span>' +
      (done
        ? '<span class="tag ok">已领取</span>'
        : '<span class="tag warn">待领取</span>' +
          '<span class="tag' + (dl.urgent ? ' bad' : ' mute') + '">' + esc(dl.text) + '</span>') +
      '<span class="tag mute">' + esc(fmtTok(s.credit_num || 0)) + ' 积分</span>' +
      (done ? '' : '<button class="xs" data-comp-uid="' + esc(r.uid) + '">领取</button>') +
      '</div>';
  }).join('');
  // err 文本来自上游响应体，是外部可控字符串，必须 esc。
  const errRows = failed.map(r =>
    '<div class="comp-row"><span>' + esc(r.nickname || String(r.uid).slice(0, 8)) + '</span>' +
    '<span class="tag bad">查询失败</span>' +
    '<span class="tag mute">' + esc(String(r.error).slice(0, 120)) + '</span></div>'
  ).join('');

  $('compList').innerHTML = okRows + errRows;

  const n = compPending(rows).length;
  let note = n ? (n + ' 个账号待领 · 共 ' + fmtTok(compTotalPending(rows)) + ' 积分') : '当前无待领补偿';
  // 混合态下也要把失败数说出来，否则 note 仍读作「没事」。
  if (failed.length) note += ' · ' + failed.length + ' 个账号查询失败';
  $('compNote').textContent = note;
}

// renderCachedCompensation 从缓存重渲染（不发请求），让倒计时走动。
// 由 refreshVisible 每轮调用；无缓存（还没查过）时什么都不做。
function renderCachedCompensation() {
  if (compCache) renderCompensation(compCache);
}

async function loadCompensation() {
  // 已有在途请求就等它跑完，而不是静默返回——领取后紧接着的刷新必须能看到新状态，
  // 否则用户点了「领取」但列表纹丝不动。
  if (compInFlight) { await compInFlight; return; }
  compInFlight = (async () => {
    try {
      const d = await api('compensation');
      compCache = d;
      renderCompensation(d);
    } catch (e) {
      const box = $('compBox');
      if (box) box.hidden = false;
      if ($('compList')) $('compList').innerHTML =
        '<div class="empty">查询失败：' + esc(e.message) + '</div>';
      if ($('compNote')) $('compNote').textContent = '';
    }
  })();
  try {
    await compInFlight;
  } finally {
    compInFlight = null;   // 必须 finally：否则一次异常就让此后所有刷新永久失效
  }
}

// 领取按钮走事件委托：列表每次重渲染都整块换 innerHTML，绑在具体节点上必丢。
if ($('compList')) $('compList').addEventListener('click', async e => {
  const b = e.target.closest('[data-comp-uid]');
  if (!b) return;
  b.disabled = true;
  try {
    const r = await api('accounts/' + encodeURIComponent(b.dataset.compUid) + '/compensation', { method: 'POST' });
    const res = r.result || {};
    if (res.claimed) toast('已领取 ' + fmtTok(res.credit || 0) + ' 积分', 'ok');
    else if (res.error) toast('查询失败，未能确认是否有补偿', 'err');
    else if (res.skip) toast(res.skip_msg || '无需领取', '');
    else toast('领取失败：未知结果', 'err');
    await loadCompensation();
    loadOverview(true);
  } catch (err) {
    toast('领取失败：' + err.message, 'err');
  }
});

if ($('btnCompRefresh')) $('btnCompRefresh').onclick = () => loadCompensation();

if ($('btnCompClaimAll')) $('btnCompClaimAll').onclick = async () => {
  const b = $('btnCompClaimAll');
  b.disabled = true;
  try {
    const r = await api('compensation_all', { method: 'POST' });
    const s = r.summary || {};
    // 顺序要紧：先看有没有账号没拿到结论，再看有没有领到。
    // 只按 claimed 判定的话，「全部查询失败」会被说成「当前没有待领补偿」，
    // 把故障说成了「没活动」——比不说更糟。
    if (s.errored) {
      toast(s.errored + ' 个账号查询失败，未能确认是否有补偿', 'err');
    } else if (s.claimed) {
      toast('已领取 ' + s.claimed + ' 个账号，共 ' + fmtTok(s.credit || 0) + ' 积分', 'ok');
    } else {
      toast('当前没有待领补偿', '');
    }
    await loadCompensation();
    loadOverview(true);
  } catch (e) {
    toast('领取失败：' + e.message, 'err');
  } finally {
    b.disabled = false;
  }
};
