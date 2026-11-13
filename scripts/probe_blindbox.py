#!/usr/bin/env python3
"""Buddy 能量盲盒 / 抽奖端点探针（只读为主）.

用途
  probe     只读：查抽奖次数、Buddy 能量额度、旅行状态、连登状态、能量余额
  open      写：开 Buddy 盲盒（消耗能量）——必须 --yes
  draw      写：抽一次奖（消耗抽奖次数）——必须 --yes

背景
  本项目（workbuddy2api-panel）已实现：
    POST /activity/growth/lottery/draw   （且已带 client_token，见 upstream/streak.go:103）
    GET  /activity/growth/lottery/summary
    POST /activity/growth/makeup-cards/use
  参考实现 88lin/workbuddy-auto-signin 另有两个本项目未实现的端点：
    GET  /v2/activity/growth/buddy/quota  → {affordable, max_open_count, ...}
    POST /v2/activity/growth/buddy/open   → {count, client_token}
  本脚本用于实测确认这两个端点的真实形状，以及「能量是否真的只进不出」。

  ⚠️ 盲盒开出的可能是实物奖（需填写收件地址），脚本不会代填。

用法
  python3 probe_blindbox.py probe 00e26541
  python3 probe_blindbox.py probe ALL
  python3 probe_blindbox.py open  00e26541 --count 1 --yes
  python3 probe_blindbox.py draw  00e26541 --yes

注意
  - 默认 dry-run：写动作必须显式加 --yes
  - 写操作不自动重试（服务端可能已处理完成，重试会重复消耗）
  - 同一账号连续写请求间隔 >= 1.05s
"""
import argparse
import glob
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid

# auths 目录：优先环境变量，其次脚本同级的 ../auths，最后回落线上路径。
_HERE = os.path.dirname(os.path.abspath(__file__))
AUTHS = (os.environ.get("WB_AUTHS_DIR")
         or (os.path.join(os.path.dirname(_HERE), "auths")
             if os.path.isdir(os.path.join(os.path.dirname(_HERE), "auths"))
             else "/root/workbuddy2api/auths"))

# growth 域在 chat base 上（见 internal/upstream/travel.go growthJSON 用 chatBase）。
CHAT_BASE_CN = "https://copilot.tencent.com"
CHAT_BASE_GLOBAL = "https://www.workbuddy.ai"

# growth 端点（/v2/activity/growth 前缀）
GROWTH_PREFIX = "/v2/activity/growth"

# 出站头对齐 internal/upstream/headers.go：
#   defaultClientVersion = "5.5.4"，defaultCliVersion = "2.137.1"
#   UA = "WorkBuddy/<cv> WorkBuddy/<cv> CLI/<cli>"（CN 形态）
CLIENT_VERSION = "5.5.4"
CLI_VERSION = "2.137.1"
ORIGIN_CN = "https://www.codebuddy.cn"
ORIGIN_GLOBAL = "https://www.workbuddy.ai"


# ---------------------------------------------------------------------------
# 凭证加载
# ---------------------------------------------------------------------------

def load_auths(spec):
    """按 uid 前缀 / 文件名 / ALL 加载凭证，返回 list[{token,uid,domain,nick,file,realm}]。

    auth 文件是 UTF-8；显式 encoding 避免 Windows 中文 locale（GBK）解码失败。
    """
    if spec.upper() == "ALL":
        paths = sorted(glob.glob(os.path.join(AUTHS, "*.json")))
    elif os.path.sep in spec or spec.endswith(".json"):
        p = spec if os.path.isabs(spec) else os.path.join(AUTHS, spec)
        paths = [p]
    else:
        paths = sorted(glob.glob(os.path.join(AUTHS, f"*{spec}*.json")))

    if not paths:
        raise SystemExit(f"未找到账号凭证：{spec}（AUTHS={AUTHS}）")

    out = []
    for p in paths:
        try:
            d = json.load(open(p, encoding="utf-8"))
        except Exception as e:
            print(f"  ! 跳过 {os.path.basename(p)}：{e}", file=sys.stderr)
            continue
        # 兼容两种形态：插件 OAuth 嵌套形 {auth:{...},account:{...}} 与扁平形。
        a = d.get("auth") or d
        acc = d.get("account") or {}
        token = a.get("accessToken") or a.get("access_token") or ""
        if not token:
            print(f"  ! 跳过 {os.path.basename(p)}：无 accessToken", file=sys.stderr)
            continue
        uid = acc.get("uid") or a.get("uid") or ""
        domain = a.get("domain") or ""
        realm = a.get("realm") or ("global" if domain.endswith("workbuddy.ai") else "cn")
        out.append({
            "token": token,
            "uid": uid,
            "domain": domain,
            "nick": acc.get("nickname") or "",
            "file": os.path.basename(p),
            "realm": realm,
        })
    if not out:
        raise SystemExit("没有可用凭证")
    return out


