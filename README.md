<p align="center">
  <img src="docs/logo.svg" width="88" height="88" alt="kiro.cache" />
</p>

<h1 align="center">kiro.cache</h1>

<p align="center">
  <strong>让 Kiro 的日常使用更有条理：会话、缓存、用量与预算，一处查看。</strong>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-BSD--3--Clause-1C1C1C?style=flat-square" alt="License" /></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-%E2%89%A51.26-1C1C1C?style=flat-square&logo=go&logoColor=white" alt="Go" /></a>
  <a href="https://github.com/wnddd839/kiro.cache/releases/latest"><img src="https://img.shields.io/github/v/release/wnddd839/kiro.cache?style=flat-square&color=1C1C1C" alt="Release" /></a>
  <img src="https://img.shields.io/badge/Kiro-Local%20Tools-1C1C1C?style=flat-square" alt="Kiro" />
  <img src="https://img.shields.io/badge/Compatible-Anthropic%20%7C%20OpenAI-1C1C1C?style=flat-square" alt="Downstream" />
</p>

<p align="center">
  <a href="#下载">下载</a> ·
  <a href="#快速开始">快速开始</a> ·
  <a href="#管理台">管理台</a> ·
  <a href="#接口">接口</a> ·
  <a href="#配置">配置</a> ·
  <a href="#免责声明">免责声明</a>
</p>

---

> 多一点连续性，少一点重复消耗。

---

## 一句话

一个围绕 Kiro 会话、缓存与用量的本地工具。**把账号管理、客户端接入和预算记录放在一起，方便个人与小团队查看使用情况。**

关注每次使用的来龙去脉：**缓存如何复用、消耗多少 credits、预算花在哪里。**

**不做本地补全缓存。** 把同一段对话认回来——钉号、钉 `conversationId`、让请求前缀字节稳定，让 Kiro 自己的 cache 打中。详见 [`notes/cache-strategy.md`](notes/cache-strategy.md)。

定位：**个人 / 小团队的 Kiro 预算分摊与用量对账工具**。给下游发带预算的 key，各自看自己的花销。

## 它做了什么

- **三种下游协议**：Anthropic Messages、OpenAI Chat Completions、OpenAI Responses（含工具调用、reasoning、流式 / 非流式）。
- **用量计量**：每次请求拆成 input / cache read / cache write / output，默认自动模拟 5 分钟前缀缓存；可切换为按客户端断点与 TTL 计量（含 1h）。
- **两本账**：对下游收费（Claude 官方价）与上游成本（credits 折算）分开记，管理台看毛利与亏损。
- **缓存命中优化**：会话粘号 + 稳定 conversationId + 前缀修复（剥离每次都变的 `x-anthropic-billing-header`、工具排序、按模型能力使用原生思考参数）。
- **账号管理**：浏览器 OAuth 登录（Google / GitHub / Builder ID / IAM IdC）、导入 Kiro IDE 凭证、手填 token。单飞 refresh、冷却、额度轮询。
- **管理台**：概览、账号、用量账单、接入、请求、模型，纯静态页 + `/admin/*`。

## 下载

预编译单文件，无需运行时依赖：

| 平台 | 文件 |
|---|---|
| Windows x64 | `kiro-proxy-windows-amd64.exe` |
| Linux x64 | `kiro-proxy-linux-amd64` |
| macOS Apple Silicon | `kiro-proxy-darwin-arm64` |

