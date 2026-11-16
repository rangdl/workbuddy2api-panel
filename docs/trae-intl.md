# Trae 国际版（global 域）接入

> 本文整理自参考项目 `dsh-connect-trae` 的国际版取证（2026-09 ~ 2026-10），
> 并对齐到本项目的域模型。**本文只写"本项目该怎么做"**，不照抄对方的模块划分。
>
> 证据等级沿用对方口径：**A** = 官方客户端真实请求日志；**A+** = 受控只读探测
> 实测 200（零状态变更）；**B** = 安装包/bundle 可证；**D** = 纯推断。

## 0. 结论先说

**国际版是 Trae 上游的 `global` 域，不是新上游、不是新维度。**

| 维度 | 取值 | 谁回答 |
|---|---|---|
| 上游 | `trae`（不变） | `acc.Backend()` |
| 域 | `cn` / `global` | `acc.Realm()` |

这条边界必须守住：参考项目内部管国际版叫 `ai` 桶（`TraeRegion = 'cn' | 'ai'`），
那是**它的**存储键名，与本项目的 `global` 是同一件事。**不要把 `ai` 引进本仓库的
域枚举**——本项目已经有 `cn` / `global`（CodeBuddy 在用），再造一个同义词会让
`ParseModelRef`、`gateSpec`、用量账本同时出现三套域名。

同样，**不要因为模型阵容不同就新建一个 `trae-global` 上游**。上游身份由协议
（端点、鉴权头、SSE 事件、请求体）决定，国际版与国内版是同一套协议，只是 base
和目录不同。

## 1. 与国内版的契约差异（汇总表）

| 维度 | `cn` | `global` |
|---|---|---|
| 凭证 host | `api.trae.cn`（统一） | `api-sg-central.trae.ai` / `growsg-normal.trae.ai`（**按 App 分，不统一**） |
| 域判定字段 | `userRegion.region == "CN"` | `"SG"`（日志里也见小写 `"sg"`，**必须大小写不敏感**） |
| 聊天网关 | `https://trae-api-cn.mchost.guru` | `https://coresg-normal.trae.ai` |
| remote 目录 | `https://solo.trae.cn/api/remote/v1` | `https://coresg-normal.trae.ai/api/remote/v1` |
| 用量/支付 | `api.trae.cn` 的 `web_user_ent_usage`（积分包） | 凭证自带 host 的 `ide_user_pay_status`（订阅状态） |
| 签到 | `/trae/api/v2/ug/checkin_credits/*` | **不存在**（全族 404） |
| 模型阵容 | GLM / Kimi / DeepSeek / Doubao | Gemini / GPT / MiniMax / Kimi（**基本不重叠**） |
| 积分倍率 | `features.consumption_rate` 存在 | **未见**（订阅制，无按量积分） |
| 出站头 | `x-app-id` 等整套 | **相同**（实测被 SG 网关原样接受） |
| 存储解密器 | `decryptTraeStorageValue` | **相同**（四版通吃） |

**只有 base、目录、用量数据源、签到可用性四项不同，协议本身相同。**

## 2. 域判定：三层，从凭证自身读

参考实现的判定顺序（`regionOfCredential`），每一层都来自凭证自带的数据，
**不需要用户配置**：

```
userRegion.region  →  host 后缀  →  edition 标签
   ("CN"/"SG")        (.trae.ai / .trae.cn / .trae.com.cn)     (cn/sg/solo/solo-sg)
```

三条硬要求：

1. **`userRegion` 是权威**。桌面版 storage.json 解出来是对象
   `{"region":"CN","_aiRegion":"CN"}`，App 日志里却是裸小写 `"sg"`——两种形态都要认，
   且大小写不敏感。
2. **host 不统一**。国际侧有 `api-sg-central` 和 `growsg-normal` 两个 host，
   且 `account` 里有 `migrateToSG` 字段（CN 账号可迁移到 SG）。所以**路由必须按
   凭证自带的 host**，不能硬编码单个 SG host。
3. **不能拿 base 反推域**。本项目现在的 `realmOfBase(chatBase, apiBase)` 是拿
   配置里的 base 域名判域——这等于**让整份 `trae.json` 只能是一个域**。国际版
   落地时必须改成按账号判：域是账号的属性，不是配置文件的属性。