# ---------------------------------------------------------------------------
# 请求
# ---------------------------------------------------------------------------

def _base(c):
    return CHAT_BASE_GLOBAL if c["realm"] == "global" else CHAT_BASE_CN


def _headers(c):
    """复刻 internal/upstream/headers.go 的 BillingHeaders + 稳定设备头。

    growth 域走 growthJSON → BillingHeaders，因此这里对齐 BillingHeaders 的字段，
    并补 CommonHeaders 才有的 X-Machine-ID / X-Session-ID（BillingHeaders 本身不注入，
    但上游对 growth 域不强制；带上更接近真实客户端，减少风控误判）。
    """
    origin = ORIGIN_GLOBAL if c["realm"] == "global" else ORIGIN_CN
    platform = "WorkBuddy AI" if c["realm"] == "global" else "WorkBuddy"
    ua = f"WorkBuddy/{CLIENT_VERSION} {platform}/{CLIENT_VERSION} CLI/{CLI_VERSION}"
    h = {
        "Authorization": "Bearer " + c["token"],
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-CodeBuddy-Request": "1",
        "Accept-Language": "en-US" if c["realm"] == "global" else "zh-CN",
        "User-Agent": ua,
        "Origin": origin,
        "Referer": origin + "/",
    }
    if c["uid"]:
        h["X-User-Id"] = c["uid"]
    if c["domain"]:
        h["X-Domain"] = c["domain"]
    return h


