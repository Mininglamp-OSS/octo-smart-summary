#!/bin/bash
#
# Download tokenizer model files for exact token counting
# 
# Usage:
#   ./scripts/download_tokenizer_models.sh
#   TOKENIZER_MODELS_DIR=/custom/path ./scripts/download_tokenizer_models.sh
#
# Models downloaded (matching actual API models used):
#   - Qwen3.6-27B (qwen/tokenizer.json) - for qwen3.6-max/plus/flash
#   - DeepSeek-V3 (deepseek/tokenizer.json) - for deepseek-v4-flash/pro
#   - Kimi-K2 (kimi/tiktoken.model) - for kimi-k2
#
# Note: 
#   - Uses HuggingFace mirror (hf-mirror.com) as primary for large files
#   - Falls back to huggingface.co for small files
#   - If behind a proxy, ensure HTTP_PROXY/HTTPS_PROXY are set correctly
#

set -e

# Determine target directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

if [ -n "$TOKENIZER_MODELS_DIR" ]; then
    TARGET_DIR="$TOKENIZER_MODELS_DIR"
else
    TARGET_DIR="$REPO_ROOT/internal/tokenizer/models"
fi

echo "=== Downloading tokenizer models ==="
echo "Target directory: $TARGET_DIR"
echo ""

# Create directories
mkdir -p "$TARGET_DIR/qwen"
mkdir -p "$TARGET_DIR/deepseek"
mkdir -p "$TARGET_DIR/kimi"

# Disable SOCKS proxy (often causes issues), keep HTTP proxy
unset ALL_PROXY all_proxy

# If HTTP proxy is set, use it explicitly with curl
CURL_OPTS="--connect-timeout 30 --max-time 600"
if [ -n "$HTTP_PROXY" ] || [ -n "$http_proxy" ]; then
    PROXY="${HTTP_PROXY:-$http_proxy}"
    CURL_OPTS="$CURL_OPTS --proxy $PROXY"
    echo "Using proxy: $PROXY"
    echo ""
fi

# Function to download with retry and mirror fallback
download_file() {
    local repo="$1"
    local file="$2"
    local output="$3"
    local max_retries=3
    
    # For large files (tokenizer.json, tiktoken.model), prefer mirror
    # For small files (tokenizer_config.json), try original first
    local urls
    if [[ "$file" == "tokenizer_config.json" ]]; then
        urls=(
            "https://huggingface.co/$repo/raw/main/$file"
            "https://hf-mirror.com/$repo/raw/main/$file"
        )
    else
        urls=(
            "https://hf-mirror.com/$repo/resolve/main/$file"
            "https://huggingface.co/$repo/resolve/main/$file"
        )
    fi
    
    for url in "${urls[@]}"; do
        local retry=0
        while [ $retry -lt $max_retries ]; do
            echo "  Downloading: $(basename "$output") from $(echo "$url" | cut -d'/' -f3)"
            if curl -fsSL $CURL_OPTS -o "$output" "$url" 2>/dev/null; then
                # Verify file is not empty and has reasonable size
                if [ -s "$output" ]; then
                    local size=$(stat -f%z "$output" 2>/dev/null || stat -c%s "$output" 2>/dev/null)
                    if [ "$size" -gt 100 ]; then
                        return 0
                    fi
                fi
            fi
            retry=$((retry + 1))
            [ $retry -lt $max_retries ] && sleep 2
        done
    done
    
    echo "  ERROR: Failed to download $file"
    return 1
}

# === Qwen ===
# Using Qwen3.6-27B for qwen3.6-max/plus/flash models
echo "[1/3] Downloading Qwen3.6-27B tokenizer..."
download_file "Qwen/Qwen3.6-27B" "tokenizer.json" "$TARGET_DIR/qwen/tokenizer.json"
download_file "Qwen/Qwen3.6-27B" "tokenizer_config.json" "$TARGET_DIR/qwen/tokenizer_config.json"
echo "  Done: qwen/"

# === DeepSeek ===
# Using DeepSeek-V3 for deepseek-v4-flash/pro models
echo "[2/3] Downloading DeepSeek-V3 tokenizer..."
download_file "deepseek-ai/DeepSeek-V3" "tokenizer.json" "$TARGET_DIR/deepseek/tokenizer.json"
download_file "deepseek-ai/DeepSeek-V3" "tokenizer_config.json" "$TARGET_DIR/deepseek/tokenizer_config.json"
echo "  Done: deepseek/"

# === Kimi ===
# Using Kimi-K2-Instruct for kimi-k2 models
echo "[3/3] Downloading Kimi-K2 tokenizer..."
download_file "moonshotai/Kimi-K2-Instruct" "tiktoken.model" "$TARGET_DIR/kimi/tiktoken.model"
download_file "moonshotai/Kimi-K2-Instruct" "tokenizer_config.json" "$TARGET_DIR/kimi/tokenizer_config.json"
echo "  Done: kimi/"

echo ""
echo "=== Download complete ==="
echo ""

# Clean up any .cache directories created by hf
rm -rf "$TARGET_DIR"/*/.cache 2>/dev/null || true

# Verify downloads
echo "Verifying files..."
MISSING=0

for f in \
    "$TARGET_DIR/qwen/tokenizer.json" \
    "$TARGET_DIR/deepseek/tokenizer.json" \
    "$TARGET_DIR/kimi/tiktoken.model"; do
    if [ -f "$f" ] && [ -s "$f" ]; then
        SIZE_BYTES=$(stat -f%z "$f" 2>/dev/null || stat -c%s "$f" 2>/dev/null)
        SIZE_HUMAN=$(echo "$SIZE_BYTES" | awk '{printf "%.1fM", $1/1024/1024}')
        REL_PATH="$(basename "$(dirname "$f")")/$(basename "$f")"
        
        # Check minimum expected size (simple threshold check)
        if [ "$SIZE_BYTES" -ge 1000000 ]; then
            echo "  ✓ $REL_PATH ($SIZE_HUMAN)"
        else
            echo "  ✗ $REL_PATH too small ($SIZE_HUMAN)"
            MISSING=1
        fi
    else
        echo "  ✗ MISSING: $f"
        MISSING=1
    fi
done

if [ $MISSING -eq 1 ]; then
    echo ""
    echo "ERROR: Some files are missing or incomplete!"
    exit 1
fi

TOTAL_SIZE=$(du -sh "$TARGET_DIR" | cut -f1)
echo ""
echo "All tokenizer models downloaded successfully!"
echo "Total size: $TOTAL_SIZE"
echo ""
echo "Models:"
echo "  - Qwen3.6-27B     → qwen3.6-max, qwen3.6-plus, qwen3.6-flash"
echo "  - DeepSeek-V3     → deepseek-v4-flash, deepseek-v4-pro"
echo "  - Kimi-K2-Instruct → kimi-k2"
echo ""
echo "For production, set: TOKENIZER_MODELS_DIR=$TARGET_DIR"
