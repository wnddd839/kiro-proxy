// Package config 是 kiro-proxy 的配置文件。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wnddd839/kiro.cache/internal/kiro"
	"github.com/wnddd839/kiro.cache/internal/meter"
)

// ConversationMode 决定上游 conversationId 怎么取。
const (
	ConversationSession = "session" // 同一会话+同一号复用（默认）
	ConversationRandom  = "random"  // 每次随机，与 Magpie / 插件一致，用于 A/B 对照
)

// cache_ttl 的取值。
const (
	CacheTTLClient = "client" // 按客户端声明：1h 写入按 2 倍输入价
	CacheTTL5m     = "5m"     // 客户端声明的 1h 一律按 5m 计
)

// Duration 接受 "45m" 这样的字符串。
type Duration time.Duration

// UnmarshalJSON 解析 Go duration 字符串。
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON 输出 Go duration 字符串。
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Config 是全部配置。
type Config struct {
	Listen       string `json:"listen"`
	AccountsFile string `json:"accounts_file"`
	// APIKeys 是下游客户端可用的 key；空表示不校验（仅监听本机时可接受）。
	APIKeys []string `json:"api_keys,omitzero"`
	// AdminToken 保护 /admin；空表示只允许本机（回环地址 + localhost Host）访问。
	AdminToken string `json:"admin_token,omitzero"`
	// KeysFile 是下游 key 库（预算 / 周期 / 模型白名单）。库为空且 api_keys 为空时不校验。
	KeysFile string `json:"keys_file"`
	// UsageFile 是用量账本（JSONL）；空字符串只在内存里记。
	UsageFile string `json:"usage_file"`
	// UsageRetention 是账本保留期。
	UsageRetention Duration `json:"usage_retention"`

	// CacheMode 是本地 prompt cache 计量方式：auto（默认：所有协议自动前缀缓存，5 分钟滑动 TTL）|
	// protocol（Anthropic Messages 走 explicit、OpenAI 走 auto）| explicit（只认 cache_control，支持客户端 TTL）| off。
	// 未收到上游输入侧 tokenUsage 时，缓存读写用量由本地模拟；promptCaching.supported=false 的模型强制 off。
	CacheMode string `json:"cache_mode"`
	// CachePoints 是实验开关（默认空 = 关）：在这些位置给 Kiro 发 cachePoint{type:"default"}。
	// 取值 first-user | assistant | tools。有没有用先跑 `kiro-proxy probe cachepoint` 对比。
	CachePoints []string `json:"cache_points,omitzero"`
	// OpenAIHostedTools 是 OpenAI 协议里转不了的内置工具（web_search 等）的处理：
	// drop（默认，跳过这些工具与它们的历史调用，打 debug 日志；Codex 默认带 web_search）| reject（返回 400 说明原因）。
	OpenAIHostedTools string `json:"openai_hosted_tools,omitzero"`
	// Identity 是对上游报的客户端身份（CLI 版本、social refresh 的 Desktop UA）；空字段用默认值。
	Identity kiro.Identity `json:"identity,omitzero"`
	// ReportedUsage 是上游报了 tokenUsage 时的口径（上游目前不报，这是预防）：
	// conservative（默认：各字段取最后一次上报的值，缺失不覆盖，保留 Kiro 自带输入）| raw（同 conservative，兼容旧配置）|
	// sum（多条累加）| ignore（只用本地拆分）。等 probe usage 有结论再改。
	ReportedUsage string `json:"reported_usage,omitzero"`
	// CacheTTL 是 explicit / protocol 模式下对客户端 1h 声明的处理；auto 固定按 5m 计。
	// client（默认，按声明计 1h，写入 2 倍价）| 5m（一律按 5m 计）。
	CacheTTL string `json:"cache_ttl"`
	// CacheFile 保存本地 prompt cache 状态（前缀指纹 + 过期时间，不含内容），重启后仍能判定命中；空 = 只在内存。
	CacheFile string `json:"cache_file"`
	// CostBasis 是对下游报的费用口径：api（默认，API 标价等价）| credits（credits × credit_usd）。
	CostBasis string `json:"cost_basis"`
	// CreditUSD 是一个 Kiro credit 的美元价，用于号池成本与 credits 口径。
	CreditUSD float64 `json:"credit_usd"`
	// CreditRates 是按 token 估 credits 的系数（模型前缀 → 每百万 token 的 credits），
	// 只用于上游没来得及报 credits 的请求（中断 / 断流）。空 = 内置暂定系数。
	CreditRates map[string]meter.CreditRate `json:"credit_rates,omitzero"`
	// Prices 覆盖内置价格表：模型 id 前缀 → 每百万 token 美元。
	Prices              map[string]meter.Price `json:"prices,omitzero"`
	HiddenTokensBilling string                 `json:"hidden_tokens_billing"`
	HiddenTokens        map[string]int         `json:"hidden_tokens,omitzero"`
	UnknownModelPricing string                 `json:"unknown_model_pricing"`
	// PriceSync 是在线价格的拉取间隔（仅 OpenRouter）；0 = 只用内置表。
	PriceSync Duration `json:"price_sync"`
	// PricesFile 缓存上次拉到的在线价格；空 = 只在内存。
	PricesFile string `json:"prices_file"`

	SessionTTL       Duration `json:"session_ttl"` // 无结束信号时按空闲过期，默认 24h
	ConversationMode string   `json:"conversation_mode"`
	SortTools        bool     `json:"sort_tools"`
	// PinThinking 仅固定无原生参数 schema 的旧模型预算，避免预算标签改写首条消息。
	PinThinking bool `json:"pin_thinking"`
	// SystemStrip 是从 system 里删掉的正则（如 cwd / 日期行）。只在确认它们打破 cache 时加。
	SystemStrip []string `json:"system_strip,omitzero"`
	// ModelAliases 把下游模型名映射到 Kiro 模型 id，优先于内置推断。
	ModelAliases map[string]string `json:"model_aliases,omitzero"`

	MaxAttempts              int      `json:"max_attempts"`
	UpstreamConnectTimeout   Duration `json:"upstream_connect_timeout"`
	UpstreamFirstByteTimeout Duration `json:"upstream_first_byte_timeout"`
	UpstreamIdleTimeout      Duration `json:"upstream_idle_timeout"`
	// MaxConcurrent 是没单独设 max_concurrent 的号的并发上限；0 = 不限。
	MaxConcurrent  int      `json:"max_concurrent"`
	LimitsInterval Duration `json:"limits_interval"`
	// BreakerWindow 是全局熔断的滑动窗口：窗口内超过一半的号（至少 2 个）出现同一类失败
	// （刷新失败 / 403 / 网络）时只冷却、不停号。
	BreakerWindow Duration `json:"breaker_window"`
	LogLevel      string   `json:"log_level"`
	// ServerReadTimeout 是下游 HTTP 读超时（请求行 / 头 / body）。0 = 用内置默认。
	ServerReadTimeout Duration `json:"server_read_timeout,omitzero"`
	// DebugRequestsDir 非空时保存已脱敏的上游请求与前缀诊断（对话正文保留供诊断，
	// Authorization / profileArn / 图片字节 / 显式密钥替换为 [REDACTED]）；默认关闭。
	DebugRequestsDir string `json:"debug_requests_dir,omitzero"`
	// DebugRequestsMaxBytes 是调试目录的字节上限；超出时按最旧优先清理。0 = 用内置默认。
	DebugRequestsMaxBytes int64 `json:"debug_requests_max_bytes,omitzero"`
	// DebugRequestsRetention 是调试快照的保留期；过期文件由后台周期清理。0 = 用内置默认。
	DebugRequestsRetention Duration `json:"debug_requests_retention,omitzero"`
	// AllowInsecureListen 显式允许在没有下游 key 时监听非回环地址；启动时会打醒目警告。
	AllowInsecureListen bool `json:"allow_insecure_listen,omitzero"`

	// EnvAdminToken / EnvAPIKey 只从环境变量读取，不写进配置文件。
	// 容器里用它们注入密钥，避免把 secret 固化到镜像或配置卷。
	EnvAdminToken string `json:"-"`
	EnvAPIKey     string `json:"-"`
}

