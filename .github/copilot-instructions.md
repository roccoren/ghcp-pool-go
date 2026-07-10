# Copilot backend notes

- `GHCP_BACKEND=copilot` uses the official `github.com/github/copilot-sdk/go` SDK path only.
- `GHCP_BACKEND=copilot-cli` uses the SDK in `ModeCopilotCli` only.
- `GHCP_COPILOT_MODE` / `gateway.copilot.mode` must be `sdk`; `opencode` is rejected.
- Do not document or suggest direct Copilot HTTP access or `copilot_base_url` configuration.
- Under the SDK/CLI backends, `POST /v1/embeddings` is unsupported and returns HTTP 501.