从 [Releases](https://github.com/wnddd839/kiro.cache/releases/latest) 下载最新版（当前 **v0.3.0**），校验：

```sh
sha256sum -c SHA256SUMS.txt
```

更新说明见 [v0.3.0 发布说明](docs/RELEASE-v0.3.0.md)。

## 快速开始

项目已更名为 `kiro.cache`；当前发布包、启动命令与配置文件仍使用 `kiro-proxy` 名称，下面的命令可直接沿用。

```sh
make build                           # 或 go build -o bin/kiro-proxy.exe ./cmd/kiro-proxy

bin/kiro-proxy.exe login             # 浏览器登录（Google / GitHub / Builder ID / IdC），加入号池，可重复加多号
bin/kiro-proxy.exe import-ide        # 或：导入 Kiro IDE 当前登录（~/.aws/sso/cache/kiro-auth-token.json）
bin/kiro-proxy.exe list              # 列出号池
bin/kiro-proxy.exe                   # 启动，默认 127.0.0.1:8787
bin/kiro-proxy.exe -version
```

`login` 需要 IDE 的回调端口（3128、4649…）空闲，登录前关掉 Kiro IDE 的登录页。公司自建 IdP（external_idp）暂不支持。

**在服务器上登录：** 回调固定打向 `localhost` 端口，到不了服务器进程，所以两边都支持「粘贴回调地址」：

- **CLI**：运行 `kiro-proxy login`，终端会提示粘贴回调 URL；浏览器完成登录后，把地址栏里以 `oauth/callback` 结尾的整条地址粘回去（也可以只粘 `?` 后的部分）。
- **管理台**：号池页「开始登录」后，会出现「粘贴回调地址」输入框，贴上同一条 URL 提交即可。Builder ID / IdC 会先返回一个 AWS 登录链接，跟着打开、完成后再次粘贴回调。

> **同一个 Kiro 账号只走一条路。** `login` 加的号自己持有 refresh token，不写回任何文件；`import-ide` 加的号刷新后会写回 IDE 的 token 文件。
> 若同一个账号两种加法的记录都在池里（或 IDE 里仍登录着它），两边各自 refresh 会使对方的 refresh token 失效，表现为该号被自动停用并记 `sign-in expired`。加号前先 `kiro-proxy list` 看池里有没有同一邮箱的号。

客户端填：

```sh
# Claude Code 等（Anthropic 协议）
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787

# OpenAI SDK / Cursor / Codex 等
export OPENAI_BASE_URL=http://127.0.0.1:8787/v1
```

鉴权：管理台建了 key 或配置了 `api_keys` 后，带 `x-api-key` 或 `Authorization: Bearer`。从未配置 key 的本机部署允许匿名使用；key 库一旦启用鉴权，删除最后一把 key 后仍要求鉴权，并通过 `require_auth` 保存该状态。无效凭据使 key 库加载失败。管理台「接入」页可一键生成替换 API Key。

## 服务器反向代理与出站代理

若挂载在 `/kiro/` 下，给下游 API 单独设置 location，管理台的 Basic Auth 只放在 `/kiro/` location 内。Basic Auth 与客户端的 `Authorization: Bearer` 共用同一个头；下游 API 应交给 kiro-proxy 的 API Key 鉴权。示例（位于同一 nginx `server` 中）：

```nginx
location ^~ /kiro/v1/ {
    auth_basic off;
    proxy_pass http://127.0.0.1:8787/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    proxy_buffering off;
    proxy_cache off;
}

location /kiro/ {
    auth_basic "kiro admin";
    auth_basic_user_file /etc/nginx/kiro-admin.htpasswd;
    proxy_pass http://127.0.0.1:8787/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

`proxy_pass` 的尾斜杠会将 `/kiro/v1/messages` 转为 `/messages`，服务端支持该路径。公网 API 必须先配置下游 key，管理 API 另设 `admin_token`。客户端 Anthropic Base URL 填 `https://你的域名/kiro`，OpenAI Base URL 填 `https://你的域名/kiro/v1`。当前管理台「接入」页按域名根路径生成地址，反代带前缀时需手动补 `/kiro`。

出站 HTTP 客户端已继承 Go 的代理环境变量支持，可在 systemd drop-in 中仅为本服务设置 `HTTPS_PROXY`、`HTTP_PROXY` 和 `NO_PROXY=127.0.0.1,localhost,::1`；支持 `socks5://用户名:密码@代理地址:端口`，无需新增应用配置。含凭据的 drop-in 保留在服务器上，不入库。代理失效时请求会失败，不会自动回退直连；替换代理后执行 `systemctl daemon-reload` 并重启服务。

## 管理台

浏览器打开 `http://127.0.0.1:8787/`（或 `/ui`）。六个章节：

- **概览**：命中率、号池计数、今日 / 本月 credits、平均 TPS、配置；prompt token + 缓存命中率趋势折线。
- **号池**：登录 / 导入 / 手填凭证；单号与批量刷新、启停、删除、测试、拉额度；额度与冷却倒计时。
- **用量账单**：按日期 / key / 号 / 协议 / 模型筛选；credits、命中率、TPS；每日折线与分组表；明细分页、CSV 导出。
- **接入**：Base URL（Anthropic / OpenAI）、一键生成 API Key、推荐模型、环境变量与 curl 示例。
- **请求**：最近 500 次上游尝试，含号、thread、conversation、cache read/write、credits、TPS、错误。
- **模型**：credit 倍率、上下文、是否支持缓存、价格来源。

页面本身不带数据，全部走 `/admin/*`：设了 `admin_token` 时页面提示输入，存在浏览器 localStorage。**没设 `admin_token` 时 `/admin` 只接受本机访问**（回环连入、Host 为 `localhost` / `127.0.0.1`、且不带 `X-Forwarded-For` / `Forwarded` / `X-Real-IP` 等转发头，挡局域网、DNS rebinding 与反代）。**放在反代后面时必须设 `admin_token`**。`/admin` 还拒绝跨站写请求（`Sec-Fetch-Site` / `Origin` 校验）。导入 IDE 凭证时自定义路径只接受名为 `kiro-auth-token.json` 的普通文件（之后会被读取、刷新时写回）。

也可以直接 `POST /admin/accounts`：

```json
{"label":"b","cred":{"method":"social","access_token":"...","refresh_token":"...","region":"us-east-1"}}
{"label":"c","cred":{"method":"idc","refresh_token":"...","client_id":"...","client_secret":"...","region":"us-east-1"}}
{"label":"k","cred":{"access_token":"ksk_..."}}
{"import":"ide","path":"/path/to/kiro-auth-token.json"}
```

## 接口

- `POST /v1/chat/completions`、`POST /v1/responses`（流式 / 非流式，含工具调用、reasoning），`usage` 里带 `cached_tokens` 与 `credits`
- `POST /v1/messages`（流式 / 非流式）、`POST /v1/messages/count_tokens`（粗估）、`GET /v1/models`（拿不到模型列表时 503，不编造）
- Kiro 不接收生成限制参数：`max_tokens` / `max_completion_tokens` / `max_output_tokens`（未给则不在本地截断）按本地计量限制下发输出，含 thinking 与工具包装，到限即取消上游并返回 `max_tokens`（OpenAI：`length` / incomplete）；`stop_sequences` / Chat `stop` 跨事件匹配，删去匹配串及其后内容并返回匹配到的 `stop_sequence`。丢弃的输出不计费，但拦不住上游已做的工作；`temperature` 首次出现时日志告警“已忽略”，之后记 debug。`tool_choice` 不转发，`top_p` 仍不支持
- 路径前缀容错：`/messages`、`/v1/v1/messages`、`/anthropic/v1/messages`、尾 `/`、重复 `/` 都落到同一 handler
- `GET/POST /admin/accounts`、`DELETE /admin/accounts/{id}`、`POST /admin/accounts/{id}/{enable|disable|refresh}`
- `POST /admin/accounts/batch` `{"action":"test|limits|refresh|enable|disable|delete","ids":[]}`（ids 空 = 全部，delete 必须给 ids）
- `GET/POST/DELETE /admin/login`：浏览器登录状态 / 发起 / 取消；`POST /admin/login?callback=<url>` 在服务器上手工提交回调地址
- `GET/POST/PATCH/DELETE /admin/keys`（下游 key 库，只存 sha256）
- `GET /admin/usage?from&to&key&account&model&protocol&status&bucket&page&page_size`、`GET /admin/billing?from&to`（默认本月）、`GET /admin/prices`、`POST /admin/prices/refresh`、`GET /admin/models`、`POST /admin/models/refresh`；时间可写 RFC3339、`YYYY-MM-DD` 或 `24h` / `7d`

## 配置 `kiro-proxy.json`（可省略，缺省即下表）

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:8787` | 未启用下游鉴权的非 loopback 监听拒绝启动；仅显式 `-allow-insecure-listen` 可覆盖并醒目告警 |
| `accounts_file` | `accounts.json` | 号池文件，含 token，0600，已在 .gitignore |
| `api_keys` | 空 | 不记名、不受预算约束的全权 key（自用） |
| `keys_file` | `keys.json` | 管理台生成的下游 key。只存 secret 的 sha256；明文只在新建 / 换 secret 时显示一次 |
| `usage_file` | `usage.jsonl` | 用量账本；空字符串 = 只记内存 |
| `usage_retention` | `2160h` | 明细与查询聚合保留期（90 天）；累计预算独立保存，不随明细过期清零 |
| `cache_mode` | `auto` | 本地 cache 计量：所有协议自动模拟 5m 前缀缓存，不要求 `cache_control`；`protocol` = Messages 走 `explicit`、Chat / Responses 走 `auto`；`explicit` = 只认客户端断点；`off` = 关闭模拟 |
| `cache_ttl` | `client` | `explicit` / `protocol` 模式下，客户端声明 `ttl:"1h"` 时：`client` = 按 1h 计；`5m` = 一律按 5m 计。`auto` 固定按 5m 计 |
| `cache_points` | 空 | 实验：给 Kiro 发显式 `cachePoint{type:"default"}` 的位置（`first-user` / `assistant` / `tools`）。探测无效果，保持关闭 |
| `credit_rates` | 内置拟合值 | 按 token 估 credits 的系数，模型前缀 → `{"context","output","read_factor"}`（每百万 token 的 credits），用于上游未报 credits 的请求及预算预留 |
| `reported_usage` | `conservative` | `conservative` / `raw` 按字段保留最后上报值；`sum` 累加，`ignore` 用本地计量。输入报告中的隐藏项独立拆出，见用量口径说明 |
| `hidden_tokens_billing` | `input` | `input` 按输入侧计价并跟随前缀缓存：命中轮按缓存读价、写入轮按写入价、无缓存按输入价；`off` 不收隐藏项 API 价，展示仍单独记录 |
| `hidden_tokens` | 空 | 精确规范模型 id → 隐藏 token 数；缺省用内置实测值（deepseek / minimax / glm / qwen 等），其余按 Claude 的 4052 并标未校准 |
| `unknown_model_pricing` | `highest` | 未知模型告警并用已知最高各项单价（含长上下文档）；`reject` 在生成前拒绝 |
| `openai_hosted_tools` | `drop` | OpenAI 内置工具（web_search 等）：`drop` 跳过并打 debug 日志；`reject` 返回 400 |
| `identity` | kiro-cli 2.28.0 | 对上游声明的客户端身份（CLI 版本 / api_version / desktop UA），对齐 kiro-cli 以获得稳定兼容性 |
| `cost_basis` | `api` | 对下游收费口径：`api` = Claude API 官方价；`credits` = credits × `credit_usd` |
| `credit_usd` | `0.02` | 一个 Kiro credit 的美元价，用于算上游成本与毛利 |
| `prices` | 内置 | 覆盖价格表：模型 id 前缀 → `{"input","output","cache_write","cache_write_1h","cache_read"}`（USD/百万 token） |
| `price_sync` | `24h` | 在线价格（仅 OpenRouter，过滤为 Kiro 模型）拉取间隔；`0` = 只用内置表。挂牌缺缓存价时用内置缓存价，日志与模型表告警 |
| `prices_file` | `prices.json` | 在线价格缓存；空 = 只在内存 |
| `admin_token` | 空 | `/admin` 的 `X-Admin-Token`；空 = 只允许本机访问 `/admin` |
| `session_ttl` | `24h` | 会话 → 号、→ conversationId 的空闲保留期；活跃会话续期，容量上限 4096 |
| `conversation_mode` | `session` | `random` = 每次随机，用于 A/B 对照命中率 |
| `sort_tools` | `true` | 工具声明按名排序（MCP 加载顺序不定） |
| `pin_thinking` | `true` | 旧模型固定首次 thinking 预算；原生 effort / thinking 配置按线程及模型锁定首轮值 |
| `system_strip` | 空 | 从 system 删掉的正则；确认打破 cache 再加 |
| `model_aliases` | 空 | 下游模型名 → Kiro 模型 id |
| `max_attempts` | `3` | 一次请求最多试几个号 |
| `max_concurrent` | `3` | 每个号并发上限；号上单独设了就以号上的为准。`0` = 不限 |
| `limits_interval` | `0s` | 额度定时轮询间隔，默认关闭：只在某号报额度用尽时查它一个 |
| `breaker_window` | `2m` | 滑动请求结果窗口：同类失败率 >50% 且涉及至少 2 个号触发；成功不清空历史，只冷却不停号 |
| `upstream_connect_timeout` | `15s` | 上游 TCP 连接及 TLS 握手超时 |
| `upstream_first_byte_timeout` | `3m` | 上游响应头及首个字节等待超时；长上下文冷请求的首字可能超过一分钟，误杀会冷却号并重新 prefill |
| `upstream_idle_timeout` | `120s` | 上游流连续无字节超时；不以客户端 ping 续期 |
| `server_read_timeout` | `30s` | 下游请求读取超时；ReadHeaderTimeout 为 min(10s, 读超时)，IdleTimeout 固定 60s |
| `log_level` | `info` | |
| `debug_requests_dir` | 空 | 显式开启后保存脱敏请求与相邻前缀诊断，移除凭证、profile 标识和图片字节；默认关闭，按大小及保留期自动清理 |
| `debug_requests_max_bytes` | `67108864` | 调试快照总上限 64 MiB；过大快照跳过，只管理代理快照，不删除其它文件 |
| `debug_requests_retention` | `24h` | 启动、写入时及每 10 分钟清理过期调试快照 |

## cache 命中做了什么

升级提示：配置省略 `cache_mode` 时，新默认值为 `auto`，会改变 Anthropic 请求的缓存拆分与对应费用；已有配置显式写了 `protocol`、`explicit` 或 `off` 的，继续按原配置运行。需要保留旧版按协议计量的行为，请设置 `"cache_mode": "protocol"`。

1. **会话键**：header（`X-Session-Id` 等）→ `metadata.user_id` 里的 session → 稳定 system 前缀 + 首条 user 指纹。system 追加保留键；替换前缀轮换并在账本记 `thread_key_change_reason`，持久化仅存前缀长度及摘要。
2. **粘号**：同一会话回到同一号；账号忙时等待，超时返回重试提示，不借号。只有冷却 / 停用 / 失败等不可用时才换。会话默认空闲 24h 后过期，活跃使用续期。
3. **稳定 conversationId**：按 会话 → 号 → conversationId 钉住；历史被回退 / 编辑 / 压缩时只轮换该号的。
4. **前缀修复**：剥离 Claude Code 的 `x-anthropic-billing-header`（`cch` 每次变）、工具按名排序、schema / tool-use input 对象键规范化、可选 system 正则。历史图片全部保留；不转发的旧思考块不参与会话指纹。支持原生思考参数的模型不再向 prompt 注入预算标签；旧模型原子固定首次预算。
5. **分号统计**：`/admin/stats` 与 `/admin/accounts` 给出 `cache_read / cache_write / input`，切 `conversation_mode` 做 A/B。

## 思考参数与工具描述

首次生成请求先读取模型目录的 `additionalModelRequestFieldsSchema`，后续按目录缓存刷新。Claude 类模型声明了 `output_config.effort` 时使用原生强度与 schema 允许的 `thinking` 配置；声明 `reasoning.effort` 的模型使用该字段。客户端的 `budget_tokens` 会映射为模型接受的强度，未指定预算时采用 schema 默认值。没有 schema 的旧模型保留原有 thinking 标签路径。

原生参数放在请求根部的 `additionalModelRequestFields`，同一线程/模型锁定首轮强度，后续切换被忽略以避免缓存变冷。不过 Sonnet 4.6 的实测中，首次切换 effort 后 credits 仍回到冷请求水平；本地缓存按生效参数隔离，不假定不同强度可以共用上游缓存。见 [原生参数实测](notes/native-thinking.md) 和 [Sonnet 5.5 前缀审查](notes/prefix-stability.md)。Sonnet 5.5 的同类测试切强度后仍为暖请求成本，模型之间的行为不同。

工具描述限制为 **10240 UTF-8 字节**，截断保留完整字符；本地计量与上游请求共用截断后的描述。

## 用量与计费口径

Kiro 的流里一般**不报 token**，只报上下文占用百分比和 credits。所以：

- **input / output**：客户端输入与隐藏上下文分别计量。隐藏 token 缺省取内置实测值，未实测的模型按 Claude 的 4052；`hidden_tokens` 可按精确模型 id 覆盖，Opus 5.5 保持默认并标未校准，有号后用 `probe -window 1000000` 实测再改。校准只缩放客户端输入三项，各项分别向下取整，不缩放输出，不把舍入余数记为普通输入。隐藏上下文以独立 `hidden_tokens` / `hidden_tokens_charged_usd` 记录。它排在整段前缀最前面，故跟随客户端前缀的缓存状态计价：本轮有 cache_read 时按缓存读价，只有 cache_write 时按写入价（客户端声明 1h 时按 1h），都没有时按输入价；账本行的 `hidden_cache_read` / `hidden_cache_write` 记录归类。热会话不再每轮按普通输入重复收 4052 token。`off` 关闭其 API 价计费。上游 credits 无隐藏项拆分，credits 口径的独立隐藏美元归因为 0。
- **输入字段语义**：Anthropic `input_tokens` 仅为客户端未缓存输入；客户端总输入 = `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`，不含隐藏 token。OpenAI `prompt_tokens` / Responses `input_tokens` 为含客户端缓存的总输入，但不含隐藏 token。`count_tokens` 也分离隐藏字段。管理台缓存命中率分母不含隐藏 token。
- **cache_read / cache_creation**：默认 `cache_mode: "auto"`，三种协议都自动模拟 5 分钟滑动 TTL 的前缀缓存，忽略客户端 `cache_control`。这样 OpenAI → sub2api → Anthropic 的链路即使没有缓存断点，也能返回缓存读写估算。设为 `protocol` 时，Anthropic Messages 才按客户端 `cache_control`（含顶层自动缓存）计量：每个断点往前回看 20 块，最多 4 个断点，支持 1h TTL；OpenAI 仍走自动缓存。最小可缓存长度按官方分模型表（512 / 1024 / 2048 / 4096）。这些拆分是本地模拟，不证明上游实际命中；上游报告输入侧 `tokenUsage` 时，按 `reported_usage` 配置采用报告值。当前假定报告总量含隐藏上下文（兼容性假设，未实测）：按未命中 → 写入 → 读的顺序切出至多一份隐藏基线，各项不为负、总量不变，切出部分按来源类别计价；报告很小时隐藏值可能低于基线。
- **两本账**：对下游收费（`cost_usd`，Claude API 官方价）与上游成本（`upstream_usd` = credits × `credit_usd`），管理台按 key / 号 / 模型 / 天显示毛利率与亏损。
- **中断与重试**：客户端中断按已消耗量入账（`aborted`）；上游没报 credits 则按 token 估（`credits_estimated`）。换号重试的失败尝试单独一行（`retried`），不向下游收费。
- **下游 usage**：标准字段报 token 与 credits；上游未报 credits 时返回本地估计，账本标记 `credits_estimated`，响应与账号统计使用同一结算值。扩展 `hidden_tokens_charged_usd` 单独报隐藏项美元归因。流式 `message_start.usage` 在最终输入未知时为零，`message_delta.usage` 为实际结算值（不能相加），同级 `settled_usage` 为兼容旧接入方保留同一值。基线计费调整只作用于升级后的请求，不重算历史账本。
- key 预算按各自周期（day / week / month / total，本地时区）累计，超限返回 402 + `x-should-retry: false`。准入原子检查已用 + 在途预留 + 本请求预留；完成时实际结算并释放。预留按本请求的计费口径、输入估计、完整 max_tokens（未给按 4096 估）、适用缓存 TTL 与上下文档位计算，不再叠加输入翻倍和历史平均费用。上游用量仍可能超过本地估计。`total` 的累计请求数、credits 和费用不受明细保留期影响；已被旧版本丢弃的历史无法恢复。
- **账本故障**：写入失败或待写积压达到 32 MiB 时，带预算的 key 暂停新生成请求并返回 503；匿名与 `api_keys` 不受预算约束，照常服务。`/health` 返回 503 以暴露故障。已接收请求的记录继续保留在内存；后台重试成功后写盘并恢复接单。阈值用于接单控制，在途请求完成时缓冲可暂时超过阈值。

## 失败处理

| 上游 | 动作 |
|---|---|
| 401 / 明确的 token 失效 403 | 单飞 refresh 后同号重试 |
| 其它 403（策略 / 防火墙） | 不刷新，短冷却并换号 |
| refresh 返回 400 `invalid_grant` | 停用并保留记录，提示重新登录；唯一账号也执行 |
| refresh 的其它 4xx | 冷却并计数，连续 3 次停用；刷新成功清零该号连续计数 |
| 封号（403 `TemporarilySuspended` 等 / 423） | 不刷新、停用并保留记录；唯一账号也执行 |
| 额度用尽（含 402） | 冷却至额度重置（未知则 1h），换号 |
| 429 / throttling | 冷却：上游给了 `x-amzn-kiro-ratelimit-retry-after` 就按它，否则指数退避；换号 |
| 429 / 403 / 流异常中的 suspicious activity | 软封，独立冷却 24h 并告警，不按普通限流退避 |
| 5xx / 网络 | 短冷却，换号 |
| 输入太长 / 校验错 | 直接返回，不换号 |
| 全部不可用 | 429 + `Retry-After` |

熔断依据窗口内请求结果，同类失败率超过 50% 且至少 2 个号失败时触发；单次成功只增加分母，不清空失败历史。触发时回退该窗口内已计的刷新 strike。唯一启用号不触发全局熔断；明确封号或凭证失效仍会停用，临时网络错误继续采用冷却。

冷却、请求及费用统计、strike 和熔断窗口结果存入 `accounts_file + ".state.json"` 非凭证 sidecar，临时文件加 rename 原子写入。启动恢复未过期冷却，丢弃过期冷却；不保存 token，不保存 lastUsed 和额度轮询基线。手动停用仍按管理操作执行。

- 工具参数 JSON 损坏或被本地输出上限截断时报告错误，不改写成 `{}`；合法的空参数仍为 `{}`。
- 非流式：整条收完才回写。上游中途报错或断流时丢掉半截内容，记到号上并换号；没有可换的号就回错误状态码，绝不把半句话当 `end_turn`。
- 流式：已开始向下游写 SSE 后不再换号。中途报错 / 断流发 `error` 事件（不发 `message_stop`），同时记到号上。
- 钉的号只是并发打满：先排队等它（最多 3 秒），等不到返回 429 + `Retry-After`，不借号；客户端取消排队时停止请求。

## 前缀逐字节诊断

设置 `"debug_requests_dir": ".cache/upstream-requests"` 可保存每次脱敏上游请求与 `.meta.json` 比较结果。文件仍含脱敏后的对话与工具，属于敏感调试资料；凭证字段、profileArn 和图片字节被移除，不能保证任意自然语言秘密均被识别。默认关闭，并按配置大小上限、保留期定期清理。

比较把 `history + current` 展开为消息序列，单独核对工具、账号、会话和模型参数；完整请求 JSON 会因为 current 移到 history 而改变外层结构，不能直接作为文件前缀判断。`messages_extend` 表示消息字节延续，`first_different_byte` 指向首个变化字节，`first_different_block` 给出首个变化的消息块（序号、角色、类型、至多 80 个脱敏字符）。system、工具或旧消息真正变化时如实报告，不冻结客户端内容。详见 [前缀审查与 Sonnet 5.5 实测](notes/prefix-stability.md)。

## 探测 Kiro 的真实行为

```
kiro-proxy -config kiro-proxy.json probe usage        # 上游报不报 tokenUsage、报几次、是否累计
kiro-proxy -config kiro-proxy.json probe cachepoint   # 显式 cachePoint 开 / 不开
kiro-proxy -config kiro-proxy.json probe credits      # 前缀冷热 + 长输出，拟合 credits 折算
kiro-proxy -config kiro-proxy.json probe ttl          # 缓存存活时间（实测约 5 分钟）
kiro-proxy -config kiro-proxy.json probe analyze -out probe.jsonl
```

用一个号直打上游（会花 credits），每次调用一行 JSONL，实验前后各拉一次额度核对。`analyze` 给出 TTL 命中表、cachePoint 对比、可直接贴进配置的 `credit_rates`。

实测结论（2026-10，claude-sonnet-4.5）：缓存存活约 **5 分钟**（间隔 0 / 4min 命中，6min 起不命中），与 Anthropic 默认 5m TTL 一致；请求声明 `ttl: "1h"` 且配置 `cache_mode: "explicit"`（或 Messages 使用 `protocol`）、`cache_ttl: "client"` 时，只在本地按客户端声明计费，**上游不分 TTL**。默认 `auto` 忽略该声明，按 5m 模拟。

## 开发

```sh
make check     # gofmt + vet + test -race
make build     # bin/kiro-proxy
make release   # 交叉编译到 releases/ + SHA256SUMS.txt
```

CI 见 [`.github/workflows/ci.yml`](.github/workflows/ci.yml)。

## 目录

- `cmd/kiro-proxy` — 入口、`login` / `import-ide` / `list` / `probe` / `-version`
- `internal/server` — HTTP、换号重试、SSE、计量记账、管理 API 与管理台
- `internal/pool` — 号池、单飞 refresh、选号、冷却、额度轮询
- `internal/kiro` — 请求构造、event stream 解码、鉴权 / refresh / 管理 API、失败分类、浏览器登录
- `internal/meter` — token 估算、本地 prompt cache 模拟、价格表
- `internal/sessionpin` — 会话键、粘号、稳定 conversationId
- `internal/usage` — 用量账本（JSONL 持久化）
- `internal/openai` / `internal/anthropic` — 下游协议类型与互转
- `internal/normalize` / `internal/keys` / `internal/turn` / `internal/tokenizer` — 前缀稳定化 / key 库 / 协议无关 Sink / 分词
- `internal/probe` — 上游行为探测
- `vendor/opencode-kiro-auth` — 上游 Kiro 插件，只作协议参考

## 未做

- kiro-cli 登录导入（其 SQLite 存储需要额外依赖；用 `login` 代替）
- 公司自建 IdP 登录
- web search 替代

## 免责声明

本项目仅用于**个人学习、研究与自用**。使用前请阅读以下各条：

- **条款风险**：Kiro / AWS 的服务条款**不允许在官方客户端之外使用其订阅**。是否允许此类调用由你与 Kiro / AWS 的协议决定。使用本项目**可能导致账号受到限制或暂停**，用户自担全部风险。
- **不提供规避手段**：本项目不提供任何规避平台风控、检测或限流的功能（无逐账号代理、无出口 IP 管理、无请求特征伪装）。不要把它当“防封”工具。
- **不得转售**：不得将本项目用于代充、售卖额度、倒卖账号或任何商业转售场景。
- **按原样提供**：软件按开源「原样」免费提供，不对上游协议持续兼容作任何保证；不提供账号，不代管凭据。

本项目与 Kiro、AWS 官方**无关联、无隶属、无背书关系**。

## License

[BSD 3-Clause](LICENSE)
