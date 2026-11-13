# 接入 Trae 上游方案（可行性 + 落地设计）

> 目标：在现有 CodeBuddy（WorkBuddy）网关之外，新增 Trae 账号池与 Trae 上游链路，
> 使同一网关能同时服务两类账号，并为「多上游统一调度」铺路。
>
> 参考实现：[`smart-open/TraeWorkAssistant`](../../test/reference/TraeWorkAssistant) 的
> `api_server/`（Trae 池：`routes.rs` / `pool.rs` / `payload.rs` / `sse.rs` / `models_sync.rs`）。

## 1. 结论

**技术上可行，但认证链路是唯一分水岭**，且有一个必须先做 PoC 验证的前提。

| 维度 | 判断 |
|---|---|
| 协议适配 | 🟢 不难（新增一个 provider，约 2000–2800 行） |
| 请求体加密 | 🟢 不需要复刻（`llm_utils_chat` 明文 JSON 已被参考项目实测可行） |
| OAuth 登录 | 🟡 需改造（Trae 用授权码 + PKCE + **本机回环回调**，本项目是服务端） |
| **token 续期** | 🔴 **最大风险**：固化协议要求 DeviceProof（ECDSA P-256 签名），私钥在客户端 |
| 上游同步面 | 🟢 可控（走独立路由，不碰 `handler.go` 主链路） |

## 2. Trae 与 CodeBuddy 的协议差异

两套上游几乎没有共同点，属于**新增一个 provider** 而非"扩展参数"：

| 维度 | CodeBuddy（现有） | Trae（要加） |
|---|---|---|
| chat 端点 | `copilot.tencent.com/v2/chat/completions` | `trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat` |
| 认证头 | `Authorization: Bearer <token>` | `Authorization: Cloud-IDE-JWT <token>` + `X-Cloudide-Token` / `X-Ide-Token` / `X-App-Id` / `X-Device-Id` |
| 请求体 | OpenAI 形态直接改写 | SOLO 私有形态（`function=solo_work_lite` 等） |
| 响应 | 标准 OpenAI SSE | **自定义 SSE 事件**（见 §2.1） |
| token 续期 | refresh_token 端点 | ExchangeToken + **DeviceProof** |
| 模型目录 | `/v2/...models` | `/api/ide/v1/get_detail_param` |
| 区域 | cn / global 双 realm | 仅 cn（`trae.cn` / `trae.com.cn`） |
| 积分 | 成长任务体系 | IDE 积分 208（通用）/ Work 积分 209（不可复刻） |

参考项目记录的 Trae 端点全貌（`docs/tech-framework.md` §6.1）：

| 域名 | 端点 | 用途 |
|---|---|---|
| `trae-api-cn.mchost.guru` | `POST /api/agent/v3/llm_utils_chat` | 核心对话（HTTP + SSE） |
| `trae-api-cn.mchost.guru` | `POST /api/ide/v1/get_detail_param` | 模型列表 |
| `api.trae.cn` | `POST /trae/api/v2/ug/checkin_credits/claim` / `status` | 签到 |
| `api.trae.cn` | `POST /trae/api/v2/pay/ide_user_ent_usage` | 积分/权益查询 |
| `api.trae.com.cn` | `POST /cloudide/api/v3/trae/oauth/ExchangeToken` | Token 刷新 |
| `api.trae.com.cn` | `POST /cloudide/api/v3/trae/GetUserInfo` | 用户信息 |
| `www.trae.cn` | `GET /authorization` | OAuth 授权页 |

### 2.1 SOLO 自定义 SSE 事件

```
event:metadata      会话元数据（session_id/model，忽略）
event:timing_cost   耗时统计（忽略）
event:output        ×N 增量内容（response / reasoning_content / tool_calls）
event:extra_info    额外信息（忽略）
event:token_usage   token 统计（prompt_tokens/completion_tokens/total_tokens）
event:done          结束信号（finish_reason）
event:error         流内错误（code:1005 → PlanLimit 等）
```

转换要点（参考项目已验证）：

- `output` → `delta.content` / `reasoning_content` / `tool_calls`（需清理 namespace、partial_arguments 等 SOLO 专属字段）；
- `token_usage` 附到最后 chunk 的 `usage`；
- `done` → `finish_reason` + `[DONE]`；上游中断无 done 时幂等兜底仍写 `[DONE]`；
- `error` → 回调冷却 + 注入错误事件。

## 3. 认证链路可行性（核心）