def _call(c, method, path, body=None, timeout=30):
    """发一次请求；返回 (http_code, parsed_body)。

    写操作调用方保证只发一次——不在此重试。
    """
    url = path if path.startswith("http") else _base(c) + path
    data = json.dumps(body).encode("utf-8") if body is not None else None
    req = urllib.request.Request(url, data=data, headers=_headers(c), method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
            return r.status, _parse(raw)
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        return e.code, _parse(raw)
    except Exception as e:
        return -1, {"__err__": repr(e)}


def _parse(raw):
    try:
        return json.loads(raw)
    except Exception:
        return {"__raw__": raw[:500]}


def _dig(obj, *keys, default=None):
    """按序取第一个存在的键（上游字段名在不同活动期会漂移）。"""
    if not isinstance(obj, dict):
        return default
    for k in keys:
        if k in obj:
            return obj[k]
    return default


def _envelope(body):
    """拆信封：上游多为 {code, msg, data:{...}}，也见过直接平铺。"""
    if isinstance(body, dict) and "data" in body and isinstance(body["data"], dict):
        return body["data"]
    return body if isinstance(body, dict) else {}


def _ok(code, body):
    if code < 200 or code >= 300:
        return False
    if isinstance(body, dict) and body.get("code") not in (0, None):
        return False
    return True


def _brief(code, body):
    if code == -1:
        return "网络错误：" + str(body.get("__err__", ""))[:120]
    if isinstance(body, dict):
        msg = _dig(body, "msg", "message", "displayMsg", default="")
        c = body.get("code")
        if msg or c is not None:
            return f"HTTP {code} code={c} msg={str(msg)[:100]}"
    return f"HTTP {code} " + json.dumps(body, ensure_ascii=False)[:160]


def _client_token(prefix="u"):
    """防重放 token：官方前端用 crypto.randomUUID() 拼 "u-<uuid>"。

    服务端只做幂等去重、不校验格式（见 internal/upstream/streak.go:27 同实现）。
    """
    return f"{prefix}-{uuid.uuid4()}"


# ---------------------------------------------------------------------------
# 只读探测
# ---------------------------------------------------------------------------

def probe_one(c, verbose=False):
    """对一个账号跑全部只读检查，返回 dict（供汇总）。"""
    r = {"uid": c["uid"], "nick": c["nick"], "realm": c["realm"]}

    # 1) 抽奖次数
    code, body = _call(c, "GET", GROWTH_PREFIX + "/lottery/summary")
    if _ok(code, body):
        d = _envelope(body)
        r["lottery_chances"] = _dig(d, "chances", "balance", default=0)
        r["lottery_ok"] = True
    else:
        r["lottery_err"] = _brief(code, body)

    # 2) Buddy 能量额度（本次要确认的新端点）
    code, body = _call(c, "GET", GROWTH_PREFIX + "/buddy/quota")
    if _ok(code, body):
        d = _envelope(body)
        r["energy"] = _dig(d, "energy", "balance", "energy_balance", default=None)
        r["affordable"] = _dig(d, "affordable", "affordable_count", default=None)
        r["max_open_count"] = _dig(d, "max_open_count", "max_open", default=None)
        r["cost_per_open"] = _dig(d, "cost_per_open", "cost", "energy_cost", default=None)
        r["quota_ok"] = True
        if verbose:
            r["quota_raw"] = body
    else:
        r["quota_err"] = _brief(code, body)

    # 3) 旅行状态（盲盒前置：必须 idle）
    code, body = _call(c, "GET", GROWTH_PREFIX + "/buddy/travel/status")
    if _ok(code, body):
        d = _envelope(body)
        r["travel_state"] = _dig(d, "state", default="?")
        r["travel_daily_limit"] = _dig(d, "daily_limit_reached", default=None)
        r["travel_reward"] = _dig(d, "reward_credit", default=None)
    else:
        r["travel_err"] = _brief(code, body)

    # 4) 能量余额（另一处口径，用于交叉验证 quota 的数字）
    code, body = _call(c, "GET", GROWTH_PREFIX + "/energy")
    if _ok(code, body):
        d = _envelope(body)
        r["energy_direct"] = _dig(d, "energy", "balance", "total", default=None)
        if verbose:
            r["energy_raw"] = body
    else:
        r["energy_err"] = _brief(code, body)

    # 5) 连登 + 补登卡（确认补签卡是否还有存量没用）
    code, body = _call(c, "GET", GROWTH_PREFIX + "/streak")
    if _ok(code, body):
        d = _envelope(body)
        s = d.get("streak") if isinstance(d.get("streak"), dict) else d
        mc = d.get("makeup_cards") if isinstance(d.get("makeup_cards"), dict) else {}
        r["streak_days"] = _dig(s, "days", default=None)
        r["makeup_cards"] = _dig(mc, "balance", "count", default=None)
    else:
        r["streak_err"] = _brief(code, body)

    return r


def cmd_probe(args, verbose=False):
    creds = load_auths(args.account)
    rows = []
    for i, c in enumerate(creds):
        if i:
            time.sleep(args.gap)  # 同账号集连打时留间隔，降低风控误判
        print(f"\n=== {c['nick'] or '?'}  {c['uid'][:8]}  [{c['realm']}]  ({c['file']}) ===")
        r = probe_one(c, verbose=verbose)
        rows.append(r)
        for k in ("lottery_chances", "energy", "affordable", "max_open_count",
                  "cost_per_open", "energy_direct", "travel_state", "travel_reward",
                  "streak_days", "makeup_cards"):
            if k in r:
                print(f"  {k:18} = {r[k]}")
        for k in ("lottery_err", "quota_err", "travel_err", "energy_err", "streak_err"):
            if k in r:
                print(f"  {k:18} ! {r[k]}")
        if verbose and "quota_raw" in r:
            print("  quota_raw:", json.dumps(r["quota_raw"], ensure_ascii=False)[:600])

    if len(rows) > 1:
        print("\n=== 汇总 ===")
        print(f"{'账号':<14}{'抽奖':>6}{'能量':>8}{'可开':>6}{'上限':>6}{'单价':>6}  {'旅行':<10}{'连登':>5}{'补签卡':>7}")
        for r in rows:
            print(f"{(r['nick'] or r['uid'][:8]):<14}"
                  f"{str(r.get('lottery_chances', '-')):>6}"
                  f"{str(r.get('energy', r.get('energy_direct', '-'))):>8}"
                  f"{str(r.get('affordable', '-')):>6}"
                  f"{str(r.get('max_open_count', '-')):>6}"
                  f"{str(r.get('cost_per_open', '-')):>6}  "
                  f"{str(r.get('travel_state', '-')):<10}"
                  f"{str(r.get('streak_days', '-')):>5}"
                  f"{str(r.get('makeup_cards', '-')):>7}")

    # 结论提示：这是本次探测的核心问题
    print("\n--- 判读 ---")
    any_quota_ok = any(r.get("quota_ok") for r in rows)
    if any_quota_ok:
        print("✓ /buddy/quota 可用 → 能量盲盒端点确认存在")
        energies = [r.get("energy") for r in rows if r.get("energy") is not None]
        if energies and all(e == 0 for e in energies):
            print("  · 能量全为 0：可能确实没有存量，或字段名不同（用 --verbose 看原始响应）")
        elif energies:
            print(f"  · 检测到能量存量 {energies} → 这些能量当前无处消耗，盲盒值得做")
    else:
        print("✗ /buddy/quota 全部失败 → 该端点可能已变更，或需要不同的域/头")

    travel_states = [r.get("travel_state") for r in rows if r.get("travel_state")]
    if travel_states:
        non_idle = [s for s in travel_states if s != "idle"]
        print(f"  旅行状态分布：{ {s: travel_states.count(s) for s in set(travel_states)} }")
        if non_idle:
            print(f"  · {len(non_idle)} 个账号非 idle → 按参考实现，这些账号当天不能开盲盒")
    return 0


# ---------------------------------------------------------------------------
# 写操作
# ---------------------------------------------------------------------------

def cmd_open(args):
    """开 Buddy 盲盒。先查 quota，再决定 count（不会超过服务端允许的上限）。"""
    creds = load_auths(args.account)
    for i, c in enumerate(creds):
        if i:
            time.sleep(args.gap)
        print(f"\n=== {c['nick'] or '?'}  {c['uid'][:8]} ===")

        # 前置检查 1：旅行必须 idle（参考实现的门控）
        code, body = _call(c, "GET", GROWTH_PREFIX + "/buddy/travel/status")
        if _ok(code, body):
            st = _dig(_envelope(body), "state", default="?")
            if st != "idle":
                print(f"  跳过：旅行状态 {st}（需 idle 才能开盲盒）")
                continue
        else:
            print(f"  警告：旅行状态查询失败（{_brief(code, body)}），继续尝试")

        # 前置检查 2：能量额度
        code, body = _call(c, "GET", GROWTH_PREFIX + "/buddy/quota")
        if not _ok(code, body):
            print(f"  失败：quota 查询失败 {_brief(code, body)}")
            continue
        d = _envelope(body)
        affordable = _dig(d, "affordable", "affordable_count", default=0) or 0
        max_open = _dig(d, "max_open_count", "max_open", default=1) or 1
        try:
            affordable, max_open = int(affordable), int(max_open)
        except (TypeError, ValueError):
            print(f"  失败：quota 字段无法解析 {json.dumps(d, ensure_ascii=False)[:200]}")
            continue
        if affordable <= 0:
            print(f"  跳过：能量不足（affordable=0）")
            continue

        count = min(affordable, max_open, args.count or 1)
        payload = {"count": count, "client_token": _client_token()}
        print(f"  能量可开 {affordable} 次，上限 {max_open}，本次开 {count} 次")
        if not args.yes:
            print(f"  [dry-run] 将要 POST {GROWTH_PREFIX}/buddy/open {json.dumps(payload, ensure_ascii=False)}")
            continue

        code, body = _call(c, "POST", GROWTH_PREFIX + "/buddy/open", payload)
        if _ok(code, body):
            d = _envelope(body)
            name = _dig(d, "buddy", "name", "buddies", default="?")
            if not isinstance(name, str):
                name = json.dumps(name, ensure_ascii=False)[:80]
            print(f"  ✓ 开盒成功 ×{count}：{name}")
            if _dig(d, "need_address", "require_address", default=False):
                print("  ⚠ 实物奖：需自行到成长中心填写收件地址")
        else:
            print(f"  ✗ 开盒失败：{_brief(code, body)}")
    return 0


def cmd_draw(args):
    """抽一次奖（验证 client_token 是否必需——本项目已带，这里用作对照）。"""
    creds = load_auths(args.account)
    for i, c in enumerate(creds):
        if i:
            time.sleep(args.gap)
        print(f"\n=== {c['nick'] or '?'}  {c['uid'][:8]} ===")
        code, body = _call(c, "GET", GROWTH_PREFIX + "/lottery/summary")
        chances = 0
        if _ok(code, body):
            chances = _dig(_envelope(body), "chances", "balance", default=0) or 0
        try:
            chances = int(chances)
        except (TypeError, ValueError):
            chances = 0
        if chances <= 0:
            print("  跳过：无抽奖次数（次数来自连登兑换）")
            continue
        if args.no_token:
            payload = {}
            print("  注意：本次故意不带 client_token（对照组，预期 400）")
        else:
            payload = {"client_token": _client_token()}
        if not args.yes:
            print(f"  [dry-run] 将要 POST {GROWTH_PREFIX}/lottery/draw {json.dumps(payload)}")
            continue
        code, body = _call(c, "POST", GROWTH_PREFIX + "/lottery/draw", payload)
        if _ok(code, body):
            d = _envelope(body)
            prize = _dig(d, "prize_name", "prize", default="?")
            print(f"  ✓ 中奖：{prize}")
            if _dig(d, "need_address", "require_address", default=False):
                print("  ⚠ 实物奖：需自行填写收件地址")
        else:
            print(f"  ✗ 抽奖失败：{_brief(code, body)}")
    return 0


# ---------------------------------------------------------------------------

def main():
    ap = argparse.ArgumentParser(
        description="Buddy 能量盲盒 / 抽奖端点探针",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__)
    ap.add_argument("action", choices=["probe", "open", "draw"])
    ap.add_argument("account", help="uid 前缀 / auth 文件名 / ALL")
    ap.add_argument("--yes", action="store_true", help="写操作必须显式放行")
    ap.add_argument("--count", type=int, default=1, help="open 时本次开盒数（受服务端上限钳制）")
    ap.add_argument("--gap", type=float, default=1.05, help="多账号之间的间隔秒数（默认 1.05）")
    ap.add_argument("--no-token", action="store_true", help="draw 时故意不带 client_token（对照实验）")
    ap.add_argument("--verbose", action="store_true", help="打印原始响应")
    args = ap.parse_args()

    print(f"auths 目录：{AUTHS}")
    if args.action == "probe":
        return cmd_probe(args, verbose=args.verbose)
    if args.action == "open":
        if not args.yes:
            print("\n[dry-run 模式] 加 --yes 才会真正开盒（消耗能量，不可逆）")
        return cmd_open(args)
    if args.action == "draw":
        if not args.yes:
            print("\n[dry-run 模式] 加 --yes 才会真正抽奖（消耗次数，不可逆）")
        return cmd_draw(args)


if __name__ == "__main__":
    sys.exit(main())