## 3. US 区：一个必须显式存在、但**不被服务**的区域

参考项目踩过的坑（issue #66），值得单独记一节，因为它是"三级回退"的经典陷阱：

```
US 凭据 { host: "grow-normal.traeapi.us", userRegion: "US", edition: "sg" }
  regionOfUserRegion("US")    → undefined   ← 只认 cn / sg
  regionOfHost("…traeapi.us") → undefined   ← 只认 .trae.ai / .trae.cn
  regionOfEdition("sg")       → "ai"        ← 唯一命中，于是 US 变成了 SG
  → 所有读请求打到 *.trae.ai → 401
  → 路由 catch 一律包成 HTTP 500
  → 用户看到"插件坏了"，重登 → 又拿到 US 凭据 → 死循环
```

US 集群是**真实存在**的独立集群（`grow-normal.traeapi.us` 对 pay 端点回 200，
而 SG 网关回 401），但契约不同（remote 目录 404、`llm_utils_chat` 回 403
`40006 no schema found`）。把 US 映射成 `global` 等于用 SG 的契约打 US，两条路都错。

**本项目的对应做法**：域判定函数必须能返回"我认识这个域，但不服务"的第三态，
而不是回落成 `global`。判定结果是 `US` 时，选号阶段就排除（或把账号标记为
不可用并给出明确原因），**不要让它进池**——进池的后果是它会被当成普通 global
账号选中，然后 401，然后被记成一次"上游故障"，污染熔断判据。

同理，`401` / `403` **必须原样保留状态码语义**跨过网关边界。把它翻译成 500
会让排障方向从"域不对"偏到"服务端崩了"。

> 证据等级 A（报告人提供双 host 对比实测）。US 区本身**未支持**，本文不涉及。

## 4. 模型目录：必须按 (上游, 域) 分缓存

国际版与国内版的模型阵容**基本不重叠**（CN 是 GLM/Kimi/DeepSeek，国际是
Gemini/GPT/MiniMax）。参考实现为此把目录、勾选、预算、fallback 全部按 region 二分，
并明确一条迁移语义：

> **旧扁平配置只作 `cn` 桶的迁移来源，`ai`（国际）桶绝不继承。**

理由：国内勾选与国际目录求交集后，会静默丢掉所有国内模型——用户看到的是
"勾选莫名其妙消失"。

**本项目当前有两处单例缓存是明确缺陷**（国际版一旦接入就会立刻暴露）：

| 位置 | 现状 | 问题 |
|---|---|---|
| `internal/trae/catalog.go` 的 `globalCatalogCache` | 进程内单例，注释写"模型目录与账号无关" | 国际账号与国内账号**共用一份**目录 |
| `internal/trae/remote_models.go` 的 `globalRemoteCache` | 同上 | 同上，且这份还带积分倍率 |

`pool/catalog.go` 的 `catalogGroup{backend, realm}` 已经是按域分组的，**池那一层
是对的**；错的是 provider 内部这层缓存。改法：把这两个包级单例变成
`map[realm]*catalog`，键用域。

### 4.1 `function` 归属：目录查询与聊天不是同一张表

参考实现区分了两张表，本项目目前只有一张，这是接入国际版时最容易踩的坑：

| 用途 | 表 | 说明 |
|---|---|---|
| `get_detail_param` 目录查询 | `TRAE_REMOTE_DIRECTORY_FUNCTIONS` | 问全，不认识的 function 上游宽容忽略 |
| `llm_utils_chat` 实际调用 | `TRAE_CHANNEL_PRIORITY` | **只含真正能聊天的 function** |

`TRAE_CHANNEL_PRIORITY` 两个域不同：

```
cn: solo_work_remote → solo_work_lite → solo_agent_remote → chat_v3 → solo_coder
global: solo_work_remote → solo_work_lite → solo_agent_remote → chat_v3
```

关键事实（都是实测）：

- **`solo_agent` 不能用于聊天**。它的 `get_detail_param` 返回的条目数是所有
  function 里最多的一档（参考项目两份材料分别记为 39 / 43 条，取样时间不同），
  但每个模型用 `function: "solo_agent"` 发出去都会失败。它优先级一高就会
  "把自己不能服务的 config 盖上自己的名字"，导致 10 个国际模型里 8 个调不动。
