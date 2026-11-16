/* ── 模型 ─────────────────────────────────────────────────────────── */
/* 实测上限标注：scripts/probe_max_tokens.py --panel-out 写入探测结果，
   /panel/api/model_probes 只读透传。探测键带域前缀（cn:glm-5.2），模型表
   显示裸名，按「精确命中或 :后缀」关联。无数据时本列退回上游声称值。 */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
  if (!ts) return null;
  const t = new Date(String(ts).replace(' ', 'T'));
  const d = (Date.now() - t.getTime()) / 86400000;
  return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
  if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
  const tip = '声称 ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · 实测 ' + (pr.measured ? fmtK(pr.measured) : '?') +
    (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
  const days = probeDays(pr.tested_at);
  const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
  if (pr.verdict === 'clamped' && pr.measured) {
    if (pr.claimed && pr.measured < pr.claimed) {
      const x = pr.claimed / pr.measured;
      const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
      return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
        fmtK(pr.measured) + ' ⚠</span><div class="note">钳制 ' + xs + stale + '</div></td>';
    }
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
      (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
  }
  if (pr.verdict === 'at_least' && pr.measured)
    return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
  return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">未测出' + stale + '</div></td>';
}

/* rateCell 倍率列：牌价 vs 生效价。上游 credits 是牌价（转正后基准倍率），
   modelPromotions 给当前生效折扣（限时免费 factor=0 / 夜间五折 0.5 等）——
   WorkBuddy 客户端显示的正是生效价。有折扣：生效价大字 + 标签 + 划线牌价，
   悬停带时段说明；无 factor 只有标签（错峰类）：牌价 + 标签。 */
function rateCell(m) {
  const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
  if (m.promo_factor != null && m.promo_credits) {
    const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
    const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
    return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
  }
  if (m.promo_label) {
    return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
      ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
  }
  return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
  const tb = $('mdBody');
  tb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  const mb = $('mdMergedBody');
  if (mb) mb.innerHTML = '<tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>';
  try {
    // 探测、模型锁与最近请求都是本地可选增强：任一失败不影响模型目录本身。
    // 可用性标签不额外发推理请求，只用模型锁和最近真实请求结果合成。
    // 模型锁取自 overview（面板侧唯一暴露 Pool.ModelLockView() 的接口；
    // 此前这里请求的是面板并不存在的 status 路由，404 被 catch 吞掉后标签恒空）。
    const [d, pr, ov, rm] = await Promise.all([
      api('models'),
      api('model_probes').catch(() => ({})),
      api('overview').catch(() => ({})),
      api('request_metrics').catch(() => ({})),
    ]);
    mdAll = d.models || [];
    mdMerged = d.merged || [];
    mdProbes = pr.probes || {};
    mdLocks = Array.isArray(ov.model_locks) ? ov.model_locks : [];
    mdRecent = Array.isArray(rm.recent) ? rm.recent : [];
    if (!mdAll.length) {
      const empty = '<tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>';
      tb.innerHTML = empty;
      if (mb) mb.innerHTML = empty;
      $('mdCount').textContent = '';
      $('mdNote').textContent = '上游未返回模型';
      return;
    }
    // 探测键带域前缀（cn:glm-5.2），模型表显示裸名，按「精确命中或 :后缀」关联。
    const probeKeys = Object.keys(mdProbes);
    mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
    const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
    $('mdNote').textContent = mdMerged.length + ' 个模型（合并后）· 明细 ' + mdAll.length +
      ' 条' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
    renderModels();
  } catch (e) {
    mdAll = [];
    mdMerged = [];
    const err = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
    tb.innerHTML = err;
    if (mb) mb.innerHTML = err;
    $('mdCount').textContent = '';
  }
}

/* ── 模型筛选（按条件查询）───────────────────────────────────────────
   模型目录一次拉全（几十条），筛选与排序全部在前端完成：改条件零延迟，且不会
   因为调一次筛选就打一次上游——/panel/api/models 是直连上游的实时查询，很贵。
   条件之间是 AND；每个条件为空即不参与判定。 */
// mdRateValue 当前生效的积分倍率数值：优先促销价（限时免费 = 0），无倍率记为
// Infinity 排到最后（排序时"没有价格"不该冒充最便宜）。
function mdRateValue(m) {
  const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
  const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
  return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText 参与关键字搜索的字段（ID / 展示名 / 厂商 / 描述 / 标签）。
function mdSearchText(m) {
  return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
    .filter(Boolean).join(' ').toLowerCase();
}

// mdRealmOf 取模型 id 里的域段。
//
// 两种形态：CodeBuddy 是 "<realm>:<模型>"（cn:glm-5.2），Trae 是
// "trae:<realm>:<模型>"（trae:cn:doubao-…）。直接 startsWith 判断会把 Trae 的
// CN 模型当成非 CN 而筛掉，所以先剥掉上游前缀再取域。
function mdRealmOf(m) {
  let id = String((m && m.id) || '');
  if (id.startsWith('trae:')) id = id.slice('trae:'.length);
  const i = id.indexOf(':');
  return i > 0 ? id.slice(0, i) : '';
}

// mdUpstreamOf 模型所属上游。条目自带 upstream 字段（后端标注）；缺字段时按 ID
// 前缀回退——"trae:" 开头的必然是 Trae，其余走 CodeBuddy。回退不是冗余：老缓存
// 或第三方构造的条目可能没有该字段，按前缀判断比整列空白好。
function mdUpstreamOf(m) {
  const own = String((m && m.upstream) || '');
  if (own) return own;
  return String((m && m.id) || '').startsWith('trae:') ? 'trae' : 'codebuddy';
}

// mdUpstreamLabel 上游的展示名（徽标与筛选项共用同一套文案）。
//
// 直接用 accounts.js 里的 BACKEND_LABEL —— **全站唯一一份**。这里原先复制过
// 一份并写成 'CodeBuddy'，于是同一个上游在模型页叫 CodeBuddy、在账号池和配置页
// 叫 WorkBuddy。取值与为什么是 WorkBuddy 见 accounts.js 的定义处。
const MD_UPSTREAM_LABEL = BACKEND_LABEL;

// mdMatch 单个模型是否满足全部筛选条件。
function mdMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    const text = mdSearchText(m);
    // 空格分词后逐个匹配：多关键词是 AND，便于"cn 视觉"这类组合查询。
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  if (f.upstream && mdUpstreamOf(m) !== f.upstream) return false;
  if (f.realm && mdRealmOf(m) !== f.realm) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.cap === 'default' && !m.is_default) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  const factor = m.promo_factor == null ? null : Number(m.promo_factor);
  if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
  if (f.promo === 'free' && !(factor === 0)) return false;
  if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
  return true;
}