// Default 是默认配置。
func Default() Config {
	return Config{
		Listen:                   "127.0.0.1:8787",
		AccountsFile:             "accounts.json",
		KeysFile:                 "keys.json",
		UsageFile:                "usage.jsonl",
		UsageRetention:           Duration(90 * 24 * time.Hour),
		CacheMode:                meter.ModeAuto,
		OpenAIHostedTools:        "drop",
		CacheTTL:                 CacheTTLClient,
		CacheFile:                "promptcache.json",
		CostBasis:                meter.BasisAPI,
		HiddenTokensBilling:      "input",
		UnknownModelPricing:      "highest",
		CreditUSD:                0.02,
		PriceSync:                Duration(24 * time.Hour),
		PricesFile:               "prices.json",
		SessionTTL:               Duration(24 * time.Hour),
		ConversationMode:         ConversationSession,
		SortTools:                true,
		PinThinking:              true,
		MaxAttempts:              3,
		UpstreamConnectTimeout:   Duration(15 * time.Second),
		UpstreamFirstByteTimeout: Duration(3 * time.Minute),
		UpstreamIdleTimeout:      Duration(120 * time.Second),
		MaxConcurrent:            3,
		LimitsInterval:           0, // 不定时轮询：只在号报额度用尽时查它一个
		BreakerWindow:            Duration(2 * time.Minute),
		LogLevel:                 "info",
	}
}