- **国际版查目录要用 `solo_agent` 才全**：`solo_work_lite` 只覆盖 7 个远程模型里的
  3 个，`solo_agent` 覆盖 7/7。**聊天仍然统一用 `solo_work_lite`**（国际网关
  端到端实测 200）。
- `chat_v3` 是国际侧最宽的聊天名册（40 条），放最后只补空缺，不抢优先级。

**本项目的现状缺口**：`internal/trae/payload.go` 用一张硬编码表
`soloAgentModels` 决定 function（`doubao-seed-code` / `glm-5.3-flash` / …），
这些全是 CN 模型。国际模型（`gemini-*` / `gpt-*` / `minimax-*`）不在表里，
会一律落到 `solo_work_lite`——而参考实现实测其中一部分只在
`solo_agent_remote` 下可调。**接入国际版时必须改成目录驱动**（`Catalog.FunctionForConfigName`
已经存在，只是 `backend.go:66` 在用），硬编码表退化为兜底。

## 5. 错误码语义（决定成败分流）

| 码 | 文案/上下文 | 真正含义 | 处置 |
|---|---|---|---|
| `4001` | param is invalid | **该 function 不服务这个模型** | 换 function 或换号；不是账号问题 |
| `4011` | — | **该路由的限流**（不是"模型不存在"） | 退避，不要摘号 |
| `1005` + `extra.plan:4` | — | **模型存在，但当前档位不够**（订阅门禁） | 明确告诉用户"需订阅"，别当故障 |
| `40006` | no schema found | US 区未注册该端点 | 域不支持，不做重试 |
| `20405` | Device proof required | 新路径完全没带 proof | 回退旧契约 |
| `20403` | Token device not match | 带了 proof 但签名/公钥/绑定对不上 | 检查签名原文分隔符与私钥来源 |
| `10101` | 无效参数 | **ClientID 用错** | 检查是否 SOLO 包 |
| `20101` | refresh token is invalid | refresh token 本身失效 | 需重新登录 |

> `4011` 的语义在参考项目里被修正过一次：早期读成"该路由不服务此模型"，
> 后来拿到"在**正确** function 上也出现 4011"的样本，才确认是限流。
> 两者的区分靠 `4001`——它才是"这条路由不服务这个模型"。

## 6. token 续期：ClientID 按「是否 SOLO 包」分叉

当前契约（2026-10-11 实测，四版全部走）：

```
POST {凭证 host}/trae/api/v3/oauth/ExchangeToken
强制 DeviceProof（ECDSA P-256 / SHA-256，base64）
```

| edition | ClientID | PlatformCode |
|---|---|---|
| 桌面（CN / 国际） | `ono9krqynydwx5` | `IDE_PC` |
| SOLO（CN / 国际） | `en1oxy7wnw8j9n` | `SOLO_PC` |

**分叉依据是"是否 SOLO 包"，不是域。** 所以：
- `solo`（SOLO CN）**也必须**用 `en1oxy7wnw8j9n`。用桌面 id 会得到
  `10101 无效参数`。
- 参考实现记录：旧 `/cloudide` 路径今天对四版仍返回 200，但**同样必须用各自
  对应的 ClientID**；用错会得 `10101 refresh token is not matched to the client`。
  也就是说这个分叉属于 **OAuth client**，不属于 endpoint。
- **历史事故**：3.1.0 之前的插件在旧路径上写死桌面 id，导致国际 SOLO 账号
  "一旦过期就再也刷不回来"。

**本项目的现状**：`internal/trae/oauth.go` 只有一个
`DefaultClientID = "ono9krqynydwx5"`，全场景使用。**国际 SOLO 账号会刷不动**，
且报错是 `10101`，看上去像参数问题而不像 ClientID 问题。落地时必须按账号
是否 SOLO 包选 ClientID。

### 6.1 DeviceProof 签名细节（缺一不可）

- 签名原文：`["POST", path, ClientID, RefreshToken, String(ts秒), nonce].join("\n")`
  —— **分隔符是换行符**。用空格连接会被拒成 `20403`，与"密钥不对"表现
  完全一致，极易误诊。
- 私钥来源：`storage.json` 的 `iCubeAuthInfo://icube-dc:<deviceId>`，值与凭证
  同样加密，**用同一个解密器**即可解出
  `{"privateKeyPEM":…,"publicKeyPEM":…}`。⇒ 证明设备的密钥就存在它守护的
  那份登录旁边，无需 keychain。
