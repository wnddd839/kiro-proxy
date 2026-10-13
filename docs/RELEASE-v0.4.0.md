# kiro-proxy v0.4.0

本次更新来自一轮代码审查，重点修复计费口径、预算预留、流式用量、账本故障处理和启动安全。升级前请核对计费与鉴权规则。

## 计费与用量口径

- **隐藏上下文单独计量**：Kiro 自带 system / 对话模板从客户端输入中拆出，作为独立 `hidden_tokens` / `hidden_tokens_charged_usd` 上报与记账。缺省按内置实测基线（Claude 4052，deepseek / minimax / glm / qwen 等按实测值），未实测模型回退 Claude 基线并标记未校准；可用 `hidden_tokens` 按精确模型 id 覆盖。
- **隐藏项跟随前缀缓存计价**：隐藏上下文排在整段前缀最前面，本轮有缓存命中时按缓存读价、只有写入时按写入价（客户端声明 1h 按 1h）、都没有时按输入价。账本行以 `hidden_cache_read` / `hidden_cache_write` 记录归类，热会话不再每轮按普通输入重复计费。`hidden_tokens_billing: "off"` 关闭其 API 价计费，展示仍单独记录。
- **输入字段语义收敛**：Anthropic `input_tokens` 仅指客户端未缓存输入，客户端总输入 = `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`，不含隐藏 token；OpenAI `prompt_tokens` / Responses `input_tokens` 为含客户端缓存的总输入，同样不含隐藏 token。`count_tokens` 也分离隐藏字段，管理台缓存命中率分母不含隐藏 token。
- **校准只缩放客户端输入**：按上下文百分比校准只作用于客户端输入三项，各项分别向下取整，不缩放输出与 reasoning，不把舍入余数记为普通输入。
- **未知模型定价**：在线价格缺缓存字段时回退内置对应档位价并在日志 / 模型表告警；未知模型默认 `unknown_model_pricing: "highest"`，按已知最高各项单价（含长上下文档）计价，`"reject"` 则生成前拒绝。
- **credits 口径统一**：响应、账本与账号统计使用同一结算值；上游未报 credits 时返回本地估算并标记 `credits_estimated`。
- **流式用量**：`message_start.usage` 在最终输入未知时报零，`message_delta.usage` 为实际结算值（不能两者相加）；同级保留 `settled_usage` 兼容旧接入方。上游报告的输入总量按「缓存未命中 → 写入 → 读」切出至多一份隐藏基线，各项不为负、总量不变（兼容性假设，未实测）。

## 预算与账本

- **预留按本请求口径计算**：准入原子检查已用 + 在途预留 + 本请求预留；预留使用本请求的计费口径、输入估计、完整 max_tokens（未给按 4096 估）、适用缓存 TTL 与上下文档位，不再叠加输入翻倍或历史平均费用。上游实际用量仍可能超过本地估计。
- **累计预算不受明细保留期影响**：`total` 周期的累计请求数、credits 和费用通过账本中的紧凑检查点跨清理与重启保留；已被旧版本丢弃的历史无法恢复。
- **账本故障可控**：写入失败或待写积压达到 32 MiB 时，带预算的 key 暂停新生成请求并返回 503，`/health` 返回 503；匿名与 `api_keys` 不受预算约束照常服务。已接收请求的记录保留在内存，后台重试成功后写盘并恢复接单。

## 生成控制与稳定性

- **本地输出上限**：Kiro 不接收 `max_tokens` / `max_completion_tokens` / `max_output_tokens`，改为按本地计量限制下发输出（含 thinking 与工具包装），到限即取消上游并返回 `max_tokens`（OpenAI 为 `length` / incomplete）；未给上限时不再本地截断。丢弃的输出不计费，但拦不住上游已做的工作。
- **结束串跨事件匹配**：`stop_sequences` / Chat `stop` 跨事件匹配，删去匹配串及其后内容并返回匹配到的 `stop_sequence`。
- **上游超时可控**：新增 `upstream_connect_timeout`（默认 15s）、`upstream_first_byte_timeout`（默认 3m）、`upstream_idle_timeout`（默认 120s）；空闲超时以上游字节为准，不被下游 ping 续期。取消 / 关闭先于等待 reader 结束，释放租约与预留。
- **线程前缀稳定**：原生 effort / thinking 配置按线程及模型锁定首轮值，后续切换被忽略以避免缓存变冷；追加 system 保留原 key，替换前缀在账本记 `thread_key_change_reason`，持久化仅存前缀长度及摘要。
- **号池熔断与状态**：改为滑动请求结果窗口，同类失败率 > 50% 且涉及至少 2 个号才触发，成功只增加分母不清空历史；最后一个可用号不会被自动停用或全局熔断。冷却 / 统计 / 窗口 / 计数以非敏感 sidecar 原子持久化，重启后可恢复。
- **风控软封**：可疑活动 429 / 403 与流错误进入独立的 24h 冷却与告警，不再按普通 ≤5m 指数退避。

## 安全

- **拒绝不安全监听**：未启用下游鉴权的非 loopback 监听拒绝启动；仅显式 `-allow-insecure-listen` 可覆盖并醒目告警。空白配置 key 被拒绝。
- **最后一把 key 删除后仍要求鉴权**：通过 `require_auth` 保存该状态；从未配置 key 的本机部署仍允许匿名使用；无效凭据使 key 库加载失败。
- **HTTP 超时**：新增 `server_read_timeout`（默认 30s），ReadHeaderTimeout 为 min(10s, 读超时)，IdleTimeout 固定 60s。
- **调试快照默认关闭且脱敏**：`debug_requests_dir` 显式开启后保存脱敏请求（移除凭证、profile 标识与图片字节）；`debug_requests_max_bytes`（默认 64 MiB）与 `debug_requests_retention`（默认 24h）在启动、写入时及每 10 分钟清理。
- **日志脱敏**：日志统一经脱敏 handler，清理结构化 body、疑似凭证字符串、secret 属性与错误；自然语言里的密钥无法保证完全识别。

## 升级注意

1. 隐藏上下文计费口径与 v0.3.0 不同：客户端输入不再包含 Kiro 基线，隐藏项独立上报与计价。下游计费说明需同步。
2. 计费与账本不迁移、不重算历史；保留配置、账号、key、账本与配套状态文件后，替换对应平台 binary 并重启即可。
3. 非 loopback 且未配置下游鉴权的部署将拒绝启动；确认已建 key，或显式接受 `-allow-insecure-listen` 的风险。
4. `total` 累计预算依赖账本检查点；旧版本已丢弃的历史无法恢复。
5. 所有上游用量 / credits 估算都可能被实际上游超过，预留不是硬性消费上限。
