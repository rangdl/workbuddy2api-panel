# 架构约束手册

> 本页是**改动前的必读约束**，不是设计说明书。它回答一个问题：
> 「我要加/改一个功能时，代码应该放哪、不该放哪、哪些边界不能碰。」
>
> 事实基线：2026-11-15，commit `d9561ba`（模型目录收进池之后）。
> 代码与本页冲突时，**以本页为准并修正代码**——除非你同时更新了本页并说明理由。

---

## 一、一句话架构

```
HTTP 接口（server）
   └─ 选号/轮换/记账 ──▶ 账号池（pool）──▶ 上游无关抽象（gateway）◀── 各上游实现（backend/*）
                                                                         │
   定时任务（scheduler）──▶ pool 的可选能力 ─────────────────────────────┘
   管理面板（panel）──────▶ pool 的快照 API（不直连上游）
```

**核心原则：池认识「能力」，不认识「上游」。**

`internal/pool` 里不该出现 `if acc.Backend() == "trae"`、`switch backend`、
`import backend/trae` 这类写法。判断「这个号能不能做某事」的唯一方式是
**类型断言到 `gateway` 的可选接口**。新增一个上游时，只要它的 `Account` 实现了
对应接口，池、调度器、面板的全部批量流程自动生效，一行不改。

---

## 二、依赖方向（硬约束）

实际依赖图（`go list` 导出，非推测）：

```
                    ┌───────────────────────────┐
                    │  internal/gateway         │  不 import 任何 internal 包
                    └───────────────────────────┘
                       ▲        ▲          ▲
         ┌─────────────┘        │          └──────────────┐
         │                      │                         │
   internal/pool          internal/backend/*        internal/server
         │                      │                         │
         │                      ├── codebuddy → internal/{auth,upstream,logfmt}
         │                      └── trae      → internal/trae
         │
         └── internal/logfmt

   internal/scheduler ──▶ pool, gateway, upstream, logfmt
   internal/panel     ──▶ pool, gateway, auth, upstream, trae, …
                          （不 import backend/*：包装与凭证访问经装配期注入）
   cmd/server         ──▶ 以上全部（唯一的装配点）
```

### 允许的依赖

| 从 | 到 | 说明 |
|---|---|---|
| `pool` | `gateway` | 只经由接口 |
| `backend/*` | `gateway` | 实现 `Backend` / `Account` / 可选能力 |
| `server` | `pool`, `gateway`, `backend/*` | 见「已知例外 ②③」 |
| `scheduler` | `pool`, `gateway` | 批量任务只经池的批量入口 |
| `panel` | `pool`, `gateway` | 运维操作落到池的既有入口 |
| `cmd/server` | 全部 | **唯一的装配点** |

### 禁止的依赖

- ❌ `gateway` → 任何 internal 包（它是抽象层，必须零依赖）
- ❌ `pool` → `backend/*`（池不认识具体上游）
- ❌ `pool` → `upstream`（会让积分保底这类策略与 CodeBuddy 协议耦合）
- ❌ `backend/codebuddy` → `backend/trae`（两个上游互不认识）
- ❌ `scheduler` → `backend/*`（调度器只跑池的批量能力）
- ❌ 任何包 → `cmd/server`（反之不成立）

### 已知例外（真实存在，不要当成范本照抄）

原来有三处违反上述依赖方向的历史遗留，**其中两处已在 2026-11-15 修掉**。
保留这段记录是因为它们的成因有代表性，将来别再用同样的方式绕。

**① `pool` 依赖 `internal/auth` —— 已修（commit 待补）**

原状：`pool/adapter.go` 的 `authAdapter` 直接持有 `*auth.Auth`，
`pool/state.go` 的 `AuthByUID` 返回 `*auth.Auth`，`UnwrapAuth`/`WrapAuth` 也在这里。
结果是池事实上认识 CodeBuddy 的凭证类型。

