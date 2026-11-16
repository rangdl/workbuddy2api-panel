# 已弃用功能

记录已**停止维护且已从代码中移除**的功能，以及仍在维护的部分。

> 本页最近更新：2026-11-14（三条协议路径已完整删除，不再是「保留可用」状态）

## 状态总览

| 功能 | 弃用日期 | 代码状态 | 说明 |
|---|---|---|---|
| Responses / Codex 接入（`/v1/responses`） | 2026-11-13 | **已删除** | 端点返回 404 |
| Anthropic / Claude Code 接入（`/v1/messages`） | 2026-11-13 | **已删除** | 端点返回 404 |
| 第三方 OpenAI 兼容上游（`/tp/v1/*`） | 2026-11-13 | **已删除** | 端点返回 404 |
| Trae 上游（`/trae/v1/*`） | — | **继续维护** | — |

**弃用原因**：作者改用 [new-api](https://github.com/Calcium-Ion/new-api) 做上游中转，
上述三项的职责由 new-api 承担。

## 已完成的移除（2026-11-14，commit `52bcb38`）

删除的包：
```
internal/responses/       Responses ⇄ Chat 协议转换
internal/anthropic/       Anthropic Messages ⇄ Chat 协议转换
internal/responsesstore/  previous_response_id 增量会话存储
internal/thirdparty/      OpenAI 兼容第三方上游转发
internal/inlinethink/     仅被上面两个协议包引用
```

删除的端点（现在返回 404）：
```
POST /v1/responses
POST /v1/messages · POST /v1/v1/messages
POST /tp/v1/chat/completions · /tp/v1/responses · /tp/v1/messages
GET  /tp/v1/models
```

同时删除：
- 面板「协议接入」视图（`index.html` 的 `#view-clients`）与四个配置接口
  （`responses_config` / `anthropic_config` / `thirdparty_config` / `thirdparty_test`）
- 三份示例配置（`responses.example.json` / `anthropic.example.json` / `third_party.example.json`）
- 三份设计文档（`responses-api-plan.md` / `anthropic-messages-plan.md` / `third-party-upstream-plan.md`）
- `ensureAllConfigs` 不再生成这三份配置

## 保留的部分

- **`POST /v1/search/messages`**：联网搜索端点，走 Anthropic Messages 兼容形态，
  但**不依赖** `internal/anthropic`（它只构造响应体，不做请求转换）。继续维护。
  注意：该端点用的是 `h.withAuth`（Bearer），不涉及 `x-api-key`。

## 遗留死代码（待清理，非功能问题）

- `httpauth.VerifyAnthropic`（`internal/httpauth/httpauth.go:24`）：**零调用**。
  它原本服务于 `/v1/messages`，端点删除后已无使用者。搜索端点走 `VerifyBearer`。
- `internal/trae/cooldown.go` 的 `Cooldowns` 表：`Mark` 零调用，表恒为空。
  Trae 的冷却已由账号池统一承担（`Pool.Cooldown`），这张表只剩面板展示壳。
- `gateway.Backend` 的 `Accounts` / `Supports` / `WireModel` / `Credits` 四个方法：
  生产零调用（余额与装载都走账号对象）。