### 3.1 OAuth 登录 —— 可解，需改造

参考项目的做法是**授权码 + PKCE + 本机回环回调**：

```rust
// commands/oauth.rs
const OAUTH_CLIENT_ID: &str = "ono9krqynydwx5";     // 公开常量
const OAUTH_CLIENT_SECRET: &str = "-";              // 公开常量
pub(crate) const OAUTH_LOOPBACK_PORT: u16 = 17388;
pub(crate) const OAUTH_REDIRECT_URI: &str = "http://127.0.0.1:17388/authorize";
```

关键发现：它的兜底变体 `ExchangeToken/AuthCode` **只用一个自生成的 `device_id`**，
不依赖本机客户端：

```rust
// oauth.rs:611-627 兜底变体（无 proof、无 DeviceInfo）
let mk = |tag: &str, auth_key: &str, url: String| {
    (... ureq::json!({
        "ClientID": client_id,
        auth_key: code_v,
        "CodeVerifier": verifier_v,
        "DeviceID": device_v,          // ← load_or_create_oauth_device()，工具自生成
        "PlatformCode": OAUTH_PAGE_PLATFORM_CODE,
    }), false)
};
```

**对本项目的意义**：回环回调需要改造（公网回调端点，或面板「手动粘贴回调 URL」——
与现有 WorkBuddy 设备码流程不同），但逻辑上不需要 Trae 客户端。

### 3.2 token 续期 —— 最大风险

参考项目注释明确（`oauth.rs:531`）：

> AuthCode 场景**不发 DeviceProof**（那是 refreshToken 刷新场景专属）

而 DeviceProof 的实现（`icube_auth.rs:9-12`）：

> ECDSA P-256 + SHA-256，签名原文 5 个 `\n` 连接：`POST\n<path>\n<ClientID>\n<AuthCode>\n<Ts>\n<Nonce>`
> 字段必须 PascalCase（Signature/Timestamp/Nonce），Timestamp 必须为 JSON int；
> 签名编码 P1363 r||s 64B 优先，DER 实测被拒。

**私钥从哪来？** 从本机 Trae 客户端的 `storage.json`：

```
iCubeAuthInfo://icube-dc:<deviceId> → tc 信封（AES-128-CBC）→ {"privateKeyPEM": "..."}
```

`extract_device_credentials()` 扫描
`%APPDATA%/{Trae CN, TRAE SOLO CN, Trae, Trae Work}/User/globalStorage/storage.json`。

**重要转机**：tc 信封的 pepper 是**随安装包分发的公开常量表**
（`icube_auth.rs:20-23` 硬编码 `WOE_T`/`VOE_T`/`JOE_T`/`HOE_T` 四组 64 字节，从
`out/main.js` 提取；解密算法 = `SHA512(random) || pepper → SHA512 → 前 32B` 切分为
aesKey[0..16] / iv[16..32]）。这意味着：

> **解密不需要任何客户端专有秘密——只要拿到密文，任何人都能解出私钥。**

于是服务端有两条出路：

| 出路 | 做法 | 风险 |
|---|---|---|
| **a. 用户上传 storage.json** | 面板粘贴 `iCubeAuthInfo://icube-dc:*` 的值（或整个文件），服务端解密取私钥 → 完整 DeviceProof 能力 | 需用户有 Trae 客户端；体验一般但可行 |
| **b. 依赖 Legacy 变体** | 参考项目保留了 `Refresh/Legacy` 兜底（`{ClientID, RefreshToken, ClientSecret}`，**无 proof**） | 注释称其为"旧协议兜底，保留至固化协议验证期结束"——**上游随时可能关闭** |

参考项目自己的错误文案印证了这点：

> `"无可用刷新变体（本机未发现 Trae 设备凭证且旧协议未启用）"`

### 3.3 JWT 生命周期 —— 有缓冲

JWT 全量寿命约 **13 天**，参考项目把刷新压到最后 48h（`accounts.rs:1523`）。
所以续期即使暂时失败，账号仍有较长可用窗口，不会立刻失效。

## 4. 落地方案

### 4.1 设计原则（与第三方上游方案一致）

- **独立路由**：`/trae/v1/*`，与 CodeBuddy 的 `/v1/*`、第三方的 `/tp/v1/*` 三者隔离；
- **独立包**：`internal/trae/`（client / pool / sse / payload / errors / auth）；
- **不动主链路**：`chatCompletions` 的 CodeBuddy 语义（6004/11102/11115/WAF 错误分类、
  `applyErrorPolicy` 十路状态机、`gateway_hint`、提示词改写、指纹脱敏）**一行不改**；