- 签名编码是 **P1363（r||s 固定 64 字节）**，DER 会被拒成 20405。

本项目 `internal/trae/proof.go` 这三条**都已经是对的**（换行连接、P1363、
PascalCase + number 时间戳），无需改动。

### 6.2 DeviceInfo 必填性（实测梯度）

| 字段 | 必填 | 备注 |
|---|---|---|
| `DeviceID` | ✅ | |
| `PlatformCode` + `DeviceType` | ✅ | 二者**同时**存在才通过；去掉任一 → 20403 |
| `MachineID` | ❌ | 去掉仍 200 |
| `ClientVersion` | ❌ | 去掉仍 200 |
| `DevicePublicKey` | ❌ | 签名已证明身份；但按官方形态带上 |

成功响应除 `Token` / `TokenExpireAt` / `RefreshToken` 外还有
`RefreshExpireAt`、`BoundDeviceID`、`DeviceBindStatus:"BOUND"`、`UserJwt`。

**回退策略**：先试新契约（带 proof），失败再试旧 `/cloudide`；**没有 keypair 时
直接走旧契约**，不发注定被 20405 拒绝的请求。

## 7. 签到：国际域没有该能力，由账号返 `Skipped`

参考实现实测（2026-09-24）：`/trae/api/v2/ug/*` 全族在**国际版网关上不存在**——
`growsg-normal.trae.ai` / `api-sg-central.trae.ai` / `api.trae.ai` 均 404，
`www.trae.ai` 回落 HTML 首页。

所以：

- **国际账号照常进入签到轮转**，不要在上层（调度器/池）按域过滤——这与
  CodeBuddy 的处理一致：`codebuddy` 的 global 账号也照常参与签到，只有
  **企业版**才跳过。上层过滤会让账号在面板上"神秘消失"，用户看不出原因。
- 过滤发生在 `Account.Checkin()` 里，且**返回 `Skipped` 而不是 `ErrUnsupported`**：

  ```go
  if x.realm == "global" {
      return &gateway.CheckinResult{Skipped: true, Message: "国际版无签到体系"}, nil
  }
  ```

  两者的语义差别是硬约定（`gateway/backend.go` 的注释）：
  - `Skipped` = 「这个账号正常，只是这类账号不参与」→ 池按正常流程记账，
    **面板能读到**（`panel.go` 的 `checkin_done`），可以显示「该区域无签到」；
  - `ErrUnsupported` = 「这个上游没实现」→ 池静默跳过，**用户什么也看不到**。

  国际版账号是正常的 Trae 账号，只是那个域没有签到端点，属于前者。
  这与 CodeBuddy 企业版的写法（`Skipped: true` + 中文说明）逐字对齐。
- 本项目的签到是**单点排程**（`config.json` 的 `schedule.checkin_enabled` +
  `checkin_backends`，按上游）。国际版是同一上游的不同域，所以**不需要新增
  开关**——`checkin_backends` 是按上游的，Trae 整个上游一个开关，域差异由
  账号自己回答。

## 8. 用量与积分：数据源不同构，不能复用

| | `cn` | `global` |
|---|---|---|
| 端点 | `POST /trae/api/v2/pay/web_user_ent_usage` | `POST /trae/api/v1/pay/ide_user_pay_status` |
| 语义 | 积分包明细（按到期批次） | 订阅 / 试用状态 |
| 关键字段 | `user_entitlement_pack_list[]` | `has_package` / `trial_status{…}` / `is_dollar_usage_billing` / `enable_solo_*` / `pay_identity_priority_list` |

- `ide_user_pay_status` 的 `body` 是 `{}`。
- 国际版**没有消费积分这个概念**（订阅制），所以 `gateway.Credits` 那套
  "可用额度 / 到期批次"在国际域意义不大，面板应按订阅状态渲染不同区块。
- **与本文同批修复的"积分只认实测"约束在这里同样成立**：国际版目录里
  没有 `consumption_rate`，更没有可估算的积分。国际账号的积分列只能显示
  "—"或订阅状态，**绝不要退回按 token 估算**。

## 9. 与 CodeBuddy 的 cn/global 处理对齐