// mdSortList 按当前排序条件返回新数组（不改动入参，保持上游原始顺序可回溯）。
function mdSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
  else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
  return out;
}

// mdAvailabilityOf 合成 Global 模型当前可用性：模型锁优先于最近请求结果。
// 只返回有真实本地证据的状态；目录中存在但从未请求过的模型标为“待验证”，
// 避免把“上游列出了模型”误当成“当前账号有权限”。
function mdAvailabilityOf(m, locks, recent) {
  const id = String((m && m.id) || '');
  if (!id.startsWith('global:')) return null;
  locks = Array.isArray(locks) ? locks : mdLocks;
  recent = Array.isArray(recent) ? recent : mdRecent;
  const bare = id.slice('global:'.length);
  const lock = locks.find(r => r && r.realm === 'global' && r.model === bare);
  if (lock) {
    const reason = String(lock.reason || '').trim();
    const lower = reason.toLowerCase();
    if (lock.state === 'partial') {
      return { key: 'partial', label: '部分可用', cls: 'warn',
        tip: '仍有 ' + Number(lock.servable || 0) + ' 个 global 账号可服务；已锁 ' + Number(lock.locked || 0) + ' 个' + (reason ? '；' + reason : '') };
    }
    if (lock.state === 'starved') {
      return { key: 'starved', label: '账号暂不可用', cls: 'mute',
        tip: '没有账号能服务该模型，但并非该模型自身被锁；请检查账号冷却/在途占用' };
    }
    if (lower.startsWith('11158') || lower.includes('not allowed for user type')) {
      return { key: 'denied', label: '无权限', cls: 'bad',
        tip: '当前 global 账号类型不支持该模型' + (reason ? '；' + reason : '') };
    }
    if (lower.startsWith('11102') || lower.includes('service info not found')) {
      return { key: 'missing', label: '上游无此模型', cls: 'bad',
        tip: '上游目录残留，实际请求返回模型不存在' + (reason ? '；' + reason : '') };
    }
    return { key: 'limited', label: '限流/不可用', cls: 'warn',
      tip: '模型级冷却中' + (reason ? '；' + reason : '') };
  }
  const ev = recent.find(e => e && e.model === id);
  if (ev) {
    const status = Number(ev.status || 0);
    const when = ev.time ? new Date(ev.time).toLocaleString('zh-CN', { hour12: false }) : '';
    if (ev.outcome === 'success' || (status >= 200 && status < 300)) {
      return { key: 'ok', label: '可用', cls: 'ok',
        tip: '最近一次真实请求成功' + (when ? ' · ' + when : '') };
    }
    if (status === 429) {
      return { key: 'rate', label: '限流', cls: 'warn',
        tip: '最近一次请求被限流' + (when ? ' · ' + when : '') };
    }
    if (status >= 500) {
      return { key: 'upstream', label: '上游错误', cls: 'bad',
        tip: '最近一次请求上游错误 HTTP ' + status + (when ? ' · ' + when : '') };
    }
    return { key: 'failed', label: '最近失败', cls: 'bad',
      tip: '最近一次请求失败 HTTP ' + (status || '—') + (when ? ' · ' + when : '') };
  }
  return { key: 'unknown', label: '待验证', cls: 'mute',
    tip: '尚无模型锁或近期真实请求记录；目录存在不代表当前账号有权限' };
}