修法：
- `pool/adapter.go` 拆成 `pool/bare.go`（上游无关的 `bareAccount` + 占位工厂）
- CodeBuddy 的适配器搬到 `backend/codebuddy/placeholder.go`
- `pool.AuthByUID` 删除，需要凭证的调用方改走 `codebuddy.UnwrapAuth(acc)`
- `cmd/server` 用 `SetPlaceholderFactory` 注入占位账号（必须在 `RestoreFromSnapshot` 之前）
- 池的测试经 `pool/shim_test.go`（仅测试构建）重导出 `WrapAuth`，241 处调用点无需改动

验证：`go list -f '{{join .Imports "\n"}}' ./internal/pool | grep internal/auth` 为空。

**残留**：`pool/bare.go` 里还定义着 `BackendCodeBuddy` 常量，`persist.go:142,314`
用它判定「落盘键是裸 uid 还是 `backend:uid`」。这是 `state.json` 的**数据格式**
向后兼容（旧文件的键属于 CodeBuddy），不是上游逻辑分支——池不认识 CodeBuddy 的
任何能力，只认识这个键名。要清理需要一次显式的 state 迁移，不应顺手改。

**② `server` 按上游名分流流内错误 —— 已修（commit 待补）**

原状：`applyStreamError` 用 `acc.Backend() == "trae"` 分到两套分类器
（`applyTraeStreamError` / CodeBuddy 的 `applyErrorPolicy`），加第三个上游
就要加第三个分支。根因是 `StreamStats` 只带 `ErrorCode/ErrorMsg`，没带
「已分类的 Decision」。

修法：`StreamStats` 增加 `Decision *Decision` 字段，**由 backend 在 `Convert`
里填**（CodeBuddy 按 error 帧原文分类，Trae 按事件流业务码分类）。
server 侧只剩一个 `applyStreamError`，统一走 `applyFailure`；缺 `Decision`
时打 WARN 且不罚号（契约违约要可观测，但不能凭猜测罚号）。

**③ `server`/`panel` 直连具体 backend —— 部分已修**

- `panel` → `backend/*`：**已消除**。改为装配期注入三个闭包
  （`WrapAccount` / `CredentialFor` / `TraeCredential`），见 `panel.Config`。
- `server` → `backend/codebuddy`：**仍然存在，且是合理的**。
  `server/backends.go` 要构造带降级 `RetryHook` 的 CodeBuddy backend——降级状态机
  与提示词属于网关策略，不能放进 backend 包。这是「网关策略需要注入到 backend」，
  不是「server 认识上游协议」。
- `server` → `backend/trae`：**已消除**（随 ② 一起）。

**④ `server` 里的上游专属观测 —— 已改为可选能力**

原先 `server/dispatch.go` 有三处 `acc.Backend() == "trae"`：在途计数、
余额上报、用量分组。它们不是处置逻辑，但同样把上游名硬编码进了 server。
现改为账号可选能力：`StreamLifecycle` / `RemainObserver`，由 Trae 的账号
自己实现（统计实例经 `SetStats` 装配注入）。

其中两处后来被**删除**，因为把它们做成"能力"本身就是错的：

- **`UsageLabeller`**（用量记到哪个域）：域和上游是两个正交维度，分别由
  `Realm()` / `Backend()` 回答，两个都已有权威来源。再让账号自报一个标签，
  实际结果就是 Trae 把 `"trae"` 写进 realm，面板「按域」冒出 cn/global/trae
  三行。现在用量账本按 `(backend, realm)` 分别记账（`usage.bucket.Backend`）。
- **`CreditEstimator`**（按 token × 倍率估算扣费）：倍率 `consumption_rate`
  是**相对倍率**（IDE 显示的 `x0.06`），不是每 token 积分数。乘 token 数没有
  语义依据，实测虚高约 1500 倍。真实扣费只认上游实测 credit 或余额差；
  没有观测就显示 "—"，不编数字。

**仍然保留的一处**：`server/handler.go:410` 的 `catalogPrefix` 用
`backend == "trae"` 决定模型 ID 前缀是三段式还是两段式。这是**面向客户端的
路由协议**（`trae:cn:x` vs `cn:x`），格式由消费方契约决定，不是上游协议差异，
放在 server 里是合适的。

---

## 三、分层职责