本项目的域模型是由 CodeBuddy 先落地的，Trae 的 global 域**必须与它同形**，
否则同一个 `global` 在两个上游上语义不同，`pool` / 面板 / 用量账本都会被撕裂。
下表是逐项比对（✅ = 方案已对齐，⚠️ = 需要改，❌ = 明确不同且应保持不同）。

| 维度 | CodeBuddy 现状 | Trae 方案 | |
|---|---|---|---|
| 域值 | `cn` / `global` | 同 | ✅ |
| 域来源 | 凭证 `realm` 字段，回落到 `domain` 后缀（`isGlobalDomain`） | 凭证 `userRegion.region`，回落到 host 后缀 | ✅ 同一套「凭证优先、host 兜底」 |
| 域归属 | **账号属性**（`auth.Auth.Realm()`） | 需要改成账号属性（现在按 `trae.json` 的 base 判） | ⚠️ 见清单 #1-#3 |
| 持久化 | `auth` 文件里存 `realm`，`BackfillRealm` 补写，`RealmStored` 可直读 | 建议 `Account` 存 `Host` / `UserRegion`（凭证原始值），域是派生 | ✅ 更保守：存原始值不存推导结果 |
| base 分派 | `ChatBaseFor(realm)` / `BillingBaseFor(realm)`，方法级分派 | 同名方法已存在但**忽略 realm 参数** | ⚠️ 见清单 #4 |
| 域开关 | `global.enabled`（缺省 true，false = 纯 CN 锁定逃生门） | 同形：`trae.json` 的 `global.enabled` | ✅ 形状对齐；**不要**做全局单例（CodeBuddy 那个是单上游时代的设计） |
| 目录分流 | `Catalog()` 内按 `Realm()` 分端点，池缓存 `catalogGroup{backend,realm}` | 同 | ⚠️ provider 内的两个包级单例缓存要先改成按 realm（清单 #5） |
| 签到 | global 账号**照常签到**；只有企业版返 `Skipped` | 国际域无签到端点 → 返 `Skipped`（**不是** `ErrUnsupported`） | ✅ 同为 `Skipped` 语义 |
| 补偿任务 | global 跳过（`CompSkipGlobal`，「国际版无 CN 补偿体系」） | Trae 无补偿任务，不适用 | ❌ 不适用（Trae 整体没有该能力） |
| 域级调度策略 | `strategy.domestic` / `strategy.international` + accountOrder | 同一处逻辑，pool 层自动生效 | ✅ 无需改动 |
| 单账号在途上限 | `pool.max_in_flight_global` | 同上，自动生效 | ✅ 无需改动 |
| 面板登录 | `loginEndpoints(realm)` 按域切三端点 + Origin | 国际版授权页是 `www.trae.ai/authorization` | ⚠️ 见清单 #7 |
| 用量记录 | 账本按 `(backend, realm)` 记 | 同，`acc.Realm()` 返回 `global` 即可 | ✅ 无需改动 |
| 生成 model ID | `panelPrefixFor` → `<realm>:<model>` | 现在对 trae 硬编码 `trae:cn:` | ⚠️ 见清单 #10 |

**唯一的实质性结构差异**是「域开关的载体」：CodeBuddy 的逃生门是包级
`atomic.Bool`（`auth.SetGlobalEnabled`），因为它是唯一的上游。Trae 若照抄就会
出现两个互不相干的全局开关。应改为**各自配置文件的 `global.enabled`**，
由各上游自己在 `Realm()` 里读取——形状与 `codebuddy.json` 一致，但作用域是
单上游。

> 这也回答了「上游平权」约束是否被违反：`global.enabled` **不是**「启用某个
> 上游」，而是「是否允许路由到该上游的 global 域」。CodeBuddy 的同名字段就是
> 这个语义，文档里写得很清楚（`codebuddy/config.go` 的注释）。

## 10. 落地清单（按本项目文件）

**状态：1–11 全部已实现**（2026-11-16）。下表保留原始设计意图，
「实际实现」列记录落地时与计划不同的地方——那些差异都是实现时才暴露的。