function mdAvailabilityTag(m) {
  const av = mdAvailabilityOf(m);
  if (!av) return '';
  return ' <span class="tag ' + av.cls + ' md-avail" title="' + esc(av.tip) + '">' + esc(av.label) + '</span>';
}

// mdProviderTag 单个「上游×域」徽标，文字是**可直填的调用前缀**（wb:cn / trae:global）。
//
// 为什么不写展示名（WorkBuddy·CN）：这一列是给运维看「这个模型从哪调」的，
// 用户真正要复制的是模型名前缀。展示名还得让他自己翻译回 `wb:` / `trae:`，
// 多一道心算。`wb` 是 CodeBuddy 注册的上游别名（backend/codebuddy 的
// Aliases()），客户端写 `wb:cn:<模型>` 与 `cn:<模型>` 等价。
//
// 域直接跟在后面（cn / global），与模型 ID 里的写法一致。
//
// **配色不带状态语义**：早先借 warn 表示 Trae、mute 表示 WorkBuddy，于是同一
// 张表里两个来源背景和字色都不一样，Trae 还看着像在告警。现在两者共用一套
// 中性外观（见 index.html 的 .md-prov），上游差异只由左侧色条表达。
function mdProviderTag(p, clickable) {
  const up = String((p && p.upstream) || '');
  const realm = String((p && p.realm) || 'cn');
  // 前缀用上游的**短别名**（后端注册过的那个），不是展示名。
  const prefix = mdUpstreamPrefix(up);
  // 未知上游也要有色条位（落到 accent），避免将来新增上游时没有视觉标识。
  const cls = 'md-prov up-' + (up === 'trae' ? 'trae' : 'codebuddy');
  const hidden = p && p.hidden === true;
  const bits = [];
  if (p && p.credits) bits.push('倍率 ' + p.credits);
  if (p && p.context_length) bits.push('上下文 ' + fmtK(p.context_length));
  if (p && p.max_output_tokens) bits.push('最大输出 ' + fmtK(p.max_output_tokens));
  if (hidden) bits.push('已被 models.hidden 隐藏（客户端调不到）');
  const tip = '调用名：' + String((p && p.id) || '') + (bits.length ? ' · ' + bits.join(' · ') : '');
  // 域单独一层淡色，让「哪一半是上游、哪一半是域」一眼分开。
  const text = esc(prefix) + '<span class="md-prov-realm">:' + esc(realm) + '</span>';
  const style = hidden ? ' style="opacity:.45;text-decoration:line-through"' : '';
  if (!clickable) return '<span class="tag ' + cls + '" title="' + esc(tip) + '"' + style + '>' + text + '</span>';
  return '<span class="tag ' + cls + '" data-up="' + esc(up) + '" data-realm="' + esc(realm) +
    '" title="' + esc(tip + '（点击筛选该来源）') + '" style="cursor:pointer' +
    (hidden ? ';opacity:.45;text-decoration:line-through' : '') + '">' + text + '</span>';
}

