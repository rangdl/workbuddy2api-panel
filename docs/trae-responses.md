# Trae 对 Responses 协议的支持

**结论：已经支持，不需要额外开发。**

本文记录验证过程与几个仍需注意的边界，供后续排查时参考。

## 为什么天然支持

`/v1/responses` 的实现（`internal/server/responses.go`）是**协议转换 + 委托**：

```
POST /v1/responses
  → responses.ToChat(raw)            Responses 请求 → OpenAI Chat 请求
  → applyResponsesModelMap          模型名映射
  → h.chatCompletions(rw, nr)       复用主链路（含选号、换号、backend 分发）
  → newResponsesWriter 投影回 Responses 形态
```

关键在第三步：它把转换后的 Chat 请求**原样交给 `chatCompletions`**。而
`chatCompletions` 在选中非 CodeBuddy 账号时会走 `dispatchViaBackend`，
所以 Trae 自动继承——不需要单独的响应式端点，也不需要第二份转换逻辑。

这正是删除 `/trae/v1/responses` 后功能没有丢失的原因：那条端点是当初
「Trae 尚未接入统一调度」时的临时通路。

## 验证结果

用真实二进制 + 假 Trae 上游实测：

```bash
POST /v1/responses
{"model":"trae:cn:glm-5.2","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}
```

返回：

```json
{
  "object": "response",
  "id": "resp_chatcmpl-c28d1c21",
  "model": "glm-5.2",
  "status": "completed",
  "output": [{"role":"assistant","content":[{"type":"output_text","text":"Trae响应"}]}],
  "usage": {"input_tokens":10,"output_tokens":20,"total_tokens":30}
}
```

内容、id 前缀、状态、用量映射全部正确。

## 三个边界（都不影响基本可用）

### 1. 工具调用只有 CodeBuddy 的「标记修复」

`upstream.WithMarkupRepair` 处理的是 CodeBuddy 特有的行为：上游把工具调用
吐成正文标记，需要还原成 `delta.tool_calls`。

Trae 走的是原生结构化 `tool_calls`（`internal/trae/sse.go` 的
`convertToolCalls`），不需要修复。所以：

- Trae 的工具调用**已经可用**（走原生路径）；
- 但如果 Trae 将来也出现「正文标记」形态，需要在 `backend/trae` 的
  `Convert` 里单独接一个修复器。

**建议**：等真实 Trae 账号验证工具调用时留意一下输出形态。若发现正文里
出现 ` ```tool_call ` 之类的标记，再补。

### 2. `previous_response_id` 增量补全与上游无关

`responses.Store` 按 `response_id` 缓存 output items，`Fill` 时插回 `input`。
这套逻辑在**网关侧**，与哪个上游服务无关——Trae 同样受益。

唯一的注意点：`ResponseID()` 来自 `chatcmpl-` 前缀的 id。Trae 侧现在由
`Inbound.ChatID` 提供同一个 id（换号重试也保持一致），所以缓存键稳定。

### 3. 流式的事件形态差异

`responsesWriter` 把 OpenAI SSE 投影成 Responses 事件流（`response.created`
/ `response.output_text.delta` / `response.completed` 等）。

CodeBuddy 与 Trae 的 SSE 在进入 `responsesWriter` 之前都已被规范化成
OpenAI SSE（前者 `upstream.StreamTo`，后者 `trae.ConvertStream`），
所以投影层无差异。

**已实测**：流式与非流式都通过。

## 如果将来要补，改动点在哪

仅在第 1 点成立时才需要动：

| 场景 | 改动位置 | 预估 |
|---|---|---|
| Trae 也要标记修复 | `backend/trae/backend.go` 的 `Convert` 里包一层 repair | 小 |
| Trae 支持 Responses 原生协议（不走 Chat 中转） | 新增 `Backend.ChatResponses` 方法 + dispatch 分支 | 中（不建议，无收益） |

## 结论

**不需要排期。** 统一调度落地后，Responses 对 Trae 的支持是免费的副作用。
建议在真实 Trae 账号验证阶段顺带跑一次带 tools 的 Responses 请求，
确认第 1 点不成立即可彻底关闭这个话题。