- **复用**：鉴权 `withAuth`、请求流水日志 `chatStat`、用量记账 `usage.Recorder`（`realm="trae"`）、
  Responses 转换（`internal/responses` + `responsesWriter`）、面板框架。

### 4.2 分层

```
客户端
  ├─ /v1/*      → CodeBuddy 账号池（现有，不变）
  ├─ /tp/v1/*   → 第三方 OpenAI 兼容（已实现）
  └─ /trae/v1/* → Trae 账号池（新增）
                    ├─ trae.Client.ChatStream()   SOLO SSE → OpenAI SSE
                    ├─ trae.Pool                  账号轮转 + 冷却
                    └─ responsesWriter             （复用：Chat → Responses）
```

### 4.3 模块划分

| 文件 | 职责 | 估算 |
|---|---|---|
| `internal/trae/config.go` | `trae.json` 配置（启用开关、超时、UA/版本、device_id 策略） | 150 行 |
| `internal/trae/auth.go` | OAuth 登录（授权码 + PKCE + 回调）、ExchangeToken、DeviceProof 签名 | 400 行 |
| `internal/trae/icube.go` | tc 信封解密 + 设备私钥导入（对应参考项目 `icube_auth.rs`） | 250 行 |
| `internal/trae/payload.go` | OpenAI Chat → `llm_utils_chat` 请求体构造 | 300 行 |
| `internal/trae/sse.go` | SOLO 自定义 SSE → OpenAI SSE / 聚合 | 500 行 |
| `internal/trae/pool.go` | 账号池（复用本项目 `pool` 的调度思想，或轻量自建） | 400 行 |
| `internal/trae/errors.go` | Trae 错误码 → 稳定形状（1005 PlanLimit 等） | 150 行 |
| `internal/server/trae.go` | 三个 handler（chat / responses / models） | 300 行 |
| `cmd/server/trae_config.go` | 配置加载 | 80 行 |

合计约 **2000–2800 行**（Go 重写；参考项目对应 Rust 约 3000+ 行）。

### 4.4 路由与配置

```
/trae/v1/chat/completions   Chat → SOLO → OpenAI SSE
/trae/v1/responses          Responses → Chat → SOLO → Responses（复用 responsesWriter）
/trae/v1/models             Trae 模型目录（get_detail_param）
/panel/ 内的 Trae 账号页     登录 / 导入设备凭证 / 状态
```

```json
{
  "enabled": false,
  "chat_base": "https://trae-api-cn.mchost.guru",
  "oauth": {
    "client_id": "ono9krqynydwx5",
    "client_secret": "-",
    "redirect_uri": "",
    "mode": "manual"
  },
  "device_proof": {
    "mode": "upload",
    "storage_json": ""
  },
  "timeout_seconds": 120
}
```

`oauth.mode`：`manual`（面板展示授权链接 → 用户粘贴回调 URL）或 `callback`（网关暴露公网回调端点）。
`device_proof.mode`：`upload`（用户粘贴 `iCubeAuthInfo` 密文，服务端解密）/ `legacy`（走无 proof 兜底）。

## 5. 分阶段

| 阶段 | 内容 | 估算 | 门槛 | 状态 |
|---|---|---|---|---|
| **P0 PoC** | 验证认证链路：① 服务端能否完成 OAuth 登录（兜底变体）；② refresh 是否必须 DeviceProof（Legacy 变体是否有效） | 1–2 天 | **决定项目成败** | ⬜ 未做（需真实账号） |
| P1 | `trae` 包骨架 + 凭证存储 + 设备私钥导入（tc 解密） | 3–5 天 | P0 通过 | ✅ **已实现** |
| P2 | `llm_utils_chat` 请求构造 + SOLO SSE → OpenAI 转换 | 4–6 天 | — | ✅ **已实现** |
| P3 | 面板接入（登录 / 账号列表 / 模型）+ 错误分类 + 冷却 | 3–4 天 | — | ✅ **已实现** |
| P4 | 签到 / 积分查询（`checkin_credits` / `ide_user_ent_usage`） | 2–3 天 | 可选 | ✅ **已实现** |

### P4 落地清单（2026-09-27）

