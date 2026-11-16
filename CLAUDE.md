# 项目约定（每次会话自动加载）

**架构约束必读：[`docs/architecture.md`](docs/architecture.md)**

改动本项目前先读那一页。它规定了依赖方向、分层职责、可选能力的扩展流程，
以及历史上那几处分层例外的成因与修法（避免再用同样的方式绕）。
代码与那页冲突时以那页为准。

## 最容易漂移的几条（摘要，完整内容见 architecture.md）

- **池认识能力，不认识上游。** `internal/pool` 里不许出现
  `acc.Backend() == "trae"` 这类分支、不许 import `backend/*` 或 `internal/auth`。
  判断「能不能做某事」用类型断言到 `gateway` 的可选接口。
- **`gateway` 零 internal 依赖。** 加新抽象放这里，加具体实现放 `backend/*`。
- **流内错误由 backend 填 `StreamStats.Decision`。** server 侧不再按上游名
  分流分类器；缺 `Decision` 会 WARN 且不罚号（契约违约要可观测）。
- **上游专属的观测也走可选能力**：`StreamLifecycle` / `RemainObserver`，
  不要写成 `if acc.Backend() == ...`。
- **「上游」和「域」是两个正交维度，绝不可合并。** 域是 `cn` / `global`
  （同一域里可能同时有 CodeBuddy 和 Trae），上游是 `codebuddy` / `trae`
  （同一上游可能有多个域）。上游身份一律由 `acc.Backend()` 回答、域由
  `acc.Realm()` 回答——**不要另造一个"标签"往 realm 里塞**（历史上的
  `UsageLabeller` 就是这么把 `"trae"` 写进 realm，让面板「按域」冒出
  cn/global/trae 三行）。用量账本按 `(backend, realm)` 分别记账。
- **Trae 国际版是 `global` 域，不是新上游、也不是第三个域名。** 参考项目
  （dsh）内部管它叫 `ai` 桶，那是它的存储键名，与本项目的 `global` 是同一件事
  ——**不要把 `ai` 引进本仓库的域枚举**，也不要新建 `trae-global` 上游（国际版
  与国内版协议相同，只有 base/目录/用量源不同）。域是**账号属性**，由凭证的
  `userRegion` 判定，不能拿 `trae.json` 的 base 域名反推（那等于整份配置只能是
  一个域）。US 区真实存在但未支持，判定到它时必须显式排除，**不可回落成
  `global`**（会 401 且污染熔断判据）。详见 `docs/trae-intl.md`。
- **Trae 的两个域各有硬性行为差异**，不要用一套逻辑套到底：
  国际域**没有签到**（`/trae/api/v2/ug/*` 整族 404，`Checkin()` 返
  `Skipped:true` + 说明，**不是** `ErrUnsupported`——后者面板读不到原因）；
  目录缓存**按域分表**（两域阵容基本不重叠）；续期 ClientID 按**是否 SOLO 包**
  分叉而非按域（SOLO 用 `en1oxy7wnw8j9n`，用桌面 id 会回 `10101`）。
  `solo_agent` 能列模型但**不能聊天**（调用回 4011），目录归属必须收敛到
  `catalog.go` 的 `chatFunctions`。
- **积分只认实测，绝不按 token 推算。** Trae 的 `consumption_rate`（如 0.06）
  是**相对倍率**（IDE 里显示的 `x0.06`），不是「每 token 多少积分」；拿它乘
  token 数会虚高约 1500 倍。真实的逐请求扣费只有两种来源：上游在 usage 里
  直接给的 credit，或余额差（`credithist` 账本）。两者都没有就显示 "—"。
- **选号要校验「该上游真的有这个模型」**（`pool/gate.go` 的 `excludeNoModel`）。
  模型名按上游隔离，但 Trae 与 CodeBuddy 的账号 realm 都可能是 `cn`，不校验
  就会把 `cn:hy4free` 派给 Trae 账号打回「模型不存在」。判据读池里的目录缓存，
  **缓存冷时放行**——不能因一次目录拉取失败让整池不可用。
- **能用性判据只在 `pool/gate.go` 的 `gateLocked` 组合一次。**
  新增选号入口必须调它，不要自己拼条件。
- **模型目录是上游级数据**（每上游挑一个号拉一次），
  **特色能力是账号级数据**（逐号执行）。两者的缓存与失败语义相反。
- **目录缓存只许有一份，在池里。** 任何消费方都不要再加缓存。
- **失败处置只认 `Decision`。** 跨层分流用 `Decision.Policy`，
  绝不用 `Kind` 字符串反查（不同上游撞名）。
- **一个错误码可能承载多语义**（如 11140 = 封禁 / 内容审核 / 限流），
  必须逐语义分流；判定范围要含 `displayTips`，不能只读 `msg`。
- **多个不同账号短窗内接连出同类问题 = 平台级信号**，走 `server/wafip.go`
  的 `multiAcctGate` fail-fast 终止轮转，别继续换号撞风控。
- **排程是单点的。** 任何上游都不许有自己的定时器；签到只在
  `config.json` 的 `schedule.checkin_enabled`(总开关) + `checkin_backends`(按上游，
  缺键=启用) 里配。加了上游专属的 schedule/enabled 就会重新分裂。
- **上游平权，没有「启用某个上游」这回事。** 池里有该上游账号即参与调度。
  不要新增 `enabled` / 上游专属 base URL 这类字段。
- **加配置前先回答一个问题：这个字段只对某一个上游有意义吗？**
  - **是** → 进该上游的文件（`codebuddy.json` / `trae.json`）
  - **否** → 进 `config.json`

  三个必须避开的错觉：①「目前只有 A 在用就归 A」（用实现代替语义——Trae 的
  SSE 超时就因此缺失过）；②「A/B 取值不同就要分两份」（除非语义本身不同）；
  ③「放主配置更简单」（代价是主配置变成大杂烩，删上游时没人敢删字段）。
  完整逐项归属表见 `docs/config-layout.md`。**守门测试**：
  `cmd/server/config_layout_test.go`（放错位置会直接失败并提示该往哪放）。
- **上游配置一上游一文件**，文件内保留 `cn` / `global` 域维度。升级时旧
  `config.json` 的 `cn`/`global`/`upstream`/`features` 里的**上游专有字段**
  会自动迁移到 `codebuddy.json` 并从主配置删除（配置单一来源）。
- **传输层超时（timeout/header/idle）在 `config.json`，不进上游文件。**
  它们描述「一条 SSE 长连接该等多久」，与上游协议无关——两个上游的对话都是
  流式长连接。
- **定时任务都带抖动**（`scheduler/jitter.go`）：整点类只向后 0~10 分钟，
  周期类 ±15%。抖加在 `nextWake` 结果上，同小时多任务共享同一偏移。

## 验证口径

```bash
export PATH=/tmp/go/bin:$PATH      # 本机 go 在 /tmp/go/bin
go build ./... && go vet ./... && gofmt -l internal/ cmd/ && go test ./...
```

必须在仓库根执行（默认配置路径是相对的）。

## 其他文档

- `docs/config-layout.md` —— 配置与数据目录划分
- `docs/deprecated.md` —— 已移除的功能与遗留死代码
- `docs/trae-upstream-plan.md` —— Trae 上游协议分析
- `docs/trae-intl.md` —— Trae 国际版（`global` 域）接入：契约差异、域判定、
  US 区陷阱、目录分缓存、续期 ClientID 分叉、签到不可用
- `README.md` —— 面向用户的使用文档
