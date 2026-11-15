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
- **上游专属的观测也走可选能力**：`StreamLifecycle` / `RemainObserver` /
  `UsageLabeller` / `CreditEstimator`，不要写成 `if acc.Backend() == ...`。
- **能用性判据只在 `pool/gate.go` 的 `gateLocked` 组合一次。**
  新增选号入口必须调它，不要自己拼条件。
- **模型目录是上游级数据**（每上游挑一个号拉一次），
  **特色能力是账号级数据**（逐号执行）。两者的缓存与失败语义相反。
- **目录缓存只许有一份，在池里。** 任何消费方都不要再加缓存。
- **失败处置只认 `Decision`。** 跨层分流用 `Decision.Policy`，
  绝不用 `Kind` 字符串反查（不同上游撞名）。

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
- `README.md` —— 面向用户的使用文档