| 文件 | 内容 |
|---|---|
| `internal/trae/credits.go` | 签到状态预检（`checkin_credits/status`）、签到（`checkin_credits/claim`）、积分包查询（`pay/ide_user_ent_usage`）；签到专用头族（VSCode UA + market/session 头）；宽容解析（`data/result/resp/response/info` 信封深挖，**但 `code` 只看顶层**——深挖会误伤嵌套业务对象的同名 code） |
| `internal/trae/scheduler.go` | 独立签到调度器（每分钟判定配置时点；同日去重；账号间限速 800ms；失败按分类写冷却、成功即清） |
| `internal/panel/trae_ops.go` | 面板运维接口：全部签到 / 单账号签到 / 积分查询 / 最近签到结果 |
| `internal/panel/index.html` + `app.js` | 账号行「签到」「积分」按钮 + 顶部「全部签到」「上次签到结果」+ 排程配置（开关 + 时点） |

**关键实现细节**：

- **签到接口与 chat 不同域**：对话走 `trae-api-cn.mchost.guru`，签到/积分走 `api.trae.cn`（`Config.APITraeBase` 可覆盖）；
- **预检优先**：先查 status，已签则跳过 claim（省一次上游请求，也避免重复领取）；
- **积分归属动态判定**：`product_id == 209` 为 Work 积分，其余（208/221…）归通用积分——
  签到积分归属曾变更（2026-09-04 前发 209，之后改发 208），**不可写死来源**；
- **调度器独立于 `internal/scheduler`**：后者是 CodeBuddy 账号池专用且属上游活跃文件
  （两轮同步均被触及），Trae 塞进去会持续产生合并成本；独立 ticker 足够且保持链路隔离。

**安全**：签到/积分接口只读上游状态，不改本地凭证；面板侧同样不返回任何凭证。

### P3 落地清单（2026-09-27）

| 文件 | 内容 |
|---|---|
| `internal/trae/oauth.go` | PKCE（S256）、授权 URL 生成（native_ide 渠道 + 抓包实证参数）、回调解析（`authCodeInfo` JSON / 裸串 / `code` 三形态）、ExchangeAuthCode（DeviceInfo 主变体 + 无 DeviceInfo 兜底）、ExchangeRefresh（DeviceProof P1363 主变体 + Legacy 兜底）、token 宽容提取（容器精确键 → 全树深挖） |
| `internal/trae/jwt.go` | JWT `exp` / `user_id` 解析、`NeedsRefresh`（过期时间未知时宁可多刷） |
| `internal/trae/errors.go` | 错误分类：1005 套餐额度用尽 → 12h 冷却；20403/20405 设备问题 → 摘出轮转；4001/4023 模型不可用 → 不罚账号；401 → 需重登；429 → 60s；5xx → 5m |
| `internal/trae/cooldown.go` | 账号级冷却表（进程内、惰性清理、快照供面板展示） |
| `internal/server/trae.go` | 接入冷却（选号跳过冷却账号）与错误策略（成功即清冷却） |
| `internal/panel/trae_config.go` | 面板接口：配置读写（**凭证脱敏**）、设备凭证导入（tc 解密）、OAuth 登录 start/finish、账号删除 |
| `internal/panel/index.html` + `app.js` | 「上游接入」页 Trae 区块：开关 / base / 账号列表（状态标签 + 冷却剩余）/ 授权登录 / 设备凭证导入 |

**安全约定**：面板**永不回显凭证**——GET 只返回 `has_token`/`has_refresh`/`has_device_key` 与 token 尾 4 位；
凭证写入只有两条路径（OAuth 换取、用户粘贴设备密文），且落盘权限 0600、文件已 gitignore。
保存配置时按 uid 列表读-改-写，**保留凭证字段**（面板拿不到明文，也就不可能回传）。

**登录流程**（服务端部署友好）：授权链接在浏览器打开 → 用户复制地址栏回调 URL 粘贴回面板 →
服务端校验 `loginTraceID` 与本地会话（CSRF）后换取 token。无需网关暴露公网回调端点。

### P1 / P2 落地清单（2026-09-27）

