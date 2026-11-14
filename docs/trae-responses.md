# Trae 对 Responses 协议的支持（已过时）

> **⚠️ 本页内容已过时（2026-11-14）**：`/v1/responses` 端点与 `internal/server/responses.go`
> 已随 Responses 协议接入一并删除（见 `docs/deprecated.md`）。本文描述的实现路径
> 不再存在，仅保留作历史记录。

## 现状

- `POST /v1/responses` 返回 404（端点和实现已一并删除）
- `POST /trae/v1/responses` **同样不存在**：Trae 从未有过独立前缀，
  `9a55778` 起统一走 `/v1/*` + `trae:` 模型名前缀
- Trae 的对话能力通过 `POST /v1/chat/completions`（`model` 带 `trae:` 前缀）提供

## 历史记录（已失效）

原实现是「协议转换 + 委托」：

```
POST /v1/responses
  → responses.ToChat(raw)            Responses 请求 → OpenAI Chat 请求
  → applyResponsesModelMap          模型名映射
  → h.chatCompletions(rw, nr)       复用主链路（含选号、换号、backend 分发）
  → newResponsesWriter 投影回 Responses 形态
```

关键在第三步：它把转换后的 Chat 请求原样交给 `chatCompletions`，因此选中
非 CodeBuddy 账号时会自动走 `dispatchViaBackend` —— Trae 因而「天然支持」。

这套机制随 `/v1/responses` 一起被删除。如需重新引入，可参考本页的历史结构。