### `internal/gateway` —— 抽象层（唯一无 internal 依赖的包）

定义**所有跨上游的概念**。加新概念时先问：两个上游都需要它吗？是 → 放这里。

| 文件 | 内容 |
|---|---|
| `account.go` | `Account`（身份 + 三项必备能力）、`QualifiedUID`、`CredentialUnwrapper` |
| `backend.go` | `Backend`（装载/转换/发送/回程）、`Inbound`、`Outbound`、`StreamStats`（含 `Decision`）、`Usage`、`Credits`、`CheckinResult`，以及观测类可选能力 `StreamLifecycle` / `RemainObserver`（`UsageLabeller` / `CreditEstimator` 已移除，原因见上方第 ④ 条） |
| `capability.go` | 可选特色能力：`Traveler` / `Blindboxer` / `StreakRunner` / `BlackcatRunner` / `Compensator` / `ActivityReporter` / `Freshener` |
| `catalog.go` | `CatalogModel` / `Cataloger` / `ModelRate` |
| `decision.go` | `Decision` / `Action` / `Policy` / `Failure` / 控制流哨兵错误 |
| `modelref.go` | 模型名前缀协议 `ParseModelRef` |
| `registry.go` | `Registry`（上游名 ↔ Backend，含别名与启用判定） |

**两种能力的区别（最容易搞错的地方）：**

| | 必备能力（`Account` 上的方法） | 可选能力（独立接口） |
|---|---|---|
| 谁能做 | 所有上游都该有 | 某个上游独有的玩法 |
| 不支持时 | 返回 `ErrUnsupported` | **不实现接口** |
| 池的行为 | 遍历全部账号逐个调用 | 类型断言，未实现则静默跳过 |
| 例子 | `Credits` / `Checkin` / `Keepalive` | `Traveler` / `Cataloger` |

**新增可选能力的标准流程**（4 步，都不要碰 pool 的主流程）：
1. `gateway/<name>.go` 定义接口 + 归一结果结构体（字段注明「空 = 未知」而非「= 0」）
2. `pool/<name>.go` 写批量调度：`for _, m := range p.Members(...) { if x, ok := m.Acc.(gateway.X); ok { ... } }`
3. 各上游账号在自己的包里实现（`backend/trae/xxx.go`）
4. `cmd/server/main.go` 若需装配期注入，用 `Set*` 函数接进来

### `internal/pool` —— 调度状态机

只管「这个号此刻能不能接这个请求」和「失败了怎么处置」，**不认识上游协议**。

| 文件 | 职责 |
|---|---|
| `pool.go` | 结构定义、构造、`Set*` 注入、在途租约（`Acquire`/`Release`） |
| `gate.go` | **可用性判据的唯一事实来源**（`gateLocked`），见下 |
| `pick.go` | 选号：成本分层 + 快过期加权 + 全冷却兜底 + 在途过滤 |
| `state.go` | 状态迁移：`Disable` / `Revive` / `Pause` / `Cooldown` / `NoteError` / `NoteSuccess` |
| `operations.go` | 必备能力的批量调度：`RefreshCredits*` / `CheckinAll` / `KeepaliveAll` |
| `capabilities.go` | 可选能力的批量调度（旅行/盲盒/连登/夜猫/补偿/活跃） |
| `catalog.go` | 模型目录调度：**按 (上游, 域) 而非按账号** |
| `members.go` | 快照出口 `Members` / `AccountsByBackend`（替代只对 CodeBuddy 有效的 `AuthByUID`） |
| `modelview.go` | 模型级锁定画像（`/status` 与面板用） |
| `persist.go` | state.json + Redis 快照的恢复与落盘 |

**「可用性判据只在 `gate.go` 组合一次」是硬约束。**
`gate.go` 文件头记录了历史事故：同一套判据曾在十处各写一遍，
`WeightedAvailableUIDsForModelRealm` 漏了积分保底，导致触底号被分配给新会话，
表现为「先粘一个死号再解绑」的抖动。**新增任何选号入口，必须调 `gateLocked`，
不要自己组合条件。**