// ReportedUsageMode 是生效的 tokenUsage 口径。
func (c Config) ReportedUsageMode() string {
	if c.ReportedUsage == "" {
		return kiro.ReportedConservative
	}
	return c.ReportedUsage
}

// KiroCachePoints 解析 cache_points。
func (c Config) KiroCachePoints() (kiro.CachePoints, error) {
	var p kiro.CachePoints
	for _, s := range c.CachePoints {
		switch s {
		case "first-user":
			p.FirstUser = true
		case "assistant":
			p.Assistant = true
		case "tools":
			p.Tools = true
		default:
			return p, fmt.Errorf("cache_points: unknown position %q (first-user | assistant | tools)", s)
		}
	}
	return p, nil
}

// Load 读配置；文件不存在时返回默认值。文件里没写的字段保持默认。
// KIRO_ADMIN_TOKEN / KIRO_API_KEY 从环境变量补充，适合容器部署。
func Load(path string) (Config, error) {
	c := Default()
	c.EnvAdminToken = strings.TrimSpace(os.Getenv("KIRO_ADMIN_TOKEN"))
	c.EnvAPIKey = strings.TrimSpace(os.Getenv("KIRO_API_KEY"))
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return c, nil
	case err != nil:
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate 检查取值。
func (c Config) Validate() error {
	if c.UpstreamConnectTimeout < 0 || c.UpstreamFirstByteTimeout < 0 || c.UpstreamIdleTimeout < 0 {
		return errors.New("upstream timeouts must not be negative")
	}
	if c.DebugRequestsMaxBytes < 0 {
		return errors.New("debug_requests_max_bytes must not be negative")
	}
	if c.DebugRequestsRetention < 0 {
		return errors.New("debug_requests_retention must not be negative")
	}
	if c.ServerReadTimeout < 0 {
		return errors.New("server_read_timeout must not be negative")
	}
	// 空白 key 会让 ConstantTimeCompare 把"没带 key"的请求判成通过（空比空相等），
	// 等于把号池暴露出去；必须拒绝。
	for _, k := range c.APIKeys {
		if strings.TrimSpace(k) == "" {
			return errors.New("api_keys must not contain blank keys")
		}
	}
	if c.HiddenTokensBilling != "" && c.HiddenTokensBilling != "input" && c.HiddenTokensBilling != "off" {
		return errors.New("hidden_tokens_billing must be input or off")
	}
	if c.UnknownModelPricing != "" && c.UnknownModelPricing != "highest" && c.UnknownModelPricing != "reject" {
		return errors.New("unknown_model_pricing must be highest or reject")
	}
	for _, n := range c.HiddenTokens {
		if n < 0 {
			return errors.New("hidden_tokens must not be negative")
		}
	}
	switch c.ConversationMode {
	case ConversationSession, ConversationRandom:
	default:
		return fmt.Errorf("conversation_mode must be %q or %q, got %q", ConversationSession, ConversationRandom, c.ConversationMode)
	}
	switch c.CacheMode {
	case meter.ModeProtocol, meter.ModeAuto, meter.ModeExplicit, meter.ModeOff:
	default:
		return fmt.Errorf("cache_mode must be protocol, auto, explicit or off, got %q", c.CacheMode)
	}
	if _, err := c.KiroCachePoints(); err != nil {
		return err
	}
	switch c.OpenAIHostedTools {
	case "", "reject", "drop":
	default:
		return fmt.Errorf("openai_hosted_tools must be reject or drop, got %q", c.OpenAIHostedTools)
	}
	switch c.ReportedUsage {
	case "", kiro.ReportedConservative, kiro.ReportedRaw, kiro.ReportedSum, kiro.ReportedIgnore:
	default:
		return fmt.Errorf("reported_usage must be conservative, raw, sum or ignore, got %q", c.ReportedUsage)
	}
	switch c.CacheTTL {
	case "", CacheTTLClient, CacheTTL5m:
	default:
		return fmt.Errorf("cache_ttl must be client or 5m, got %q", c.CacheTTL)
	}
	switch c.CostBasis {
	case meter.BasisAPI, meter.BasisCredits:
	default:
		return fmt.Errorf("cost_basis must be api or credits, got %q", c.CostBasis)
	}
	if c.CreditUSD < 0 {
		return errors.New("credit_usd must not be negative")
	}
	if c.BreakerWindow <= 0 {
		return errors.New("breaker_window must be positive")
	}
	if c.MaxConcurrent < 0 {
		return errors.New("max_concurrent must not be negative (0 = unlimited)")
	}
	if c.MaxAttempts < 1 {
		return errors.New("max_attempts must be >= 1")
	}
	if c.Listen == "" || c.AccountsFile == "" {
		return errors.New("listen and accounts_file are required")
	}
	return nil
}

// HiddenTokensOf 是模型的隐藏上下文 token：配置覆盖 → 实测表（kiro.HiddenTokens）→ Claude 的 4052。
func (c Config) HiddenTokensOf(model string) int {
	m := meter.CanonModel(model)
	if n, ok := c.HiddenTokens[m]; ok {
		return n
	}
	return kiro.HiddenTokens(m)
}

// ReachesBeyondLoopback 报告 listen 是否可能接受非本机连接：空 host、wildcard（0.0.0.0 / ::）、
// 任意非回环 IP 或域名都算。localhost / *.localhost / 回环 IP 不算。
func ReachesBeyondLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen // 没带端口（如 "127.0.0.1" / "0.0.0.0"）时整串当 host
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return true // ":8787" / "" 落在所有网卡
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true // 域名可能解析到本机之外
}