| # | 文件 | 改动 | 实际实现 |
|---|---|---|---|
| 1 | `internal/trae/config.go` / `icube.go` | `Account` 增加 `Host` / `UserRegion` / `PackageType`；从 storage.json 的 `iCubeAuthInfo://icube.cloudide` 解密带入 | ✅ 新增 `ParseLoginClaim` / `LoginClaim` |
| 2 | `internal/trae/region.go`（新） | 域判定 + US 第三态 | ✅ `ServiceRegion` / `RegionOfCredential` / `RegionOfHost` / `RegionOfUserRegion` |
| 3 | `internal/backend/trae/account.go` | `Realm()` 改用账号自身的域；US 不进池 | ✅ `realmOfBase` 降级为**老账号兜底**（不删：老账号文件没有 host 字段） |
| 4 | `internal/trae/config.go` | 加 `Global GlobalRealm`（含 `Enabled`）；`ChatBaseFor` / `APIBaseFor` 真正按 realm 分派 | ✅ `Enabled` 用 `*bool`（见下方说明） |
| 5 | `catalog.go` / `remote_models.go` | 两个包级单例缓存改为按域分表 | ✅ `map[realm]`，键由 `catalogCacheKey` 统一给出 |
| 6 | `internal/trae/payload.go` | function 归属改为目录驱动 | ✅ 新增 `chatFunctions` 表；硬编码表**改为 `solo_work_remote`**（原为 `solo_agent`，那个不能聊天） |
| 7 | `internal/trae/oauth.go` | ClientID 按「是否 SOLO 包」选 | ✅ `oauthClientIDFor(acc)`；`OAuthAuthorizeURL` **未**按域（见下方说明） |
| 8 | `credits.go` / `backend/trae/account.go` | 国际域走 `ide_user_pay_status`；`Checkin()` 返 `Skipped` | ⚠️ 签到已改（`Skipped` + 说明）；**用量接口分离未做**（见下方说明） |
| 9 | `client.go` / `remote_models.go` | 域为 global 时不发 CN 专用头 | ✅ `Package-Type` / `X-Os-Version` / `Origin` 按域切换 |
| 10 | `internal/panel/panel.go` | `panelPrefixFor` 改为 `trae:<realm>:` | ✅ |
| 11 | `internal/trae/errors.go` | 补 `1005` / `4011` 语义 | ✅ `4011` 本已正确；`1005` 文案区分「需订阅」与「额度用尽」 |

### 10.1 实现时的三处调整

**① `Global.Enabled` 用 `*bool` 而不是 `bool`。**
`codebuddy.json` 里是 `bool` + `Default()` 显式置 true，因为它的默认配置由代码
生成。Trae 的默认 `trae.json` 是空对象（`configs.go` 写 `{}`），`bool` 零值
false 会让升级后**所有国际账号静默停用**。指针能区分「没写」与「显式 false」，
缺省在 `GlobalEnabled()` 里按 true 处理。

**② `realmOfBase` 没删，降级为老账号兜底。**
计划里写「删掉」，但升级前导入的账号文件没有 `host` / `user_region` 字段。
删了它们会全部掉到 CN 默认值——行为上没错，但那是**猜**的，不如保留按配置
base 推断（就是改造前的行为）。新导入的账号走凭证判定。

**③ 用量接口分离（`ide_user_pay_status`）没做。**
国际域的 `Credits()` 仍会走国内的 `web_user_ent_usage`，返回 404 后按常规
错误处理。原因是**没有国际账号可以验证响应解析**——把订阅状态硬塞进
`gateway.Credits` 的「额度/到期批次」形状是猜的，而本项目刚因「按 token 估算
积分」踩过猜测的坑。当前表现：国际账号的积分列取不到值（显示"—"），
不影响对话。要做需要一台真实国际账号采样响应。

**④ 授权 URL 未按域。**
`OAuthAuthorizeURL` 仍是 `www.trae.cn/authorization`。国际账号的授权页是
`www.trae.ai/authorization`，但在**服务端部署**场景下这条链路本来就依赖用户
手工粘贴回调，改域只影响用户点开哪个页面。落地时按账号域切换即可，属低风险
待办——真正的门槛是国际账号首次登录，那需要人操作。

### 10.2 不能做的事

- ❌ 把国际版做成新上游 `trae-global`（协议相同，不是新上游）
- ❌ 引入 `ai` 作为第三个域值（本项目已有 `global`）
- ❌ 用 `base` 域名反推账号域（一个配置文件只能是一个域）
- ❌ 让 US 凭据回退成 `global`（会 401 且污染熔断）
- ❌ 在调度器/池里按域过滤掉国际账号的签到（与 CodeBuddy 不一致——它的
  global 账号照常签到；过滤让账号在面板上神秘消失，原因也不可见）
