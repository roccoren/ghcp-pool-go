# Deployment Guide: SDK Timeout Fix

## Summary
Fixed critical issue where API calls with tool calls (especially web_search) would hang for >10 minutes instead of timing out properly.

## Changes Made
- **File**: `internal/gateway/copilot_http.go`
- **Commit**: `39378ec` - "Fix SDK session timeout hang with tool calls"
- **Tests**: ✅ All gateway tests pass
- **Build**: ✅ Docker build validated

## Deploy to Production

### Option 1: Via Azure Container Registry (Recommended)

```bash
# 1. Push the commit to GitHub
git push origin main

# 2. Build and push to ACR
az acr build \
  --registry ghcppoolacr0f60a504 \
  --image ghcp-pool-go:v0.1.7-timeout-fix \
  --image ghcp-pool-go:latest \
  .

# 3. Update Container App
az containerapp update \
  --name ghcp-pool-go-vnet \
  --resource-group ghcp-pool-rg \
  --image ghcppoolacr0f60a504.azurecr.io/ghcp-pool-go:v0.1.7-timeout-fix

# 4. Verify deployment
az containerapp revision list \
  --name ghcp-pool-go-vnet \
  --resource-group ghcp-pool-rg \
  --query "[].{Name:name,Active:properties.active,Created:properties.createdTime}" \
  -o table
```

### Option 2: Via GitHub Container Registry (CI/CD)

The `.github/workflows/docker-publish.yml` workflow will automatically build and publish when you push to main:

```bash
# 1. Push to main
git push origin main

# 2. Wait for workflow to complete
gh run list --workflow=docker-publish.yml --limit 1

# 3. Deploy the new image
az containerapp update \
  --name ghcp-pool-go-vnet \
  --resource-group ghcp-pool-rg \
  --image ghcr.io/roccoren/ghcp-pool-go:latest
```

### Option 3: Quick Local Test Build

```bash
# Build locally
docker build -t ghcp-pool-go:timeout-fix .

# Test locally
docker run -p 8080:8080 \
  -e GHCP_BACKEND=fake \
  ghcp-pool-go:timeout-fix
```

## Verification After Deployment

### 1. Check health endpoint
```bash
curl https://gh.hereis.app/health
```

### 2. Monitor logs for timeout behavior
```bash
az containerapp logs show \
  --name ghcp-pool-go-vnet \
  --resource-group ghcp-pool-rg \
  --follow
```

Look for:
- ✅ Requests completing within 180s (for web_search)
- ✅ No "waiting for sdk session: context deadline exceeded" after reasonable time
- ❌ No >10 minute hangs

### 3. Test with web_search tool call
```bash
curl -X POST https://gh.hereis.app/v1/chat/completions \
  -H "Authorization: Bearer YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.5",
    "messages": [{"role": "user", "content": "Search the web for latest news"}],
    "tools": [{"type": "web_search"}],
    "stream": true
  }'
```

Expected behavior:
- Request completes or times out within 180 seconds
- No indefinite hang
- Proper error message if timeout occurs

## Rollback Plan

If issues arise, rollback to previous revision:

```bash
# List revisions
az containerapp revision list \
  --name ghcp-pool-go-vnet \
  --resource-group ghcp-pool-rg \
  -o table

# Rollback to previous revision
az containerapp revision activate \
  --name ghcp-pool-go-vnet \
  --resource-group ghcp-pool-rg \
  --revision <PREVIOUS_REVISION_NAME>
```

## Technical Details

See `docs/fix-sdk-timeout-hang.md` for complete technical explanation.

**Key changes:**
1. Timeout now always enforced (60s default, 180s for web_search)
2. Tool settle phase has 5s fallback timeout
3. Context cancellation properly managed

**Impact:**
- Prevents >10 min hangs
- Faster failure detection
- Better resource cleanup
- No breaking changes to API behavior