| 文件 | 内容 |
|---|---|
| `internal/trae/config.go` | `trae.json` 配置 + 账号模型 + 校验 + `AccessTokenHeader` |
| `internal/trae/icube.go` | tc 信封解密（AES-128-CBC + 公开 pepper 表 + SHA-512 完整性校验）、storage.json 解析、设备凭证提取 |
| `internal/trae/proof.go` | EC 私钥解析（PKCS#8/SEC1）、SPKI 公钥导出、DeviceProof 签名（P1363 r‖s）、验签 |
| `internal/trae/payload.go` | OpenAI Chat → `llm_utils_chat`（模型映射、tool_calls 改名、tool_choice/tools 归一、必需字段注入） |
| `internal/trae/sse.go` | SOLO 自定义 SSE → OpenAI SSE 流式转换 + 非流式聚合 |
| `internal/trae/client.go` | 上游调用（完整出站头族、Retry-After 解析） |
| `internal/server/trae.go` | `/trae/v1/{chat/completions,responses,models}` 三个 handler |
| `cmd/server/trae_config.go` | 配置加载 |

**关键设计**：P1/P2 全部为**纯函数 + 可单测**（除 client 的网络调用外无副作用），
因此 P0 PoC 未做也不影响实现——一旦认证链路验证通过，协议层可直接投入使用。

**默认关闭**：`trae.json` 不存在或 `enabled=false` 时 `/trae/v1/*` 返回 404，
与 `/v1/*`、`/tp/v1/*` 互不影响。

### P0 PoC 工具（`cmd/traepoc`，已实现）

```bash
# ① 验证「服务端无客户端能否登录」——不加 -key，只走无 DeviceProof 的兜底变体
go run ./cmd/traepoc login

# ② 验证「refresh 是否必须 DeviceProof」——不加 -key 只会试 Legacy 变体
go run ./cmd/traepoc refresh -refresh <refresh_token>

# ③ 验证凭证 + 设备头被上游接受（顺带确认 chat 链路可用同一凭证）
go run ./cmd/traepoc status -token <access_token>

# ④ 验证 tc 信封 pepper 表是否仍有效（本地解密客户端凭证）
go run ./cmd/traepoc device "<storage.json 路径>"
```

设计要点：

- **逐变体报告**：靠 `trae.Client.Trace` 钩子（生产路径不设置，零开销），
  打印每个变体的 ✓/✗ 与错误详情——"哪个变体成功"正是 P0 的答案；
- **默认走服务端场景**：不传 `-key` 时只尝试无 DeviceProof 的兜底变体，
  与"网关部署在服务器、没有 Trae 客户端"的真实条件一致；
- **不泄漏凭证**：token 只报"有无 + 长度"，私钥只报 SHA-256 前 8 字节指纹；
- **结论化输出**：每个子命令末尾直接给出 ①②③ 的判定与后续动作指引。

### P0 PoC 的三种结果

| 结果 | 后续 |
|---|---|
| ① ✅ ② ✅ | 按 §4 全量推进，最顺 |
| ① ✅ ② ❌ | 必须实现 §4.4 的 `device_proof.mode=upload`（pepper 公开，仍可行） |
| ① ❌ | Trae 接入不成立，除非依赖本机客户端配合（MITM 抓 JWT 或手动提取） |

## 6. 风险

| 风险 | 等级 | 说明与缓解 |
|---|---|---|
| token 续期依赖 DeviceProof | 🔴 高 | 服务端无客户端私钥；缓解 = 用户上传 storage.json（pepper 公开可解） |
| 上游协议收紧 | 🟡 中 | "明文 JSON 可行"是当前实测结论，上游若强制 aha 加密则路径中断 |
| OAuth 回调改造 | 🟡 中 | 回环回调 → 公网回调或手动粘贴，需新设计 |
| 设备指纹一致性 | 🟡 中 | 参考项目记录 `x-device-id` 需与账号绑定且保持一致（风控高危信号） |
| Work 积分（209）不可用 | 🟢 低 | 参考项目已排除（加密体无法复刻），不影响 chat 接入 |
| 上游同步面 | 🟢 低 | 独立路由 + 独立包，仅 `handler.go` 路由注册数行 |

## 7. 与「多上游统一调度」的关系

Trae 与第三方上游（`/tp/v1/*`）都是**同一演进方向**的两步：把上游抽象为 provider。

建议顺序：

1. **第三方上游（已完成）**——最简单的 provider，先把"独立前缀 + 复用转换层"的模式跑通；
2. **Trae（本方案）**——第一个需要私有协议转换的 provider，验证转换层与账号池；
3. **统一调度（可选）**——在两者稳定后，把 `per_model` / `priority` / `fallback`
   以纯配置实现（参考项目 `dispatch.rs` 的做法），届时 `/v1`、`/tp/v1`、`/trae/v1`
   可收敛为同一入口按模型名路由。

第 3 步的详细分析见 [`third-party-upstream-plan.md`](./third-party-upstream-plan.md) §6–§7。