// mdUpstreamPrefix 上游的**模型名前缀**（客户端实际可写的短别名）。
//
// 与展示名（MD_UPSTREAM_LABEL）是两件事：展示名给人读，前缀给人填。
// CodeBuddy 的 `wb` 来自后端的 Aliases()（见 backend/codebuddy/backend.go）；
// Trae 没有别名，前缀就是 `trae`。新增上游时这里要跟着补。
function mdUpstreamPrefix(up) {
  return up === 'trae' ? 'trae' : 'wb';
}

// mdProviderTags 一个模型的全部来源徽标，**每个来源一行**。
//
// 并列成一个长串在来源多时（3~4 个）会挤成一条难以扫读的横排，且各来源的
// 域容易看串行。逐行排列后「这个模型由谁提供」是一列清晰的清单。
function mdProviderTags(m, clickable) {
  const ps = (m && m.providers) || [];
  return '<div class="md-prov-list">' + ps.map(p => mdProviderTag(p, clickable)).join('') + '</div>';
}

// mdUpstreamTag 明细视图 ID 前的来源标签 —— **已移除，恒为空字符串**。
//
// 明细视图的 ID 本身就把来源写全了：`cn:glm-5.2` / `global:gpt-5.4` /
// `trae:cn:DeepSeek-V4-Pro`。前面再挂一个 `wb:cn` / `trae:cn` 徽标是把同一份
// 信息写两遍，还占掉模型名的横向空间。
//
// 保留这个空函数而不是删掉调用点：mdRowHtml 的拼接、以及既有测试的取值位置
// 都还指到这里；返回 '' 让「标签区」自然消失，改动面最小。
function mdUpstreamTag() {
  return '';
}

// mdRowHtml 单个模型行（纯渲染，便于独立测试）。
function mdRowHtml(m, pr) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  // 能力徽标：默认模型 / 工具调用 / 视觉 / 纯推理（上游目录全字段透出，缺失不显示）
  const caps = [];
  if (m.is_default) caps.push('<span class="tag ok">默认</span>');
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
  // hidden 的可见标记：标签区去掉后不能再靠徽标划线，改在 ID 本身加划线与
  // 悬停说明（后端 entry["hidden"] 本来就带着这个信息）。
  const hid = m.hidden === true;
  const midStyle = hid ? ' style="text-decoration:line-through;opacity:.5"' : '';
  const hidTag = hid
    ? ' <span class="tag bad" title="已被 models.hidden 隐藏：客户端 /v1/models 不返回它，点名调用也会被拒">已隐藏</span>'
    : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + mdUpstreamTag(m) + '<span class="mid"' + midStyle + '>' + esc(m.id) + '</span>' + hidTag + mdAvailabilityTag(m) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
    '<td class="num">' + rateCell(m) + '</td>' +
    '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    outCell(m, pr) + '</tr>';
}

// mdMergedRateCell 合并视图的倍率列：各来源倍率不同，逐个列出。
//
// 刻意**不合并成一个数**：倍率是各自计费的，取最小或平均都会误导用户对
// 「用这个模型要花多少」的预期。后面的来源标注用与提供者列相同的**调用前缀**
// （wb:cn / trae:global），同一行里两处指代同一个来源时写法必须一致。
function mdMergedRateCell(m) {
  const ps = (m && m.providers) || [];
  const parts = [];
  for (const p of ps) {
    if (!p.credits && !p.promo_credits) continue;
    const src = mdUpstreamPrefix(p.upstream) + ':' + (p.realm || 'cn');
    const rate = p.promo_credits || p.credits;
    const label = p.promo_label ? ' <span class="tag ok">' + esc(p.promo_label) + '</span>' : '';
    parts.push('<div class="id" title="' + esc(src) + '">' +
      esc(rate) + label + ' <span style="color:var(--ink-3)">' + esc(src) + '</span></div>');
  }
  return parts.length ? parts.join('') : '—';
}