// UnsafeListen 报告 listen 在没有任何下游 key 时是否会暴露给非本机：此时启动会把号池
// 开放给整网。调用方应拒绝启动，除非操作者显式覆盖。
func (c Config) UnsafeListen(hasKeys bool) bool {
	return !hasKeys && ReachesBeyondLoopback(c.Listen)
}

// 调试请求快照的内置默认：最多 64 MiB、保留 24h。都可被配置覆盖。
const (
	DefaultDebugRequestsMaxBytes  = int64(64 << 20)
	DefaultDebugRequestsRetention = 24 * time.Hour
)

// DebugRequestsEnabled 报告是否开启上游请求快照。
func (c Config) DebugRequestsEnabled() bool { return c.DebugRequestsDir != "" }

// DebugRequestsCap 是生效的字节上限。
func (c Config) DebugRequestsCap() int64 {
	if c.DebugRequestsMaxBytes > 0 {
		return c.DebugRequestsMaxBytes
	}
	return DefaultDebugRequestsMaxBytes
}

// DebugRequestsTTL 是生效的保留期。
func (c Config) DebugRequestsTTL() time.Duration {
	if c.DebugRequestsRetention > 0 {
		return time.Duration(c.DebugRequestsRetention)
	}
	return DefaultDebugRequestsRetention
}
