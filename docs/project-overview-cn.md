# ghcp-pool-go 用户侧项目说明

ghcp-pool-go 是一个面向 GitHub Copilot 能力的高并发网关。它把一个或多个 Copilot 账号组织成可路由的账号池，并向客户端暴露 OpenAI、Anthropic 和 Gemini 兼容接口，让现有 AI 客户端以统一 API key 调用不同协议形态的模型。

## 核心优势

### SDK 优先

默认 Copilot 模式是 `sdk`，网关会为每个账号启动官方 `github.com/github/copilot-sdk/go` 客户端。

| 优势 | 说明 |
| --- | --- |
| 官方链路 | 认证、模型发现和 SDK 支持的简单聊天请求走官方 SDK 行为，减少协议漂移风险。 |
| 安全边界清晰 | `sdk` 模式不直接调用 Copilot API；SDK 不支持的请求形态会显式失败，而不是静默降级到直接 HTTP。 |
| 多租户友好 | SDK 默认运行在 empty mode，不暴露内置工具；需要 web search 时可通过 `GHCP_COPILOT_SDK_WEB_SEARCH_MODE=cli` 显式开启受控桥接。该桥接使用临时 CLI-mode SDK 会话，但由网关自己的 `ghcp_web_search` / `ghcp_web_fetch` handler 执行网络访问，避免触发 Copilot CLI 内置抓取工具的 egress allowlist 限制。也可以用实验模式 `native_cli` 直接暴露 Copilot CLI 原生 `web_search` / `web_fetch`，但其可用性和外网访问仍受 Copilot 服务侧策略控制。 |
| 模型发现一致 | 每个账号的模型列表由 SDK 发现并保存在模型注册表中，路由时按账号能力选择可用模型。 |

### API 兼容

网关同时提供多种客户端常用协议，便于把已有应用迁移到同一个入口。

| 客户端协议 | 入口 |
| --- | --- |
| OpenAI Chat Completions | `POST /v1/chat/completions` |
| OpenAI Responses | `POST /v1/responses` |
| OpenAI Embeddings | `POST /v1/embeddings` |
| Anthropic Messages | `POST /v1/messages` |
| Anthropic token count | `POST /v1/messages/count_tokens` |
| Gemini generate/count | `GET /v1beta/models`、`POST /v1beta/models/{model}:generateContent`、`POST /v1beta/models/{model}:countTokens` |

如果需要原生 Responses、Anthropic Messages、embeddings 或 SDK simple-chat 路径暂不支持的直接 provider 行为，可以把 `GHCP_COPILOT_MODE` 或 `gateway.copilot.mode` 设置为 `opencode`。该模式会走 OpenCode 风格的直接 Copilot provider API 路径，适合明确需要 API 兼容特性的场景。

### 账号池与运维能力

- 支持多账号池化、模型路由、模型别名和多种路由策略：`round_robin`、`least_busy`、`weighted`、`quota_aware`、`smart`。
- 支持全局和单账号 RPM 限流，命中限流时返回 HTTP 429 和 `Retry-After`。
- 内置 exact-match 缓存、SQLite 用量记录、健康检查、指标、调试抓包和管理接口。
- 支持 Azure Key Vault 运行时取密钥，容器内不需要保存 Azure 凭据或明文 Copilot token。

## 简易部署方法

### 1. 本地源码运行

本地假后端适合验证网关接口，不需要真实 Copilot token。仓库根目录已有示例 `config.yaml`，如果想使用内置零配置默认值，可让 `GHCP_CONFIG` 指向一个不存在的文件路径。

```bash
GHCP_CONFIG=/tmp/ghcp-pool-empty.yaml \
GHCP_BACKEND=fake \
go run ./cmd/ghcp-pool
```

真实 Copilot 后端需要 GitHub/Copilot token。默认建议使用 SDK 模式：

```bash
export GHCP_CONFIG=/tmp/ghcp-pool-empty.yaml
export GHCP_BACKEND=copilot
export GHCP_COPILOT_MODE=sdk
export GHCP_API_KEY="sk-change-me"
export GHCP_COPILOT_TOKEN="$(gh auth token)"

go run ./cmd/ghcp-pool
```

### 2. Docker 部署

镜像运行时只包含二进制，默认监听 `8000`，默认后端为 `fake`。生产或共享环境必须显式设置自己的网关 API key。

```bash
docker build -t ghcp-pool-go .

docker run --rm -p 8000:8000 \
  -e GHCP_BACKEND=copilot \
  -e GHCP_COPILOT_MODE=sdk \
  -e GHCP_API_KEY=sk-change-me \
  -e GHCP_COPILOT_TOKEN="$(gh auth token)" \
  ghcp-pool-go
```

常用环境变量：

| 变量 | 作用 |
| --- | --- |
| `GHCP_HOST` / `GHCP_PORT` | 监听地址和端口，容器默认 `0.0.0.0:8000`。 |
| `GHCP_BACKEND` | `fake` 或 `copilot`。 |
| `GHCP_COPILOT_MODE` | `sdk` 或 `opencode`，默认 `sdk`。 |
| `GHCP_API_KEY` / `GHCP_API_KEYS` | 客户端调用网关使用的 API key，多个 key 可用逗号或空白分隔。 |
| `GHCP_ADMIN_API_KEY` / `GHCP_ADMIN_API_KEYS` | 可选的独立管理员 API key。 |
| `GHCP_COPILOT_TOKEN` | 网关访问 Copilot 上游所需的 GitHub/Copilot token。 |
| `GHCP_USAGE_SQLITE_PATH` | 用量统计 SQLite 文件路径。 |
| `GHCP_MODEL_MAP_PATH` | 模型别名持久化文件路径。 |

### 3. Azure Container Apps + Key Vault

推荐共享部署把网关 API key 和 Copilot token 放入 Azure Key Vault，并让 Container App 通过托管身份读取密钥。仓库提供了可直接使用的 Bicep 模板：

```bash
RG=ghcp-pool-rg
LOCATION=eastus
STACK=ghcp-pool-private
IMAGE=ghcr.io/<owner>/ghcp-pool-go:latest

export GHCP_API_KEY_VALUE="sk-$(openssl rand -hex 24)"
export GHCP_COPILOT_TOKEN_VALUE="$(gh auth token)"

az group create -n "$RG" -l "$LOCATION"

az stack group create \
  -g "$RG" \
  -n "$STACK" \
  --template-file infra/containerapp-keyvault.bicep \
  --parameters \
    location="$LOCATION" \
    containerImage="$IMAGE" \
    gatewayApiKey="$GHCP_API_KEY_VALUE" \
    copilotToken="$GHCP_COPILOT_TOKEN_VALUE" \
  --action-on-unmanage detachAll \
  --deny-settings-mode none
```

完整私有网络、Key Vault 私有端点、RBAC 和多账号配置请参考 [`deploy-containerapp-keyvault.md`](deploy-containerapp-keyvault.md)。

## 部署后验证

```bash
FQDN=localhost:8000
GHCP_API_KEY=sk-change-me

curl -sS "http://$FQDN/healthz"
curl -sS "http://$FQDN/v1/models" \
  -H "Authorization: Bearer $GHCP_API_KEY"
```

如果部署在 HTTPS 域名下，把 `http://$FQDN` 替换为 `https://$FQDN`。
