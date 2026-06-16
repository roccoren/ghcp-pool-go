# ghcp-pool-go 简单使用说明

本文面向网关使用者，假设 ghcp-pool-go 已经部署完成，并且你已经拿到网关地址和 API key。

## 准备变量

```bash
BASE_URL=http://localhost:8000
GHCP_API_KEY=sk-change-me
```

如果使用线上 HTTPS 部署：

```bash
BASE_URL=https://<gateway-domain>
GHCP_API_KEY=<gateway-api-key>
```

所有推理接口都需要 API key。网关支持以下认证写法：

| 客户端风格 | Header |
| --- | --- |
| OpenAI | `Authorization: Bearer $GHCP_API_KEY` |
| Anthropic | `x-api-key: $GHCP_API_KEY` |
| Gemini | `x-goog-api-key: $GHCP_API_KEY` |

## 健康检查和模型列表

```bash
curl -sS "$BASE_URL/healthz"

curl -sS "$BASE_URL/v1/models" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

`/v1/models` 只返回当前 API key 有权限访问、且已被账号池发现的模型。

## OpenAI Chat Completions

```bash
curl -sS "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1",
    "messages": [
      {"role": "user", "content": "用一句话介绍 ghcp-pool-go"}
    ]
  }' | jq
```

OpenAI SDK 客户端可把 `base_url` 指向网关：

```text
base_url = http://localhost:8000/v1
api_key = <gateway-api-key>
```

## OpenAI Responses

```bash
curl -sS "$BASE_URL/v1/responses" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1",
    "input": "给我一个三步部署检查清单",
    "max_output_tokens": 256
  }' | jq
```

如果请求中使用 `"store": true`，可继续通过 Responses 相关接口读取、取消或删除响应：

```bash
RESPONSE_ID=<response-id>

curl -sS "$BASE_URL/v1/responses/$RESPONSE_ID" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

## Anthropic Messages

```bash
curl -sS "$BASE_URL/v1/messages" \
  -H "x-api-key: $GHCP_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-haiku-4.5",
    "max_tokens": 256,
    "messages": [
      {"role": "user", "content": "请用中文列出三个使用注意事项"}
    ]
  }' | jq
```

Token 估算：

```bash
curl -sS "$BASE_URL/v1/messages/count_tokens" \
  -H "x-api-key: $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-haiku-4.5",
    "messages": [
      {"role": "user", "content": "hello"}
    ]
  }' | jq
```

## Gemini 兼容接口

```bash
curl -sS "$BASE_URL/v1beta/models" \
  -H "x-goog-api-key: $GHCP_API_KEY" | jq

curl -sS "$BASE_URL/v1beta/models/gpt-4.1:generateContent" \
  -H "x-goog-api-key: $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {"role": "user", "parts": [{"text": "用 Gemini 格式请求一次模型"}]}
    ]
  }' | jq
```

## Embeddings

```bash
curl -sS "$BASE_URL/v1/embeddings" \
  -H "Authorization: Bearer $GHCP_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4.1",
    "input": ["hello", "world"],
    "dimensions": 8
  }' | jq
```

## 常用参数

| 参数 | 支持位置 | 说明 |
| --- | --- | --- |
| `model` | 所有推理请求 | 可使用上游模型 ID，也可使用管理员配置的模型别名。 |
| `stream` | Chat Completions、Responses、Messages | 按客户端协议返回流式响应。 |
| `context_tier` | Chat Completions、Responses、Messages | 对支持长上下文的模型可传 `"long_context"`。 |
| `reasoning_effort` | Chat Completions、Responses | 可请求 `low`、`medium`、`high` 等推理强度，具体取决于模型能力。 |

## 管理端常用入口

管理员 API key 需要包含 `admin` scope。基础查看命令：

```bash
curl -sS "$BASE_URL/admin/users" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS "$BASE_URL/admin/usage/aggregate" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq

curl -sS "$BASE_URL/admin/routes" \
  -H "Authorization: Bearer $GHCP_API_KEY" | jq
```

完整账号管理、模型刷新、模型别名、路由、限流、缓存和调试操作请参考 [`admin-operations.md`](admin-operations.md)。

## 常见问题

| 现象 | 排查方向 |
| --- | --- |
| `401 invalid or missing API key` | 检查是否传了网关 API key，而不是 GitHub/Copilot token。 |
| `403 missing scope` | 当前 API key 没有对应 scope；推理需要 `inference`，管理端需要 `admin`。 |
| 模型不在 `/v1/models` 中 | 模型可能未被当前账号发现、账号未启用，或当前 API key 的 `model_allow` 不包含该模型。 |
| `429` | 命中了全局或单账号 RPM 限流，按响应里的 `Retry-After` 等待后重试。 |
| SDK 模式下某些高级请求失败 | `sdk` 模式不会自动降级到直接 API；如确需直接 provider 行为，请由管理员评估切换 `GHCP_COPILOT_MODE=opencode`。 |