判据的四个维度（顺序有意为之：先便宜的过滤，最后成本与租约）：
`tried` → `backend` → `realm` → `健康口径` → `积分保底` → `在途名额`

### `internal/backend/{codebuddy,trae}` —— 上游实现

一个上游一个包，把该上游的**全部协议差异**关在包内：
请求体转换、出站头、SSE 形态、错误码表、目录拉取、特色能力实现。

- `backend.go`：实现 `gateway.Backend`（`Chat` / `Convert` / `RetryHook`）
- `account.go`：实现 `gateway.Account` 与可选能力
- `decision.go`：实现错误码 → `gateway.Decision` 的映射
- `catalog.go`：实现 `gateway.Cataloger` / `ModelRate`
- `features.go`（仅 codebuddy）：特色能力实现

**约束：`Chat` 不得改写 `in.Raw`。** 同一个 `Inbound` 会被多个 backend 依次尝试
（跨上游回退），原地修改会污染后续尝试。要改就复制。

### `internal/server` —— HTTP 接口与调度编排

- `handler.go`：路由、鉴权、请求解析、**轮转主循环**（`chatCompletions`）
- `dispatch.go`：把请求交给 `gateway.Backend`，处理失败与记账
- `backends.go`：注册表接入与 CodeBuddy 兼容垫片
- `resolve_model.go`：模型名前缀解析

**轮转主循环的唯一职责**是：选号 → 调 backend → 按 `Decision` 决定「换号 / 停止」。
请求体的协议处理**必须**在 backend 内完成。

⚠️ **两条装配路径必须一致**：生产走 `cmd/server/main.go` 显式注入 Registry，
测试走 `ensureCodeBuddyBackend` 兼容垫片。`server/backends.go:50` 记录了事故：
`main` 若用 `backendcb.New`（不带 `Retry`），生产的 `RetryHook` 恒为 nil，
降级重试完全失效，而测试全绿。**改装配时两条路径都要看。**

### `internal/scheduler` —— 定时任务

只做两件事：按时点触发、把 `pool` 的批量能力跑起来。
**不实现任何上游逻辑**，账号侧的能力经可选接口调用。

排程类型：签到 / 活跃上报 / 猫猫旅行（含到站定时器）/ token keepalive /
夜猫子 / 成长任务 / 余额刷新。

**开关命名约定**：配置项用「禁用」命名（`checkin_disabled`），零值 = 启用，
与引入开关前的行为逐字一致。代码里 `Start*(ctx, interval)` 在 `interval <= 0`
时直接 return，不启动 goroutine。

**排程是单点的：任何上游都不许有自己的定时器。**
签到曾经有两套开关（`config.json` 的总开关 + `trae.json` 的 `schedule`），
结果是「总开关开了但某个上游还单独关着」的分裂状态，用户没法判断哪份生效。
现在只有一处：

- `schedule.checkin_enabled` —— 总开关，控制定时任务跑不跑
- `schedule.checkin_backends` —— 按上游的开关，控制跑的时候签哪些上游。
  **键缺席 / nil / true 都算启用**，只有显式 `false` 才跳过

为什么是「缺席即启用」而不是「缺席即禁用」：新增上游时忘了配这张表，正确行为
是照常签到；若反过来，一整类账号会静默停止签到，账号慢慢饿死且没有任何报错。

上游列表由 `pool.Backends()` 从**池里实际存在的账号**导出，不读配置：
配置里列着但没账号的上游跑一轮是纯空转，而新加账号的上游即使配置没提也必须参与。

**所有定时触发都带随机抖动**（`scheduler/jitter.go`）：整点类只向后加 0~10 分钟，
周期类 ±15%。原因见该文件头——毫秒精确的整点是最强的机器特征之一。抖动加在
`nextWake` 的**最终结果**上而不是 `nextFire` 内部，这样同一小时的多类任务
共享同一个偏移，不会被拆成几批。

### 上游平权：没有「主上游 / 可选上游」的等级

所有上游一律平等：**账号池里有该上游的账号 → 它就参与调度**。不存在
「在配置里启用某个上游」这回事。

