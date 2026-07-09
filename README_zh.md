# lmgo-v2

![tray](tray.jpg)

[English README](README.md)

lmgo-v2 是一个 Windows 系统托盘应用，封装了 llama.cpp server 的 **router 模式**，实现无需重启的动态模型切换。程序本身与后端无关 — 搭配任意 llama-server 版本（CUDA、ROCm、Vulkan、CPU）即可使用。

## 系统要求

- **操作系统：** Windows 11
- **架构：** x86_64
- **llama-server：** 任意后端均可 — 从 [llama.cpp releases](https://github.com/ggml-org/llama.cpp/releases) 下载预编译版本，AMD 显卡可用 [llamacpp-rocm](https://github.com/lemonade-sdk/llamacpp-rocm/releases)。

## 功能特性

- **Router 模式**：单个 llama-server 进程服务所有模型，通过 API 按需切换
- **动态模型切换**：无需重启，改 API 请求中的 `model` 字段即可换模型
- **系统托盘**：精简托盘菜单，打开 Web UI、开关自启、刷新配置
- **开机自启**：可通过启动文件夹快捷方式随 Windows 启动
- **当前模型显示**：托盘菜单顶部实时显示正在运行的模型
- **标准 OpenAI API**：`GET /v1/models`、`POST /v1/chat/completions` — 兼容所有 OpenAI 客户端
- **INI 模型配置**：`models.ini` 定义模型及参数（投机解码、多模态 mmproj、KV Cache 等全部支持）

## 快速开始

1. 从 [releases](https://github.com/zyoung11/lmgo-v2/releases) 下载 `lmgo-v2.exe`
2. 放到空文件夹中运行
3. 托盘图标出现，自动生成 `config.json` 和 `models.ini`
4. 编辑 `config.json` 设置 `modelDir` 路径
5. 编辑 `models.ini` 调整各模型参数
6. 托盘菜单点 **Refresh** 使配置生效

## 配置

### config.json

```json
{
  "modelDir": "./models",
  "autoStartEnabled": false,
  "port": 19966,
  "pollInterval": 2,
  "defaultArgs": [
    "--host", "0.0.0.0",
    "--no-host",
    "-ngl", "999",
    "--flash-attn", "on",
    "--ctx-size", "131072",
    "--cache-type-k", "f16",
    "--cache-type-v", "f16",
    "--kv-offload",
    "--no-mmap",
    "--direct-io",
    "--mlock",
    "--split-mode", "layer",
    "--main-gpu", "0"
  ],
  "excludePatterns": ["mmproj*", "mtp*"]
}
```

| 字段 | 说明 |
|---|---|
| `modelDir` | `.gguf` 模型文件目录 |
| `autoStartEnabled` | 是否开机自启 |
| `port` | llama-server HTTP 端口 |
| `pollInterval` | 托盘刷新已加载模型的间隔（秒） |
| `defaultArgs` | 全局默认参数，对应 `models.ini` 的 `[*]` 段 |
| `excludePatterns` | glob 模式排除不需要的模型文件 |

### models.ini

`[*]` 段为全局默认参数，所有模型自动继承。各模型段只需写与全局不同的参数：

```ini
[*]
host = 0.0.0.0
no-host = true
ngl = all
flash-attn = on
ctx-size = 131072
batch-size = 4096
ubatch-size = 4096
threads = 0
threads-batch = 0
cache-type-k = f16
cache-type-v = f16
kv-offload = true
no-mmap = true
direct-io = true
mlock = true
split-mode = layer
main-gpu = 0

[llama-3-8b]
model = D:/LLM/Llama-3-8B-Instruct.gguf
ctx-size = 8192
temp = 0.7

[gemma-4-12b]
model = D:/LLM/gemma-4-12B-it-qat-UD-Q4_K_XL.gguf
ctx-size = 262144
model-draft = D:/LLM/mtp-gemma-4-12B.gguf
spec-type = draft-mtp
spec-draft-n-max = 4
mmproj = D:/LLM/mmproj-F16.gguf
```

段名即 API 请求中的模型标识符。所有 [llama-server CLI 参数](https://github.com/ggml-org/llama.cpp) 都可以作为 INI key（去掉前缀 `--` 即可）。

`models.ini` 已存在时，Refresh 只追加新扫描到的 `.gguf` 文件，手动编辑的内容不会被覆盖。

## API 使用

```
GET  http://localhost:19966/v1/models
POST http://localhost:19966/v1/chat/completions
```

通过 `model` 字段切换模型：

```bash
curl http://localhost:19966/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gemma-4-12b","messages":[{"role":"user","content":"你好"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:19966/v1", api_key="not-needed")
client.chat.completions.create(
    model="gemma-4-12b",
    messages=[{"role":"user","content":"你好"}],
)
```

## 托盘菜单

```
● gemma-4-12b        ← 当前已加载模型（每 pollInterval 秒刷新）
──────────────
Web Interface        ← 打开 http://localhost:{port}
✓ Auto Startup       ← 开关开机自启
Refresh              ← 重载 config.json + models.ini + 重启 server
──────────────
Exit
```

## 从源码构建

1. 从 [llama.cpp releases](https://github.com/ggml-org/llama.cpp/releases) 下载最新的 llama.cpp server 包（如 `llama-b*-bin-win-cuda-cu12.4-x64.zip`），放到项目根目录
2. 运行：

```bash
go mod tidy
go build -ldflags "-s -w -H windowsgui" .
```

`.zip` 通过 `//go:embed` 嵌入二进制，首次运行时解压到 `server/`。如需替换为 ROCm 版本，构建前换 `.zip` 文件即可。

## 相关项目

- [lmgo](https://github.com/zyoung11/lmgo) — 旧版（进程管理 + lmc 终端界面）
- [llamacpp-rocm](https://github.com/lemonade-sdk/llamacpp-rocm) — AMD ROCm 专用 llama.cpp 构建