- ❌ 用 `ErrUnsupported` 表示「国际域无签到」（那是「上游没实现」的语义，
  面板读不到；应返 `Skipped` + 说明）
- ❌ 给 Trae 再造一个全局 `globalEnabled` 单例（CodeBuddy 那个是单上游时代的
  设计；两个上游各一个全局开关会互相矛盾，应放进各自的配置文件）
- ❌ 国际账号按 token 估算积分（没有 `consumption_rate`）

## 11. 待验证

**已在本地验证（2026-11-16，构造三个域的账号跑真实 Load+Wrap）**：

```
配置里账号数=3，进池=2
  cntest  host=https://api.trae.cn             userRegion=CN → region=cn
  sgtest  host=https://growsg-normal.trae.ai   userRegion=SG → region=global
  ustest  host=https://grow-normal.traeapi.us  userRegion=US → region=us
  进池: cntest realm=cn / sgtest realm=global
  WARN: 账号 ustest 属于未支持的区域 "us"，已排除出调度池
```

即：域判定、US 排除、逃生门、旧版平铺 `trae.json` 的向后兼容都已验证。

**仍未验证（需要真实国际账号）**：

| 项 | 现状 | 怎么验 |
|---|---|---|
| `llm_utils_chat` 在国际域的端到端可用性 | 参考实现已验证（2026-09-15，A+：`gpt-5.4` 经 `coresg-normal` 返回完整 SSE）；本项目**尚未用真号发过** | 用国际账号发一次最小请求 |
| 国际域用量接口（`ide_user_pay_status`）的解析 | **未实现**（见 §10.1 ③）。现为 404 → 积分列显示"—" | 拿到真实响应后按实际字段实现 |
| 国际域目录的真实阵容 | 只验证了缓存分表与归属收敛，没有真的拉过国际目录 | 用国际账号刷新一次面板目录 |
| 国际 CLI（`~/.trae`）的默认 host | **未验证**，参考实现第一阶段禁用国际 CLI | 取证后放开 |
| `custom_model_*` BYOK 条目是否该进目录 | display 名多为空，疑似模板 | 合并时按"display 名非空才收" |
| 国际 remote 目录 ctx 与 `get_detail_param` 数值差异（272k vs 240k） | 两源差异比 CN 大 | 回归测试锁"wire 值优先" |
| 付费档位 `plan` 数值与 `pay_identity_priority_list` 的对应 | 只见过 `plan:4`，**推断非实测** | 需一台已订阅账号采样 |

> 参考实现"付费档位探测"的结论值得记住：**不存在"付费专属接口"**。模型目录与
> 聊天走同一组端点，付费改变的是同一份响应里的授权标志与调用时的错误码（`1005`）。
> 所以不要为付费账号加新端点。

## 12. 一个容易混的相邻概念：企业版

参考实现还区分了 `TraePanel`（`cn` / `enterprise` / `ai`），其中 `enterprise`
是**同一份 CN 凭据的第二个聊天网关**（`console.enterprise.trae.cn`），租户账号
走它时记在企业池上、不动个人 `consumed_amount`。它有两条必须遵守的线上差异：
请求必须带 per-call `session_id`，且被拒时回 **HTTP 200 + 首个 SSE frame 是
`event:error` code 4001**（不看 body 就发现不了）。

**它不是域，也不是上游**——是"同一凭证的第二条计费通道"。本项目目前不做；
真要做时它是 `trae.json` 的一个开关 + base，不要塞进 realm 枚举。

## 13. 来源

- `dsh/docs/INTL_SUPPORT_PLAN.md` —— 实施方案与模块清单
- `dsh/docs/INTL_SG_EVIDENCE.md` —— 证据矩阵（host / refresh 契约 / 目录 / usage）
- `dsh/docs/INTL_PAID_TIER_PROBE.md` —— 付费档位探测（无付费专属接口）
- `dsh/docs/ISSUE66_DIAGNOSIS.md` —— US 区误路由与 401 被包成 500
- `dsh/docs/USAGE_API_RESEARCH.md` —— CN 用量端点族
- `dsh/src/region.ts` / `model-order.ts` / `solo.ts` —— 区域常量与 function 优先级