因此不要新增这类字段：`enabled`、`trae.enabled`、以及任何上游专属的
「开关」「base URL」。判断某上游是否可用只看 `pool` 里有没有它的账号。

`trae.json` 因此只保留**上游协议常量与凭证策略**（OAuth 常量、IDE 版本、
签到 UA、`verify_claim` 等）——这些是上游改版时需要覆盖、且无法从别处推导的。
对话 base、签到排程、启用开关都已从中移除。

### 上游配置：一上游一文件

CodeBuddy 的配置也不在 `config.json` 里，而是 `codebuddy.json`（见上条同样的理由）。

| 文件 | 归谁 |
|---|---|
| `config.json` | 与上游无关：监听、密钥、账号池策略、排程 |
| `codebuddy.json` | CodeBuddy：cn/global 两域 base、超时、UA、版本、脱敏 |
| `trae.json` | Trae：cn 域 base、IDE 版本、签到 UA、OAuth 常量 |

**文件内保留域维度**（`cn` / `global` 各一段）。不要为了"少一层嵌套"把两域
base 合成一个——跨域回退（issue #135）依赖两域可分别寻址，域隔离是硬约束。

**传送层参数不按域拆，也不进上游文件**：超时、SSE 首字节/空闲上限属于传输层，
所有上游共用一份（在 `config.json` 的 `upstream` 段）。它们描述的是「一条 SSE
长连接该等多久」，与上游协议无关——两个上游的对话都是流式长连接。

这条有实际教训：Trae 的首字节超时曾被硬编码、流中空闲完全没有监控，根因就是
它被默认成"CodeBuddy 的参数"。判断标准是**语义是否与上游无关**，不是"目前谁在用"。

**新增一个上游配置的判断标准**：这个字段只对某一个上游有意义吗？
是 → 放进该上游的文件。两个以上上游都要用 → 才考虑进 `config.json`。
按这个标准，`sanitize_blacklist_fingerprints` 属于 CodeBuddy（Trae 没有那套
逐字审核），`schedule.*` 属于主配置（所有上游共用同一个排程）。

### `internal/panel` —— 管理面板

内嵌单文件前端（`go:embed`），无外部构建依赖。

**只经 `pool` 的既有入口做运维**：`Revive` / `Disable` / `Pause` / `Remove` /
`CheckinOne` / `RefreshCreditsOne`。添加账号走 `auth.SaveAtomic` + `pool.Add`，
重启后与 `auths/` 目录天然对齐。

前端契约字段名是**两套**，不要混：
- `/v1/models`（客户端）：`reasoning_supported_efforts` / `reasoning_default_effort`
- `/panel/api/models`（面板）：`supported_efforts` / `default_effort`

两套映射层各留一份，但**共用同一份数据与同一个选号逻辑**（都来自 `pool.CatalogAll`）。

---

## 四、一次请求的完整链路

```
POST /v1/chat/completions
  │
  ├─ ServeHTTP          鉴权、请求日志埋点
  ├─ chatCompletions    读 body（无大小上限）、提取 declaredTools / 会话键 / 轮级键
  │                     ├─ 模型名解析：resolveModelFull → gateway.ModelRef（backend/realm/bare）
  │                     ├─ 黑名单拦截（models.hidden）
  │                     ├─ 提示词改写（prompt.Rewrite / Append，按 PromptMode）
  │                     └─ outbound 模型名重写为 bareModel
  │
  └─ for i := 0; i < MaxRotate; i++          ← 轮转循环
       ├─ 选号：粘性号优先（PickByUIDForBackend），否则 PickForBackend
       │        跨域回退（裸名 + 配置开启时）
       ├─ dispatchViaBackend(acc, ...)
       │    ├─ beginStreamStat(acc)          ← StreamLifecycle 可选能力（在途计数）
       │    ├─ Acquire 在途租约
       │    ├─ Freshener 预刷新（可选能力）
       │    ├─ be := backendFor(acc)         ← 注册表按 acc.Backend() 反查
       │    ├─ be.Chat(ctx, acc, in)         ← Inbound.Raw 只读
       │    │    └─ 失败 → *gateway.Failure（已带 Decision，调用方不二次分类）
       │    ├─ out.Convert(emit)             ← 各上游自己的 SSE 规范化
       │    │    └─ 流内错误 → StreamStats.Decision（**由 backend 填**）
       │    └─ Release 租约
       │
       └─ 按 Decision：
            ├─ Rotate=false → backendStop（换号无意义，例如请求体畸形）
            └─ Rotate=true  → backendRetry（applyFailure 处置账号后继续）
```

