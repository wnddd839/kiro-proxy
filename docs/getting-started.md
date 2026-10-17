# 下载与安装

本页是 `README.md` 的精简副本，方便在 Pages 或文档站单独引用。完整说明见
[部署文档](deployment.md)。

## Docker（推荐）

```sh
git clone https://github.com/wnddd839/kiro.cache.git
cd kiro.cache

cat > .env <<EOF
KIRO_API_KEY=$(openssl rand -hex 24)
KIRO_ADMIN_TOKEN=$(openssl rand -hex 24)
EOF

docker compose up -d --build
```

打开 `http://127.0.0.1:8787/ui`。容器首次打开会要求输入 `KIRO_ADMIN_TOKEN`，之后保存在当前浏览器。在「接入」页复制 Base URL 和 API Key。

镜像：`ghcr.io/wnddd839/kiro-cache:latest`，支持 `linux/amd64` 与
`linux/arm64`。compose 默认从当前 checkout 构建；要改用发布镜像，把
`build:` 替换为 `image: ghcr.io/wnddd839/kiro-cache:latest`。

## 预编译二进制

| 平台 | 文件 |
|---|---|
| Windows x64 | `kiro-proxy-windows-amd64.exe` |
| Windows arm64 | `kiro-proxy-windows-arm64.exe` |
| Linux x64 | `kiro-proxy-linux-amd64` |
| Linux arm64 | `kiro-proxy-linux-arm64` |
| macOS Intel | `kiro-proxy-darwin-amd64` |
| macOS Apple Silicon | `kiro-proxy-darwin-arm64` |

从 [Releases](https://github.com/wnddd839/kiro.cache/releases/latest) 下载，
校验：

```sh
sha256sum -c SHA256SUMS.txt
```

## 从源码构建

需要 Go 1.26.4 或更高版本。

```sh
go build -trimpath -ldflags "-s -w -X main.version=dev" -o bin/kiro-proxy ./cmd/kiro-proxy
```

或：

```sh
make build
```

## 首次运行

```sh
bin/kiro-proxy login       # 浏览器登录并加入号池；服务器上按提示粘贴回调地址
bin/kiro-proxy list        # 查看号池
bin/kiro-proxy             # 启动，默认 127.0.0.1:8787
```

管理台：`http://127.0.0.1:8787/ui`。

客户端：

```sh
# Claude Code
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787

# OpenAI SDK / Cursor / Codex
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
```

配好 key 后，客户端带 `x-api-key` 或 `Authorization: Bearer`。

## 服务器登录

OAuth 回调固定打向浏览器所在机器的 `localhost`，到不了服务器进程。两种入口都支持
「粘贴回调地址」：

- CLI：`kiro-proxy login` 完成后按提示粘贴。
- 管理台：号池页「开始登录」后出现输入框。

Builder ID / IAM Identity Center 会先返回 AWS 登录链接，完成后再次粘贴回调。

## 下一步

- [配置](configuration.md)
- [Docker 与反向代理](deployment.md)
- [API](api.md)
- [缓存路由](cache.md)
- [运维与排障](operations.md)
