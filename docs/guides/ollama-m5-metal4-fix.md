# Running Ollama on Apple M5: Metal 4 Tensor API Fix

If you're running Ollama on an Apple M5 chip (M5, M5 Pro, M5 Max, M5 Ultra) and models fail to load, this guide explains the issue and how to fix it.

## Symptoms

Ollama fails to load any model with:

```
Error: 500 Internal Server Error: model failed to load
```

Server logs show:

```
ollama: (Metal) Compiler failed to build request
```

With `OLLAMA_DEBUG=1`, the full error reveals:

```
static_assert failed due to requirement '__tensor_ops_detail::__is_same_v<bfloat, half>'
"Input types must match cooperative tensor types"
```

## Root Cause

The M5 chip introduces **Metal 4** with a new **Tensor API** for GPU compute. Ollama's ggml Metal backend (vendored from llama.cpp) correctly detects Metal 4 and enables the Tensor API path for matrix multiplication. However, two mixed-type kernel instantiations — `kernel_mul_mm_bf16_f16` and `kernel_mul_mm_id_bf16_f16` — pass `bfloat` for one operand and `half` for the other into the `matmul2d::run()` Tensor API call. The MetalPerformancePrimitives framework on macOS Tahoe enforces that both input tensor types must match, causing a compile-time `static_assert` failure.

### Why it worked on M1–M4

On earlier chips, the `GGML_METAL_HAS_TENSOR` flag is not set (Metal 4 not supported), so the code falls back to the `simdgroup_multiply_accumulate` path which tolerates mixed `bfloat`/`half` types.

### Why it fails on M5

On M5, the device reports `MTLGPUFamilyMetal4` support, the Tensor API is enabled, and the mixed-type kernel instantiations are compiled with Tensor API code that rejects the type mismatch.

## Check Your Ollama Version First

This fix was merged in **llama.cpp PR [#18456](https://github.com/ggerganov/llama.cpp/pull/18456)** (December 31, 2025). If you're reading this after Ollama has synced the fix, upgrading may be all you need:

```bash
brew upgrade ollama
# or download the latest from https://ollama.com/download
```

If the latest Ollama release still doesn't work on your M5, continue with the build-from-source instructions below.

## Building Ollama From Source

```bash
git clone https://github.com/ollama/ollama.git
cd ollama
```

### Apply the fix

Remove the mixed `bfloat` x `half` kernel template instantiations from both Metal shader files:

**Files to edit:**
1. `ml/backend/ggml/ggml/src/ggml-metal/ggml-metal.metal`
2. `ml/backend/ggml/ggml/src/ggml-metal/ggml-metal-embed.metal`

In each file, find and remove both of these blocks (2 occurrences per file, 4 total):

```diff
 template [[host_name("kernel_mul_mm_f16_f16")]]     kernel mul_mm_t kernel_mul_mm<...>;
-#if defined(GGML_METAL_HAS_BF16)
-template [[host_name("kernel_mul_mm_bf16_f16")]]    kernel mul_mm_t kernel_mul_mm<bfloat, bfloat4x4, simdgroup_bfloat8x8, half, half2x4, simdgroup_half8x8, bfloat4x4, 1, dequantize_bf16, bfloat, bfloat4x4, half, half2x4>;
-#endif
 template [[host_name("kernel_mul_mm_q4_0_f16")]]    kernel mul_mm_t kernel_mul_mm<...>;
```

```diff
 template [[host_name("kernel_mul_mm_id_f16_f16")]]     kernel mul_mm_id kernel_mul_mm_id<...>;
-#if defined(GGML_METAL_HAS_BF16)
-template [[host_name("kernel_mul_mm_id_bf16_f16")]]    kernel mul_mm_id kernel_mul_mm_id<bfloat, bfloat4x4, simdgroup_bfloat8x8, half, half2x4, simdgroup_half8x8, bfloat4x4, 1, dequantize_bf16, bfloat, bfloat4x4, half, half2x4>;
-#endif
 template [[host_name("kernel_mul_mm_id_q4_0_f16")]]    kernel mul_mm_id kernel_mul_mm_id<...>;
```

### Build and run

```bash
go clean -cache
go build -o ollama-dev .
./ollama-dev serve
```

### Set up as a persistent service

Instead of running `ollama-dev serve` manually each time, create a launchd service:

```bash
cat > ~/Library/LaunchAgents/com.local.ollama-dev.plist << 'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.local.ollama-dev</string>
    <key>ProgramArguments</key>
    <array>
        <string>/path/to/your/ollama/ollama-dev</string>
        <string>serve</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/tmp/ollama-dev.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/ollama-dev.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>OLLAMA_HOST</key>
        <string>127.0.0.1:11434</string>
    </dict>
</dict>
</plist>
EOF

# Update the path to your actual binary location
# Then load the service:
launchctl load ~/Library/LaunchAgents/com.local.ollama-dev.plist
```

To stop and switch back to the official Ollama when the fix is released:

```bash
launchctl unload ~/Library/LaunchAgents/com.local.ollama-dev.plist
brew services start ollama
```

## Verification

After building and starting the patched Ollama, verify it works:

```bash
ollama run gemma3:27b "Say hello in one sentence"
```

Server logs should confirm Metal 4 Tensor API is active:

```
ggml_metal_device_init: testing tensor API for f16 support    → PASS
ggml_metal_device_init: testing tensor API for bfloat support → PASS
ggml_metal_device_init: GPU name:   Apple M5 Max
ggml_metal_device_init: GPU family: MTLGPUFamilyApple10 (1010)
```

## Using With Cloister

If you're running Ollama inside [cloister](https://github.com/ekovshilovsky/cloister) VMs, the patched Ollama on the host is automatically tunneled into VMs that have the `ollama` stack:

```bash
cloister create dev --stack ollama --start-dir ~/Projects/my-app
cloister dev

# Inside the VM — uses the host's Metal-accelerated Ollama via tunnel
ollama run gemma3:27b "hello"
```

See the [Ollama Integration](../../README.md#ollama-integration) section in the README for details on how the host tunnel architecture works and recommended models.

## Related Issues and PRs

| Reference | Description |
|-----------|-------------|
| [llama.cpp #16634](https://github.com/ggerganov/llama.cpp/pull/16634) | Initial Metal 4 Tensor API support (ggerganov) |
| [llama.cpp #17986](https://github.com/ggml-org/llama.cpp/issues/17986) | BF16 x F16 compilation failure report |
| [llama.cpp #18456](https://github.com/ggerganov/llama.cpp/pull/18456) | Fix: remove BF16 x F16 kernels (merged Dec 31, 2025) |

## Impact

This fix is required for **all Apple M5 family chips** (M5, M5 Pro, M5 Max, M5 Ultra) and potentially future Apple Silicon with Metal 4 support (A19, A20). Without this fix, no GGUF models can be loaded through Ollama's ggml backend on these devices.