HTTP 层失败与流内错误走**同一个** `applyFailure`：两者都产出 `gateway.Decision`，
server 不需要知道错误来自哪一层、哪个上游。

**`Inbound` 字段的责任划分**（`backend.go` 有逐字段注释）：
- 公共：`Raw` / `Ref` / `Stream` / `ClientIP` / `SessionKey` / `ChatID`
- **`ChatID` 在轮转外生成一次**：换号重试必须产出同一个 id，否则客户端把重试
  结果当成新消息。
- 协议特化（CodeBuddy 用、Trae 忽略）：`ConversationRequestID` / `TraceID` /
  `DeclaredTools` / `FrameHint`

---

## 五、失败处置：Decision 的语义

`gateway.Decision` 是 backend 对「这次失败该怎么办」的**唯一权威答案**。
`server` 不再按错误码或 `Kind` 字符串二次判断。

| 字段 | 含义 | 注意 |
|---|---|---|
| `Action` | 对账号做什么（冷却/禁用/模型级避让/降权） | 枚举见 `decision.go` |
| `Rotate` | 换号是否可能成功 | **与 `Action` 独立**：模型不可用时 Action 罚但 Rotate=false |
| `Cooldown` | 覆盖默认时长（上游给 `Retry-After` 时填） | 0 = 用 Action 的缺省值 |
| `Kind` | 原始分类名 | ⚠️ **不要用它做跨层判定**，不同上游会撞名 |
| `Policy` | 由哪套策略处置（`PolicyCodeBuddy` / `PolicyGeneric`） | 按上游分流**只认这个字段** |

**`Policy` 存在的理由**（`decision.go` 有完整记录）：CodeBuddy 的 `applyErrorPolicy`
有 12 类精细分支（6004 墙钟对齐、11102 指数负缓存、softStreak 封顶），通用
`applyDecision` 只有固定时长。用 `Kind` 反查会因撞名把 Trae 失败送进 CodeBuddy 策略
（冷却放大约 10 倍、`session_dead` 的 reason 写成 12153）。

**新增上游时必做**：在 `backend/<name>/decision.go` 里给自己的每个错误标
`Policy: PolicyGeneric`（或新增一个 Policy 常量），不要留零值靠猜。

---

## 六、两条「上游级 vs 账号级」的分界线

这是最容易写错的地方，两条规则相反：

| 维度 | 调度单位 | 失败怎么办 | 缓存 |
|---|---|---|---|
| **特色能力**（旅行/签到/盲盒…） | **逐账号**（每号有自己状态） | 按能力自己的语义（`ErrUnsupported` = 跳过） | 不缓存 |
| **模型目录**（`CatalogAll`） | **(上游, 域)**，每上游挑一个健康号 | **不罚号**（目录失败 ≠ 对话不可用） | 成功 10 分钟 + 失败负缓存 5 分钟 |

**为什么目录不能逐号**：同一上游的任意健康号拉到的目录相同，遍历等于把同一个
上游请求打好几遍。`pool/catalog_test.go:41` 有一条断言锁死这点
（`upstream catalog calls = 1`，不是 2）。

**为什么目录失败不罚号**：`models` 拉取偶发 5xx 不代表该号的 chat 通道不可用。

**为什么缓存必须在池里**：改造前 server 与 panel 各有一份（CN 10 分钟 /
global 1 小时 / Trae 30 分钟），三份 TTL 不同，于是「面板显示的列表」与
「API 返回的列表」机制上就允许不一致。收进池后两者读同一个缓存，
一致性是结构性保证。**不要在任何消费方再加一份目录缓存。**

---

---

