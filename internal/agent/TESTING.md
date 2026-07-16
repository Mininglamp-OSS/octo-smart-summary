# Channel Access Control Tests

## Overview

This document describes how to run access control tests for the summary agent tools (`fetch_channel` and `peek_channel`).

## Test Files

### `tool_channel_access_cgo_test.go` (CGO Required)
Real integration tests using SQLite in-memory database to verify the actual `if !allowedSet[channel_id]` security branch in production code.

**Requirements:**
- `CGO_ENABLED=1`
- C compiler (gcc/clang)
- `gorm.io/driver/sqlite` (mattn/go-sqlite3)

**What it tests:**
1. **Access Denied**: Request channel-B when user only has access to channel-A → returns `"channel not accessible"` + error
2. **Access Allowed**: Request channel-A when user has access to channel-A → not denied by access control

This is the same approach as existing repo tests (`resolve_channel_test.go`, `fetch_archive_test.go`) which also use `//go:build cgo`.

### `tools_behavior_test.go` (No CGO Required)
Behavior tests that don't require SQLite:
- Message cache operations
- String truncation
- Keyword matching in `search_messages`

These tests always run in CI/CD without build dependencies.

## Running Tests

### Default (CGO Disabled, as in Current Environment)

```bash
# All tests - CGO tests are automatically excluded
go test ./...

# Agent package only
go test ./internal/agent/
```

The CGO tests (`tool_channel_access_cgo_test.go`) will be skipped with message:
```
?       github.com/Mininglamp-OSS/octo-smart-summary/internal/agent  [no test files]
```

This is **expected behavior** when `CGO_ENABLED=0` (the default).

### With CGO Enabled (Full Coverage)

```bash
# Install C compiler if not available:
# Ubuntu/Debian:
sudo apt-get update && sudo apt-get install -y build-essential

# Alpine:
apk add gcc musl-dev

# macOS:
xcode-select --install

# Run ALL agent tests including CGO integration tests
CGO_ENABLED=1 go test -tags cgo ./internal/agent/ -v

# Run ONLY the access control CGO tests
CGO_ENABLED=1 go test -tags cgo ./internal/agent/tool_channel_access_cgo_test.go -v
```

**Expected output with CGO enabled:**
```
=== RUN   TestFetchChannelTool_AccessControl
=== RUN   TestFetchChannelTool_AccessControl/AccessDenied_ChannelNotInAllowedSet
=== RUN   TestFetchChannelTool_AccessControl/AccessGranted_ChannelInAllowedSet
--- PASS: TestFetchChannelTool_AccessControl (0.01s)
    --- PASS: TestFetchChannelTool_AccessControl/AccessDenied_ChannelNotInAllowedSet (0.00s)
    --- PASS: TestFetchChannelTool_AccessControl/AccessGranted_ChannelInAllowedSet (0.00s)
=== RUN   TestPeekChannelTool_AccessControl
...
PASS
```

## Production Code Under Test

**tool_fetch_channel.go** (~lines 76-90):
```go
// Security: validate channel accessibility for system-injected uid
accessibleChannels, err := pipeline.GetUserChannels(ctx, uid, imDB)
if err != nil {
    return "", fmt.Errorf("get user channels: %w", err)
}

// Build set of accessible channel IDs
allowedSet := make(map[string]bool)
for _, ch := range accessibleChannels {
    allowedSet[ch.ChannelID] = true
}

if !allowedSet[req.ChannelID] {
    errResult := map[string]interface{}{
        "error":      "channel not accessible",
        "channel_id": req.ChannelID,
    }
    errData, _ := json.Marshal(errResult)
    return string(errData), fmt.Errorf("channel %s not accessible by user %s", req.ChannelID, uid)
}
```

**tool_peek_channel.go** has identical security logic.

## CI/CD Recommendations

### Option 1: Enable CGO in CI (Recommended)
Add gcc to your CI environment and run with CGO enabled:

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Install build dependencies
        run: sudo apt-get update && sudo apt-get install -y gcc
      
      - name: Run all tests with CGO
        run: CGO_ENABLED=1 go test -tags cgo ./...
        env:
          CGO_ENABLED: 1
```

### Option 2: Run CGO Tests in Separate Job
```yaml
jobs:
  test-no-cgo:
    runs-on: ubuntu-latest
    steps:
      - name: Run unit tests (no CGO)
        run: go test ./...
  
  test-cgo:
    runs-on: ubuntu-latest
    steps:
      - name: Install gcc
        run: sudo apt-get update && sudo apt-get install -y gcc
      
      - name: Run integration tests
        run: CGO_ENABLED=1 go test -tags cgo ./internal/agent/ -v
```

### Option 3: Docker-based Testing
```dockerfile
FROM golang:1.22-alpine AS test
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY . .
RUN CGO_ENABLED=1 go test -tags cgo ./...
```

## Manual End-to-End Testing

If you prefer to verify access control behavior manually:

```bash
# Start API with real environment
cd ~/octo-smart-summary
go build ./cmd/summary-api
./summary-api

# Test with real user token
curl -X POST http://localhost:8080/api/v1/agent/chat \
  -H "Authorization: Bearer <real-token>" \
  -H "Content-Type: application/json" \
  -d '{
    "message": "fetch channel <inaccessible-channel-id>",
    "profile": "summary"
  }'

# Expected: response contains "channel not accessible"
```

## Why CGO is Required

`gorm.io/driver/sqlite` wraps `mattn/go-sqlite3`, which is a CGO binding to SQLite's C library. This is the same dependency used by existing repo tests like:

- `internal/pipeline/resolve_channel_test.go` (`//go:build cgo`)
- `internal/pipeline/fetch_archive_test.go` (`//go:build cgo`)

We follow the same pattern for consistency.

## Troubleshooting

### "Binary was compiled with 'CGO_ENABLED=0'"
**Solution**: Set `CGO_ENABLED=1` before running tests:
```bash
export CGO_ENABLED=1
go test ./internal/agent/ -v
```

### "C compiler \"gcc\" not found"
**Solution**: Install gcc:
- Ubuntu/Debian: `sudo apt-get install build-essential`
- Alpine: `apk add gcc musl-dev`
- macOS: `xcode-select --install`

### Tests show "no test files" for agent package
**Solution**: This is normal when `CGO_ENABLED=0`. The CGO tests are correctly excluded by build tags. To include them:
```bash
CGO_ENABLED=1 go test -tags cgo ./internal/agent/ -v
```

### "could not import gorm.io/driver/sqlite"
**Solution**: Ensure dependencies are downloaded:
```bash
go mod download
go mod tidy
```

## Verification Checklist

Before submitting:
- [ ] `go build ./...` passes
- [ ] `go test ./...` passes (CGO tests auto-excluded)
- [ ] `gofmt -l internal/agent/` returns no output
- [ ] `CGO_ENABLED=1 go test -tags cgo ./internal/agent/ -v` passes (if gcc available)
- [ ] Or: Manual E2E test confirms access denial works

## Container Testing Example

If you want to run CGO tests in a clean container:

```bash
# Create test container
docker run -it --rm -v ~/octo-smart-summary:/app -w /app golang:1.22-alpine sh

# Inside container:
apk add gcc musl-dev
go mod download
CGO_ENABLED=1 go test -tags cgo ./internal/agent/ -v
```

This ensures tests pass in a reproducible environment with build tools available.
