# Copilot SDK 后端的结构化输出

在底层 SDK 完全没有结构化输出能力的前提下，`response_format` 是如何被兑现的、
网关承诺了什么、以及这个承诺到哪里为止。

## 问题

网关对外提供 OpenAI 兼容接口，因此客户端会发来：

```json
{ "response_format": { "type": "json_schema", "json_schema": { "schema": { ... } } } }
```

在本方案之前，SDK 后端对此的处理是在 prompt 末尾追加一句
`"Respond with valid JSON only."`。调用方提供的 schema **被整个丢弃**，并且无论
上游返回什么都以 `200` 应答。也就是说，一个要求严格四十字段 schema 的请求，和一个
只写 `json_object` 的请求，得到的是同样一句七个词的提示，而且没有任何迹象表明
schema 已被忽略。

## 约束

Copilot SDK 是**智能体运行时**，不是补全 API。它的发送面只有：

```go
type MessageOptions struct {
    Prompt         string
    DisplayPrompt  string
    Attachments    []Attachment
    Mode           string
    AgentMode      AgentMode
    RequestHeaders map[string]string
}
```

这里没有输出格式字段，`SessionConfig` 上也没有，而
`AssistantMessageData.Content` 是裸 `string`。

这是**协议层**的缺失，不是 Go 绑定的疏漏：各语言绑定的线格式同样不携带输出
schema，且该接口从 SDK v1.0.0 到 v1.0.9-preview.3 未发生变化。上游以
[github/copilot-sdk#41](https://github.com/github/copilot-sdk/issues/41) 追踪此
需求，目前仍处于 open；维护者已明确回复当前无法在 `session.send` 上传入 schema。
**升级 SDK 解决不了这个问题。**

Copilot CLI 是同一套运行时，同样没有对应能力。它的 `--output-format json` 控制的
是 CLI **自身的事件封装**，而非模型的回答。

## 设计

SDK 中接受调用方提供的 JSON Schema 的地方，有且只有一处——工具的参数 schema：

```go
type Tool struct {
    Name        string
    Description string
    Parameters  map[string]any // JSON Schema，会原生传给模型
    Handler     ToolHandler
}
```

因此本方案不把调用方的 schema 当作「输出格式」，而是把它当作**一个合成工具的入参
schema**。工具 schema 走的是模型原生的 function-calling 通道，schema 由此成为对模
型真实生效的约束面，而不是一段可以被无视的提示文本。

这同时解释了为什么该方法在各模型族上表现一致：Claude 与 Gemini 在 Copilot API 层
并无原生 `response_format`，但所有受支持的模型族都支持 function calling。

仅靠 schema-as-tool 并不充分——模型可能拒绝调用该工具，也可能调用了但参数不合规。
因此整体设计为**四层**，每一层兜住上一层漏掉的情况。

## 机制

### 第 1 层 — Schema as tool

对于 `json_schema` 请求，网关声明如下工具：

```go
sdk.Tool{
    Name:        "ghcp_structured_output",
    Description: "Return the final answer to the user. Call this tool exactly once, " +
                 "with arguments matching its JSON Schema. This is the only way to " +
                 "deliver the answer.",
    Parameters:  spec.Schema,
}
```

`Handler` **刻意留空**。SDK 的语义是：handler 为 nil 时只暴露工具声明、不自动执
行。于是模型发起该工具调用后，由后端捕获并中止会话。**模型填入的参数即为答案。**

prompt 中给出配套指令，让模型知道该工具就是交付通道：

> Deliver the final answer by calling the `ghcp_structured_output` tool exactly
> once, with arguments conforming to its JSON Schema. Do not answer in prose and
> do not wrap the arguments in markdown.

随后将该工具调用「提升」为普通回答，并对客户端隐藏：

```go
if args := strings.TrimSpace(call.Arguments); args != "" {
    result.Content = args
}
result.ToolCalls = kept // 剔除合成工具调用
if result.FinishReason == "tool_calls" {
    result.FinishReason = "stop"
}
```

客户端看到的是普通 content 与 `finish_reason: stop`，合成工具**永远不会**出现在
`tool_calls` 中。

### 第 2 层 — 提取

模型可能不调用工具而直接在正文作答，且常把 JSON 包在 markdown 围栏里。
`extractJSONPayload` 先剥除围栏，再以**字符串与转义状态感知**的方式扫描首个配平的
JSON 值——字符串字面量内部的结构性字符不会干扰括号深度计数。

### 第 3 层 — 校验

使用 `github.com/google/jsonschema-go`（已随 SDK 间接引入）对载荷做**真正的 schema
校验**，而非「能否解析」：

```
{"name":"ada"}                    -> required: missing properties: ["age"]
{"name":"ada","age":"x"}          -> /properties/age: has type "string", want "integer"
{"name":"ada","age":36,"extra":1} -> unexpected additional properties ["extra"]
```

编译后的 schema 缓存在有界 LRU 中（512 条），单个 schema 上限 128 KB。缓存 key 是
**客户端可控**的 schema JSON，因此设界是硬性要求，不是调优选项。

### 第 4 层 — 修复，然后失败

校验失败时重试**一次**，并把校验错误作为纠正轮喂回：

```go
cause := validateStructuredOutput(result.Content, spec)
if cause == nil {
    return result, nil
}
repaired, err := b.chatSDK(ctx, model,
    structuredOutputRepairMessages(messages, result.Content, cause), params, stream)
...
if err := validateStructuredOutput(repaired.Content, spec); err != nil {
    return ChatResult{}, err
}
```

若修复轮仍不合规，请求显式失败。**不合规的回答永远不会以成功返回。**

两轮的用量会累加，因此发生修复的请求会如实计入其消耗。

## 接口契约

在 `POST /v1/chat/completions` 上以 `response_format` 接受，在
`POST /v1/responses` 上以 `text.format` 接受：

```json
{ "type": "json_object" }
{ "type": "json_schema", "schema": { ... } }
{ "type": "json_schema", "json_schema": { "name": "x", "strict": true, "schema": { ... } } }
```

若 `json_schema` 请求中没有可用的 schema，则降级为 `json_object` 而非报错——外层写
法有误不应阻断一个本可正常服务的请求。

## 行为规则

| 场景 | 行为 |
| --- | --- |
| 调用方声明了名为 `ghcp_structured_output` 的工具 | 拒绝请求。请求 `json_schema` 期间该名称为网关保留；复用会导致网关吞掉调用方自己的工具调用 |
| `tool_choice` 为 `none`、`required`、`any` 或强制指定函数 | 撤回合成工具，降级为「提示词 + 校验」。一个对客户端不可见的工具，不得用于满足客户端可见的 `tool_choice` 契约 |
| 模型返回了调用方的工具调用 | 以其为准；不强制结构化输出 |
| `json_object` 但顶层不是对象 | 拒绝，与 OpenAI 语义一致 |
| 调用方 schema 无法编译 | 返回客户端错误，而非静默跳过校验 |
| 调用方 schema 超过 128 KB | 拒绝 |
| 修复轮后校验仍失败 | 显式报错，并标记为不可重试，避免在账号池中换账号重放 |

## 流式

带 `response_format` 的请求**支持** `stream: true`，但**不是增量的**。token 一旦
发给客户端就无法再校验、更无法修复，因此这类请求会先完整跑完并校验，再一次性发出。

不带 `response_format` 的请求保持完整的逐 token 流式。实测：无 `response_format`
时 32 个 SSE data 帧，有 `response_format` 时 4 个。

## 实现位置

| 关注点 | 位置 |
| --- | --- |
| 解析、工具合成、提取、校验、修复 | `internal/gateway/structured_output.go` |
| 向 `Tools` 与 `AvailableTools` 声明工具 | `sdkCustomToolsFromParams`、`withStructuredOutputTool` |
| prompt 指令 | `sdkPrompt` |
| 提升与归一化 | `applySDKOutputConstraints` |
| 校验 / 修复 / 失败 | `chatSDKStructured` |
| 流式缓冲路径 | `chatStreamStructured` |
| 不可重试标记 | `pool.go` 中的 `isNonRetryableClientError` |

**有一处接线极易出错。** `SessionConfig.Tools` 与 `SessionConfig.AvailableTools`
是两条独立派生的链路，而白名单由 `sdkCustomToolsFromParams` 构建。因此把合成工具
声明在 `SessionConfig` 层，结果是**该工具进了白名单却从未被声明**，模型根本看不
到它。所有 `Tools` 的来源都必须经过 `withStructuredOutputTool`。

## 边界

这是 **steering + 校验**，不是 constrained decoding。模型并未在语法层面被硬约束，
合规性是靠**拒绝不合规输出**来保证的。对调用方而言两者结果等价——要么拿到合规载
荷，要么拿到明确错误——但失败时要多付一轮上游开销。

文档中不应将其表述为「模型层面的保证」。在当前后端上，这已是能拿到的最强契约。

## 验证

`scripts/structured-output-smoke.sh` 针对真实 Copilot 后端验证该契约：

```bash
scripts/structured-output-smoke.sh --gh-user <user> [--model MODEL] [--backend copilot|copilot-cli]
```

覆盖：schema 合规性、合成工具调用的隐藏、用量计费、无围栏的 `json_object`、嵌套
enum/array schema、流式、保留名冲突拒绝，以及 `/v1/responses` 的 `text.format`
路径。

已在 `gpt-5-mini`、`claude-haiku-4.5`、`gemini-3.5-flash` 上验证通过，且
`GHCP_BACKEND=copilot` 与 `GHCP_BACKEND=copilot-cli` 两种后端均通过。其中**非
OpenAI 族才是关键用例**：它们在 Copilot API 层并无原生 `response_format`。

## 后续

若 SDK 补上原生的输出 schema 字段，第 1、2 层即可退役，第 3、4 层退化为兜底。
跟踪 [github/copilot-sdk#41](https://github.com/github/copilot-sdk/issues/41)。