## 六之二、第三种维度：**平台级**（多个账号同时出问题）

前面两条分界线讲的是「上游级」与「账号级」。还有第三种粒度，加新上游时最容易漏：
**多个不同账号在同一时间窗内接连出同一类问题**。

此时问题既不在某个账号，也不在某次目录拉取，而在更外层——平台侧的风控批次动作。
按账号级逻辑逐个处置（禁用、换号）在语义上没错，但**轮转本身是有害的**：
一次客户端请求会放大 `MaxRotate` 倍，把剩下还健康的号挨个送去撞风控。

| 信号 | 判定 | 处置 |
|---|---|---|
| WAF 403（拦的是网关出口 IP） | 60s 内 ≥2 个不同 UID 命中 | 终止轮转，激活窗口内 fail-fast |
| 11140 账号受限（封号 wave） | 10 分钟内 ≥2 个不同 UID 命中 | 终止轮转，激活窗口内 fail-fast |

两者共用 `server/wafip.go` 的 `multiAcctGate`（窗口/阈值/标签由调用方传入）。
**只数不同 UID**：同一个号反复命中永不触发——那属于账号级偶发，交给冷却/禁用管。

**为什么放 `server` 而不是 `pool`**：这类状态的唯一消费者是轮转循环（决定
「还要不要继续换号」）。`pool` 是账号级记账层，跨 UID 的语义不属于任何账号。

**为什么不会误判也漏判**：受限账号会被 `Disable` 移出选号（`gateLocked` 过滤），
同一个号不会重复喂进状态机。所以窗内每个命中都代表「又一个**原本健康**的号被
平台标记了」——这正是平台级信号的准确定义。

### 11140 是「一个码承载多语义」，判定必须三条件

这是本项目最容易写错的一处，两个方向都会造成真实损失：

| 11140 的实际语义 | 正确归类 | 误判后果 |
|---|---|---|
| 账号被限制（`request illegal` / `账号存在异常行为`） | `ErrAccountFault` → 硬禁用 | 漏判成 `ErrClient`：不禁用只喂连败（5 次才降权 10 分钟），受限号每 10 分钟被放回来再打一遍，风控越滚越重 |
| 内容未通过审核（`内容未通过安全审核`） | `ErrContentBlocked` → 不罚号、降级重试 | 误判成账号受限：**硬禁用健康账号**（与 issue 1849 的 11-128 误伤同类） |
| 模型级限流（`rate-limiting requests`） | `ErrSoftRate` → 短冷却 | 误判成账号受限：把等一会儿就好的号永久摘掉 |

判据（`upstream.IsAccountRestricted`）：**业务码 11140 + 命中受限词表 + 不命中内容审核词表**。
`11-128` 的文案与 11140 完全相同，靠业务码分流，任何时候都不能只看文案。

**文案扫描范围包含 `displayTips`**：上游的真实说明常放在 `displayTips.{zh,en}`，
`msg` 可能只有一个通用码；且非 ASCII 可能以 `\uXXXX` 转义出现，直接子串匹配会漏。

## 七、资源与生命周期的单点

| 资源 | 唯一所有者 | 说明 |
|---|---|---|
| 账号状态（冷却/积分/禁用/暂停） | `pool` | 面板与调度器只经池的入口读写 |
| 在途名额 | `pool`（`Acquire`/`Release`） | 所有上游共用；`defer Release` |
| 模型目录缓存 | `pool` | 见上一节 |
| 运行期可变配置 | `livecfg.Holder` | 不可变快照 + atomic 指针，面板改完下一请求生效 |
| 会话粘性绑定 | `session.Router` | 绑定值是 `QualifiedUID`（`"backend:uid"`） |
| 请求日志/用量 | `reqlog` / `usage` | 在 `recordAttempt` 单点调用 |
| 凭证 | `auths/<backend>-<uid>.json`（0600） | 账号文件分文件存放，各写各的 |

**`QualifiedUID` 是跨上游的唯一标识。** 裸 `uid` 在 map 里作键会让两个上游同 uid
的账号互相覆盖（表现为「账号凭空消失」）。`pool.byUID` 的键恒为复合键。