// mdMergedRowHtml 合并视图的单行（纯渲染，便于独立测试）。
//
// 「提供者」列是这个视图的核心：一行一模型，右侧列出它由哪些上游×域提供。
// 被全部来源隐藏的模型标灰划线而不是删掉——运维页面上「被藏了什么」本身
// 是需要知道的信息，而客户端 /v1/models 确实不返回它。
function mdMergedRowHtml(m) {
  const eff = (m.supported_efforts || []).slice();
  if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
  const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
    : '<span style="color:var(--ink-3);font-size:12.5px">' +
      (m.supports_reasoning ? '固定档 · 默认 ' + esc(m.default_effort || '?') : '不支持思考') + '</span>';
  const caps = [];
  if (m.supports_tool_call) caps.push('<span class="tag warn">工具</span>');
  if (m.supports_images) caps.push('<span class="tag warn">视觉</span>');
  if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">思考常开</span>');
  const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
  const hiddenAll = m.hidden_all === true;
  const nmStyle = hiddenAll ? ' style="opacity:.5;text-decoration:line-through"' : '';
  const nameLine = m.name && m.name !== m.bare_id
    ? '<div class="id">' + esc(m.name) + '</div>' : '';
  return '<tr><td class="mark" aria-hidden="true"><i></i></td>' +
    '<td class="who"><div class="nm"><span class="mid"' + nmStyle + '>' + esc(m.bare_id || m.id) + '</span>' +
    (hiddenAll ? ' <span class="tag bad" title="所有来源都被 models.hidden 隐藏，客户端 /v1/models 不返回它">已隐藏</span>' : '') +
    '</div>' + nameLine + capHtml + '</td>' +
    '<td style="white-space:normal">' + mdProviderTags(m, true) + '</td>' +
    '<td class="num">' + mdMergedRateCell(m) + '</td>' +
    '<td class="efs" style="white-space:normal">' + effs + '</td>' +
    '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
    '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td></tr>';
}

// mdMergedMatch 合并视图的筛选：复用现有条件，但按 providers 判定上游/域。
//
// 与明细视图的差别在语义：明细视图里「筛选 Trae」= 只留 Trae 那些行；合并
// 视图里 = 只留有 Trae 来源的模型（该模型的其它来源仍然显示——那正是用户
// 想看的信息：这个模型还有谁提供）。
function mdMergedMatch(m, f) {
  f = f || mdFilter;
  if (f.q) {
    // 可搜的写法：上游标识、展示名、以及**界面上真正显示的调用前缀**
    // （wb:cn / trae:global）——用户是照着标签复制粘贴来搜的。
    const provText = ((m.providers) || []).map(p =>
      [p.upstream, p.realm, MD_UPSTREAM_LABEL[p.upstream] || '',
        mdUpstreamPrefix(p.upstream) + ':' + (p.realm || 'cn')].join(' ')).join(' ');
    const text = [m.id, m.bare_id, m.name, provText].filter(Boolean).join(' ').toLowerCase();
    for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
      if (!text.includes(kw)) return false;
    }
  }
  const provs = m.providers || [];
  if (f.upstream && !provs.some(p => p.upstream === f.upstream)) return false;
  if (f.realm && !provs.some(p => p.realm === f.realm)) return false;
  if (f.cap === 'tool' && !m.supports_tool_call) return false;
  if (f.cap === 'vision' && !m.supports_images) return false;
  if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
  if (f.effort === 'off') {
    if (!m.can_disable_thinking) return false;
  } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
    return false;
  }
  // 价格筛选在合并视图里看「任一来源满足」：用户问的是「有没有便宜的来源」。
  if (f.promo) {
    const ok = provs.some(p => {
      if (f.promo === 'promo') return p.promo_factor != null || p.promo_label;
      if (f.promo === 'free') return p.promo_factor != null && Number(p.promo_factor) === 0;
      if (f.promo === 'discount') return p.promo_factor != null && Number(p.promo_factor) > 0;
      return true;
    });
    if (!ok) return false;
  }
  return true;
}

