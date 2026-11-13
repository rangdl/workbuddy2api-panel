# 已弃用功能

记录已停止维护但仍留在代码里的功能，以及后续移除时需要动的地方。

## 状态总览

| 功能 | 弃用日期 | 代码状态 | 计划 |
|---|---|---|---|
| Responses / Codex 接入（`/v1/responses`） | 2026-11-13 | 保留可用，不再演进 | 待移除 |
| Anthropic / Claude Code 接入（`/v1/messages`） | 2026-11-13 | 保留可用，不再演进 | 待移除 |
| 第三方 OpenAI 兼容上游（`/tp/v1/*`） | 2026-11-13 | 保留可用，不再演进 | 待移除 |
| Trae 上游（`/trae/v1/*`） | — | **继续维护** | — |

**弃用原因**：作者改用 [new-api](https://github.com/Calcium-Ion/new-api) 做上游中转，
上述三项的职责由 new-api 承担。

**为什么保留代码而不是直接删**：
1. new-api 不可用时是现成的退路；
2. 三者与仍在维护的 Trae 共用面板页面与部分配置装配逻辑，直接删牵动面大；
3. 当前已有未提交的大改动（前端拆分、配置目录重构），不宜叠加数千行删除。

---

## 已完成的弃用动作

- README 三个章节顶部加弃用横幅；
- README「核心能力」「视图表」「API 端点表」相关条目标注 ⚠️已弃用；
- 删除三份设计文档（`docs/responses-api-plan.md`、`docs/anthropic-messages-plan.md`、
  `docs/third-party-upstream-plan.md`）——它们是"计划"文档，与最终实现已有偏差，
  且功能已弃用，继续维护没有价值。

---

## 后续移除清单（未执行）

按依赖顺序排列，建议分三次提交，每次跑一遍全量测试。

### 第一步：第三方上游（`/tp/v1/*`）

| 动作 | 位置 |
|---|---|
| 删除包 | `internal/thirdparty/`（4 个文件） |
| 移除路由注册 | `internal/server/handler.go`（`ThirdParty` / `ThirdPartyClient` 字段与 `/tp/` 路由） |
| 删除转发实现 | `internal/server/thirdparty.go` |
| 移除配置装配 | `cmd/server/thirdparty_config.go` |
| 移除面板接口 | `internal/panel/panel.go` 的 `thirdparty_config` / `thirdparty_test` |
| 移除面板前端 | `internal/panel/js/config.js`（`tpInput` / `tpProviderRow` / `collectThirdPartyConfig` 等） |
| 移除面板 DOM | `internal/panel/index.html`「上游接入」页的第三方上游卡片 |
| 移除配置生成 | `cmd/server/configs.go` 的 `third_party.json` 默认值 |
| 移除迁移项 | `cmd/server/layout.go` 的 `legacyFiles` 条目 |
| 删除示例 | `third_party.example.json` |
| 清理文档 | README「第三方上游」整节 |

⚠️ 注意：「上游接入」页同时承载 Trae 配置，改 HTML 时不要把整页删掉。

### 第二步：Anthropic Messages（`/v1/messages`）

| 动作 | 位置 |
|---|---|
| 删除包 | `internal/anthropic/`（8 个文件） |
| 移除路由与转发 | `internal/server/handler.go`、`internal/server/anthropic.go` |
| 移除配置装配 | `cmd/server/anthropic_config.go` |
| 移除面板接口 | `internal/panel/panel.go` 的 `anthropic_config` |
| 移除面板前端 | `internal/panel/js/config.js` 的 `loadAnthropicConfig` / `renderAnthMapRows` / `collectAnthModelMap` |
| 移除面板 DOM | `internal/panel/index.html`「协议接入」页的 Anthropic 卡片 |
| 移除配置生成与迁移 | `cmd/server/configs.go`、`cmd/server/layout.go` |
| 删除示例 | `anthropic.example.json` |
| 清理文档 | README「Anthropic / Claude Code 接入」整节 |

### 第三步：Responses / Codex（`/v1/responses`）

| 动作 | 位置 |
|---|---|
| 删除包 | `internal/responses/`（约 20 个文件）、`internal/responsesstore/`（2 个文件） |
| 移除路由与转发 | `internal/server/handler.go`、`internal/server/responses.go` |
| 移除配置装配 | `cmd/server/responses_config.go` |
| 移除面板接口 | `internal/panel/panel.go` 的 `responses_config` |
| 移除面板前端 | `internal/panel/js/config.js` 的 `loadResponsesConfig` / `respMapRow` / `collectResponsesConfig` |
| 移除面板 DOM | `internal/panel/index.html`「协议接入」页 |
| 移除配置生成与迁移 | `cmd/server/configs.go`、`cmd/server/layout.go` |
| 删除示例 | `responses.example.json` |
| 删除整个视图 | 若 Anthropic 也已移除，「协议接入」页不再有任何内容 → 移除导航项、`TITLES` 条目、`go()` 分支、`view-clients` 段 |

⚠️ 注意：`internal/upstream/payload.go` 里的「原生工具调用标记还原」是为 Responses/Codex
客户端修的，但该逻辑在普通 Chat 链路上同样生效，**不要一并删除**。

---

## 仍需保留的相关能力

以下功能名字里带 `responses` / `third_party`，但**不属于弃用范围**：

- `internal/server/responses.go` 之外的 `reasoning_content` 回填（Chat 链路通用）；
- `internal/upstream/payload.go` 的工具调用标记还原（Chat 链路通用）；
- `internal/panel/js/*.js` 中的通用渲染逻辑（与接入无关）。