---

## 八、加一个新上游：改哪些文件

按顺序，**pool 与 scheduler 原则上零改动**：

1. `internal/backend/<name>/` 新建包：
   - `backend.go` 实现 `gateway.Backend`（`Name` / `Aliases` / `Enabled` / `Chat` / `RetryHook`）；
     `Convert` 里遇到流内错误必须填 `StreamStats.Decision`
   - `account.go` 实现 `gateway.Account`；有特色能力就实现对应可选接口
   - `decision.go` 实现错误分类，**每个 Decision 标 `Policy`**
   - `catalog.go` 实现 `Cataloger` / `ModelRate`（如果上游能列模型）
   - `placeholder.go` 提供 `Placeholder(uid)`（池从 state.json 恢复时用）
2. `cmd/server/main.go`：
   - 构造 backend、`backends.Register(...)`
   - 把账号 `p.SyncToDir` 或 `p.Add` 入池
   - **在 `RestoreFromSnapshot` 之前**调 `SetPlaceholderFactory`，让新上游的占位账号可构造
   - 若面板要展示该上游的统计，装配期把统计实例注入账号（`SetStats` 之类），
     不要往 `server`/`panel` 里加 `if backend == "<新上游>"`
3. 配置：`trae.json` 这类独立配置文件（含开关、排程、base URL）
4. 文档：本页「已知例外」若有变化，同步更新

**不需要改的**：`pool/gate.go` 的判据、`pool/capabilities.go` 的批量调度、
`scheduler/*`、`server` 的轮转循环、`panel/*`。

### 新上游的检查清单

```bash
# 全部应输出 0（最后一条是 server 的合理依赖，见「已知例外③」）
go list -deps ./internal/gateway | grep -c 'workbuddy2api-panel/internal/'
go list -f '{{join .Imports "\n"}}' ./internal/pool      | grep -c 'internal/auth'
go list -f '{{join .Imports "\n"}}' ./internal/pool      | grep -c 'internal/backend'
go list -f '{{join .Imports "\n"}}' ./internal/panel     | grep -c 'internal/backend'
go list -f '{{join .Imports "\n"}}' ./internal/scheduler | grep -c 'internal/backend'
```

- [ ] 上面五条依赖断言仍成立
- [ ] `pool`/`server`/`scheduler`/`panel` 里没有新增 `acc.Backend() == "<新上游>"`
- [ ] `Chat` 不改写 `in.Raw`
- [ ] 每个 `Decision` 都标了 `Policy`
- [ ] `Convert` 在流内错误时填了 `StreamStats.Decision`
- [ ] 占位账号经 `SetPlaceholderFactory` 注入（不是在 `pool` 里加 import）
- [ ] `Realm()` 返回非空（缺省 `"cn"`）
- [ ] 上游专属观测（在途/余额/用量分组/积分估算）走可选能力接口，
      没有在 `server` 里加分支
- [ ] 新上游的错误码若**一个码承载多语义**（如 11140 = 封禁/内容审核/限流），
      分类器必须逐语义分流，不能按码一刀切、也不能只看文案
- [ ] 有「平台级封号」概念的上游，接入 `multiAcctGate` 的跨账号判定，
      别让轮转把健康号一个个送去撞风控

---

## 九、验证口径

改动后必须全绿：

```bash
export PATH=/tmp/go/bin:$PATH      # 本机 go 不在默认 PATH
go build ./...
go vet ./...
gofmt -l internal/ cmd/            # 必须无输出
go test ./...
go test -race ./internal/pool ./internal/server ./internal/panel
```

**必须在仓库根执行**：默认配置路径 `config/config.json` 是相对的。

---

## 十、这份文档自身

- 本页在 `docs/` 下，进版本库。
- 仓库根的 `CLAUDE.md` 是**指针**（指向本页），因为 `.gitignore` 忽略仓库根的
  `*.md`，只有 `docs/**/*.md` 放行。
- 修改架构约定时，**先改本页再改代码**，或同一次提交里一起改。
  代码与本页冲突时以本页为准。