// mdMergedSortList 合并视图排序。默认保持后端给的「多来源优先」顺序。
function mdMergedSortList(list, f) {
  f = f || mdFilter;
  const out = list.slice();
  const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
  if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
  else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
  else if (f.sort === 'name') out.sort((a, b) => String(a.id).localeCompare(String(b.id)));
  else if (f.sort === 'rate') {
    // 合并视图按「最便宜的来源」排序：用户拿它找性价比最高的模型。
    const cheapest = m => {
      let best = Infinity;
      for (const p of (m.providers || [])) {
        const raw = (p.promo_credits != null && p.promo_credits !== '') ? p.promo_credits : p.credits;
        const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
        if (Number.isFinite(n) && n < best) best = n;
      }
      return best;
    };
    out.sort((a, b) => cheapest(a) - cheapest(b));
  }
  return out;
}

function renderMergedModels() {
  const tb = $('mdMergedBody');
  if (!tb) return;
  const list = mdMergedSortList(mdMerged.filter(m => mdMergedMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(mdMergedRowHtml).join('');
  }
  const filtered = list.length !== mdMerged.length;
  const el = $('mdCount');
  el.textContent = !mdMerged.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdMerged.length + ' 个模型'
    : mdMerged.length + ' 个模型';
  el.className = filtered ? 'note src-off' : 'note';
}

// mdApplyView 切换合并/明细视图（两个表格互斥显示，共用同一份筛选条件）。
function mdApplyView() {
  const merged = mdView !== 'detail';
  const mt = $('mdMergedTable'), dt = $('mdDetailTable');
  if (mt) mt.hidden = !merged;
  if (dt) dt.hidden = merged;
}

function renderModels() {
  mdApplyView();
  if (mdView === 'detail') {
    renderDetailModels();
  } else {
    renderMergedModels();
  }
}

function renderDetailModels() {
  const tb = $('mdBody');
  const list = mdSortList(mdAll.filter(m => mdMatch(m)));
  if (!list.length) {
    tb.innerHTML = '<tr><td colspan="7"><div class="empty">没有符合当前筛选条件的模型</div></td></tr>';
  } else {
    tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
  }
  const filtered = list.length !== mdAll.length;
  const el = $('mdCount');
  el.textContent = !mdAll.length ? ''
    : filtered ? '命中 ' + list.length + ' / ' + mdAll.length + ' 个模型'
    : mdAll.length + ' 个模型';
  el.className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
  mdFilter = { q: '', upstream: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
  $('mdQ').value = ''; $('mdUpstream').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
  $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
  // 重置只清筛选条件，**不动视图选择**：用户选了明细视图是为了看细节，
  // 清筛选条件不该顺手把他甩回合并视图。
  renderModels();
}

// 筛选控件：输入框防抖 120ms（长列表逐字符重排不必每键一次），下拉即时。
let mdQTimer = null;
$('mdQ').oninput = () => {
  clearTimeout(mdQTimer);
  mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdUpstream', 'upstream'], ['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
  const el = $(id);
  if (!el) continue;
  el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;

// 视图切换：两种视图共用同一份筛选条件（mdFilter），切过去不会丢条件。
if ($('mdViewSel')) {
  $('mdViewSel').value = mdView;
  $('mdViewSel').onchange = () => { mdView = $('mdViewSel').value; renderModels(); };
}

// 提供者徽标点击 = 筛到该来源。
//
// 用事件委托而不是给每个徽标挂 onclick：徽标是 innerHTML 批量生成的，
// 每次重渲染都会重建 DOM，逐个绑定既啰嗦又会漏（重渲染后新节点没有监听器）。
$('mdMergedBody').onclick = ev => {
  const chip = ev.target.closest('.md-prov');
  if (!chip) return;
  const up = chip.getAttribute('data-up') || '';
  const realm = chip.getAttribute('data-realm') || '';
  // 再点一次同一个徽标 = 取消筛选（避免用户被自己锁在一个来源里）。
  const same = mdFilter.upstream === up && mdFilter.realm === realm;
  mdFilter.upstream = same ? '' : up;
  mdFilter.realm = same ? '' : realm;
  if ($('mdUpstream')) $('mdUpstream').value = mdFilter.upstream;
  if ($('mdRealm')) $('mdRealm').value = mdFilter.realm;
  renderModels();
};
