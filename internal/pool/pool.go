// Package pool 是本地 Kiro 号池：账号持久化、单号单飞 refresh、粘滞优先选号、冷却与额度轮询。
//
// 选号原则：同一会话始终回到上次的号（上游 cache 跟号走）；只有该号不可用时才换。
// 新会话挑并发最少、剩余额度最多、最久没用的号。不做 round-robin。
package pool

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/wnddd839/kiro.cache/internal/kiro"
	"github.com/wnddd839/kiro.cache/internal/sessionpin"
)

// refreshLead 是提前刷新的窗口。
const refreshLead = 2 * time.Minute

// DefaultRefreshConcurrency 是全局同时进行的 token 刷新数上限。号多、同时到期时不一口气打上游。
const DefaultRefreshConcurrency = 3

// maxRefreshFailures 是停号前允许的连续刷新失败次数（不含 invalid_grant：那个立即停）。
// 参考 hank9999/kiro.rs（MIT）的 MAX_FAILURES_PER_CREDENTIAL。
const maxRefreshFailures = 3

// refreshCooldown 是一次非永久性刷新失败后的冷却。
const refreshCooldown = time.Minute

// DefaultBreakerWindow 是全局熔断的滑动窗口。
const DefaultBreakerWindow = 2 * time.Minute

// softBanCooldown 是上游风控 / 异常活动 429（软封）的冷却：远大于普通限流，换号后等一天再看。
const softBanCooldown = 24 * time.Hour

// stateSuffix 是非秘密运行时状态（冷却 / 统计）的 sidecar 后缀，与凭证文件分开落盘。
const stateSuffix = ".state.json"

// 全局熔断看的失败种类：窗口内超过一半的号出现同一种，判定为全局故障（网络 / 上游异常），不是号的问题。
const (
	FailForbidden = "forbidden" // 403（刷新或对话；不含封号文字）
	FailNetwork   = "network"   // 没拿到 HTTP 响应
	FailRefresh   = "refresh"   // 其它刷新失败（401、5xx……；不含 invalid_grant）
)

// Source 是凭证的来源。
const (
	SourceStored = ""    // 凭证只存在号池文件里
	SourceIDE    = "ide" // 来自 Kiro IDE 的 token 文件；刷新后写回，IDE 可以继续用
)

// Account 是号池文件里的一条账号。
type Account struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitzero"`
	Disabled bool   `json:"disabled,omitzero"`
	Note     string `json:"note,omitzero"` // 被停用的原因
	// Reregister 表示 IdC client 注册已过期，要重新登录（重新注册 client）。号不停，刷新一直失败并冷却。
	Reregister    bool      `json:"reregister,omitzero"`
	Source        string    `json:"source,omitzero"`
	SourcePath    string    `json:"source_path,omitzero"`
	MaxConcurrent int       `json:"max_concurrent,omitzero"` // 0 用号池默认（Options.MaxConcurrent）
	Cred          kiro.Cred `json:"cred"`
}

// Stats 是一个号的累计计数。
type Stats struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheRead    int64   `json:"cache_read_tokens"`
	CacheWrite   int64   `json:"cache_write_tokens"`
	Credits      float64 `json:"credits"`  // Kiro 实际扣的 credits
	CostUSD      float64 `json:"cost_usd"` // 按 API 标价的等价费用
}

// Add 累加一次成功请求的 usage。token 数是本地计量，Credits 是上游报的。
func (s *Stats) Add(u kiro.Usage, costUSD float64) {
	s.Requests++
	s.Credits += u.Credits
	s.CostUSD += costUSD
	s.InputTokens += int64(u.Input)
	s.OutputTokens += int64(u.Output)
	s.CacheRead += int64(u.CacheRead)
	s.CacheWrite += int64(u.CacheWrite)
}

// HitRate 是 cacheRead / 全部输入。
func (s Stats) HitRate() float64 {
	total := s.InputTokens + s.CacheRead + s.CacheWrite
	if total == 0 {
		return 0
	}
	return float64(s.CacheRead) / float64(total)
}

// View 是一个号的脱敏快照，给管理 API 用。
type View struct {
	ID            string      `json:"id"`
	Label         string      `json:"label,omitzero"`
	Method        kiro.Method `json:"method"`
	Source        string      `json:"source,omitzero"`
	Region        string      `json:"region"`
	Disabled      bool        `json:"disabled"`
	Note          string      `json:"note,omitzero"`
	Reregister    bool        `json:"reregister,omitzero"`
	RefreshFails  int         `json:"refresh_failures,omitzero"`
	ExpiresAt     time.Time   `json:"expires_at,omitzero"`
	InFlight      int         `json:"in_flight"`
	CooldownUntil time.Time   `json:"cooldown_until,omitzero"`
	CooldownWhy   string      `json:"cooldown_reason,omitzero"`
	Limits        kiro.Limits `json:"limits"`
	LimitsAt      time.Time   `json:"limits_at,omitzero"`
	Stats         Stats       `json:"stats"`
	HitRate       float64     `json:"cache_hit_rate"`
	Recon         Recon       `json:"recon"`
}

// BreakerView 是全局熔断的状态，给管理台用。
type BreakerView struct {
	Open   bool          `json:"open"`
	Window string        `json:"window"`
	Kinds  []BreakerKind `json:"kinds,omitzero"`
}

// BreakerKind 是一种正在熔断的失败。
type BreakerKind struct {
	Kind     string    `json:"kind"`
	Accounts int       `json:"accounts"` // 窗口内出现这种失败的 distinct 号数
	Enabled  int       `json:"enabled"`  // 全池启用号数
	Failures int       `json:"failures"` // 窗口内该种类的失败结果数
	Total    int       `json:"total"`    // 窗口内全部请求结果数（比例分母）
	Ratio    float64   `json:"ratio"`    // Failures / Total
	Since    time.Time `json:"since"`
}

// breaker 是全局熔断：由 Pool.mu 保护。
//
// 判据是「滑动时间窗内的失败比例」：窗口内某个种类的失败结果数 / 窗口内全部请求结果数 > 1/2，
// 且失败覆盖 >= 2 个不同的号。按请求结果（outcome）计数，不是按账号数：
// 这样单个号的一次成功只是把分母变大，不会清空其它号留下的整段失败历史。
type breaker struct {
	window   time.Duration
	outcomes []outcome            // 窗口内的请求结果（成功 / 失败），按时间追加
	since    map[string]time.Time // 种类 → 熔断开始；没熔断的不在表里
	// counted 是种类 → 号 → 熔断触发前已计入 refreshFails 的时刻；触发时按时间窗回退。
	counted map[string]map[string][]time.Time
}

// outcome 是一次请求结果：成功（Fail=false，Kind 为空）或某种失败。字段导出以便落盘。
type outcome struct {
	Kind string    `json:"kind,omitzero"`
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
	Fail bool      `json:"fail,omitzero"`
}

type slot struct {
	acct Account

	refreshMu sync.Mutex // 同一号同时只有一次 refresh；refresh token 可能轮换

	// 以下字段由 Pool.mu 保护
	inflight     int
	cooldown     time.Time
	cooldownWhy  string
	strikes      int
	refreshFails int // 连续刷新失败次数；成功清零
	lastUsed     time.Time
	limits       kiro.Limits
	limitsAt     time.Time
	stats        Stats
	recon        Recon
	// metered / estimated 是自启动以来记到这个号上的 credits：上游 meteringEvent 报的 / 按 token 估的。对账用
	metered   float64
	estimated float64
}

// Recon 是一个号的未归属 credits 对账：Get-Usage-Limits 的已用增量减去同一时段账本记的 credits。
// 差额为正是上游扣了、账本没记的（中断前没报 meteringEvent、别的客户端共用这个号……）；
// 为负是按 token 估高了。额度重置或已用变小时从新的基线重新累计。
type Recon struct {
	Since      time.Time `json:"since,omitzero"`     // 累计起点（第一次拉到额度）
	Upstream   float64   `json:"upstream_credits"`   // 起点以来号上实际扣的（额度增量之和）
	Recorded   float64   `json:"recorded_credits"`   // 同时段账本记到该号的 = Reported + Estimated
	Reported   float64   `json:"reported_credits"`   // 其中上游 meteringEvent 报的
	Estimated  float64   `json:"estimated_credits"`  // 其中按 token 估的（中断 / 断流前上游没报）
	Unassigned float64   `json:"unassigned_credits"` // Upstream − Recorded
	Resets     int       `json:"resets,omitzero"`    // 额度重置（已用变小）次数
	baseUsed   float64
	baseMeter  float64
	baseEst    float64
}

// Pool 是号池。零值不可用，用 Open。
type Pool struct {
	path   string
	client *kiro.Client
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	slots   map[string]*slot
	order   []string // 文件顺序
	pins    *sessionpin.Table
	pending map[string]*sessionLease // 首次答复前的临时账号选择，不落盘、不提交正式钉号
	freed   *sync.Cond               // 有号释放并发名额时广播；L 是 &mu
	// pick 在同一档的 n 个候选里选一个（随机；测试可替换）
	pick func(n int) int
	// pinWait 是钉的号只是忙时排队的上限；负数不等
	pinWait time.Duration
	// maxConc 是没单独设上限的号的并发上限；0 不限
	maxConc int
	// refreshSem 限制全局同时进行的刷新数；每个号仍有自己的 refreshMu（同一号单飞）
	refreshSem chan struct{}
	brk        breaker

	saveMu sync.Mutex
}

// ErrNoAccount 表示当前没有可用账号。
var ErrNoAccount = errors.New("no usable Kiro account")

// UnavailableError 是没有可用号时的详情：RetryAt 是最早恢复时间（可能为零）。
type UnavailableError struct {
	RetryAt time.Time
	Reason  string
}

func (e *UnavailableError) Error() string {
	if e.Reason != "" {
		return ErrNoAccount.Error() + ": " + e.Reason
	}
	return ErrNoAccount.Error()
}

func (e *UnavailableError) Unwrap() error { return ErrNoAccount }

// Options 配置号池。
type Options struct {
	Client *kiro.Client
	Logger *slog.Logger
	PinTTL time.Duration // 会话→号粘滞时长
	// PinWait 是钉的号并发打满时排队等它的上限；0 用 DefaultPinWait，负数不等。
	PinWait time.Duration
	// RefreshConcurrency 是全局同时刷新数上限；0 用 DefaultRefreshConcurrency。
	RefreshConcurrency int
	// BreakerWindow 是全局熔断的滑动窗口；0 用 DefaultBreakerWindow。
	BreakerWindow time.Duration
	// MaxConcurrent 是没单独设 max_concurrent 的号的并发上限；0 不限。
	MaxConcurrent int
}

type file struct {
	Accounts []Account `json:"accounts"`
}

// accountState 是一个号落在 sidecar 里的非秘密运行时状态。冷却原因与统计都经过清理，
// 绝不含原始上游正文或凭证。
type accountState struct {
	CooldownUntil time.Time `json:"cooldown_until,omitzero"`
	CooldownWhy   string    `json:"cooldown_reason,omitzero"`
	Strikes       int       `json:"strikes,omitzero"`
	RefreshFails  int       `json:"refresh_fails,omitzero"`
	Stats         Stats     `json:"stats"`
}

// stateFile 是 sidecar 的内容：号 ID → 运行时状态。与凭证文件分开落盘。
type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
	// Breaker 是全局熔断的请求结果历史（非秘密：只有 kind / 号 ID / 时间 / 成功失败）。
	// 存下来是为了重启后失败比例窗口不丢，冷却与统计之外也能继续判熔断。
	Breaker []outcome `json:"breaker,omitzero"`
	// Counted 是熔断触发前已计入各号的 refreshFails 时刻（非秘密：只有 kind / 号 ID / 时间）。
	// 不存的话，重启后窗口内累的旧 strike 无法在新失败触发熔断时回退。
	Counted []countedStrike `json:"counted,omitzero"`
}

// countedStrike 是一条「熔断触发前已计入某号某类失败」的记录。字段导出以便落盘。
type countedStrike struct {
	Kind string    `json:"kind"`
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
}

// Open 读号池文件；文件不存在时从空池开始。
func Open(path string, opts Options) (*Pool, error) {
	p := &Pool{
		path:    path,
		client:  cmp.Or(opts.Client, kiro.NewClient(nil)),
		log:     kiro.SafeLogger(cmp.Or(opts.Logger, slog.Default())),
		now:     time.Now,
		slots:   map[string]*slot{},
		pins:    sessionpin.New(opts.PinTTL),
		pending: map[string]*sessionLease{},
	}
	p.freed = sync.NewCond(&p.mu)
	p.pick = func(n int) int { return mrand.IntN(n) }
	p.refreshSem = make(chan struct{}, cmp.Or(max(0, opts.RefreshConcurrency), DefaultRefreshConcurrency))
	p.pinWait = opts.PinWait
	if p.pinWait == 0 {
		p.pinWait = DefaultPinWait
	}
	p.maxConc = max(0, opts.MaxConcurrent)
	p.brk = breaker{
		window:  cmp.Or(opts.BreakerWindow, DefaultBreakerWindow),
		since:   map[string]time.Time{},
		counted: map[string]map[string][]time.Time{},
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		p.loadState()
		return p, nil
	case err != nil:
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, a := range f.Accounts {
		if a.ID == "" || p.slots[a.ID] != nil {
			return nil, fmt.Errorf("%s: empty or duplicate account id %q", path, a.ID)
		}
		p.slots[a.ID] = &slot{acct: a}
		p.order = append(p.order, a.ID)
	}
	p.loadState()
	return p, nil
}

// Len 是账号数。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.order)
}

// statePath 是运行时状态的 sidecar 路径（与凭证文件分开）。
func (p *Pool) statePath() string { return p.path + stateSuffix }

// loadState 读回 sidecar 里的非秘密运行时状态：冷却 / 统计 / 失败计数。
// 缺失或损坏的 sidecar 只是没有历史，不影响启动；过期的冷却直接丢弃（号已经自然恢复）。
func (p *Pool) loadState() {
	raw, err := os.ReadFile(p.statePath())
	if err != nil {
		return
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		p.log.Warn("ignoring unreadable pool state sidecar", "path", p.statePath(), "err", err)
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	// 重启后仍保留窗口内的请求结果历史，让失败比例分档跨重启连续。
	for _, o := range sf.Breaker {
		if now.Sub(o.At) <= p.brk.window {
			p.brk.outcomes = append(p.brk.outcomes, o)
		}
	}
	// 恢复熔断触发前的 strike 记账（只留窗口内的，过期丢弃），让重启后的新失败仍能回退旧 strike。
	for _, cs := range sf.Counted {
		if now.Sub(cs.At) > p.brk.window {
			continue
		}
		if s := p.slots[cs.ID]; s == nil || s.acct.Disabled {
			continue
		}
		if p.brk.counted[cs.Kind] == nil {
			p.brk.counted[cs.Kind] = map[string][]time.Time{}
		}
		p.brk.counted[cs.Kind][cs.ID] = append(p.brk.counted[cs.Kind][cs.ID], cs.At)
	}
	for id, st := range sf.Accounts {
		s := p.slots[id]
		if s == nil {
			continue // 号已删除：状态一并丢弃
		}
		if st.CooldownUntil.After(now) {
			s.cooldown, s.cooldownWhy = st.CooldownUntil, kiro.Diagnostic(st.CooldownWhy)
		}
		s.strikes, s.refreshFails, s.stats = st.Strikes, st.RefreshFails, st.Stats
	}
}

// stateSnapshot 在 p.mu 下抓一个非秘密运行时状态快照。写出的原因文本再清理一遍：
// sidecar 绝不能带原始上游正文或凭证回声。
func (p *Pool) stateSnapshot() stateFile {
	sf := stateFile{Accounts: make(map[string]accountState, len(p.order))}
	for _, id := range p.order {
		s := p.slots[id]
		if s == nil {
			continue
		}
		sf.Accounts[id] = accountState{
			CooldownUntil: s.cooldown,
			CooldownWhy:   kiro.Diagnostic(s.cooldownWhy),
			Strikes:       s.strikes,
			RefreshFails:  s.refreshFails,
			Stats:         s.stats,
		}
	}
	// 只存窗口内的结果：过期的本来就不参与比例判断。
	now := p.now()
	for _, o := range p.brk.outcomes {
		if now.Sub(o.At) <= p.brk.window {
			sf.Breaker = append(sf.Breaker, o)
		}
	}
	// 只存窗口内的 strike 记账：过期的重启时也会被丢弃。
	for kind, byID := range p.brk.counted {
		for id, times := range byID {
			for _, at := range times {
				if now.Sub(at) <= p.brk.window {
					sf.Counted = append(sf.Counted, countedStrike{Kind: kind, ID: id, At: at})
				}
			}
		}
	}
	return sf
}

// saveState 原子地把最新的非秘密运行时状态写到 sidecar（临时文件 + rename）。
// 锁序与 save 一致：先 saveMu 再 p.mu，快照在 saveMu 内取，避免并发下旧快照覆盖新状态。
func (p *Pool) saveState() {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	sf := p.stateSnapshot()
	p.mu.Unlock()
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.log.Error("marshal pool state", "err", err)
		return
	}
	if err := kiro.WriteFileAtomic(p.statePath(), raw); err != nil {
		p.log.Error("save pool state", "err", err)
	}
}

// Add 加一个号并落盘。ID 为空时生成。
func (p *Pool) Add(a Account) (Account, error) {
	if a.Cred.AccessToken == "" && a.Cred.RefreshToken == "" {
		return Account{}, errors.New("account needs an access or refresh token")
	}
	if a.Cred.Method == "" {
		a.Cred.Method = kiro.MethodSocial
	}
	if a.Cred.MachineID == "" {
		a.Cred.MachineID = kiro.NewMachineID() // 来源没带就生成一个，之后固定
	}
	p.mu.Lock()
	if a.ID == "" {
		a.ID = newID()
	}
	if p.slots[a.ID] != nil {
		p.mu.Unlock()
		return Account{}, fmt.Errorf("account %q already exists", a.ID)
	}
	p.slots[a.ID] = &slot{acct: a}
	p.order = append(p.order, a.ID)
	p.mu.Unlock()
	return a, p.save()
}

// Remove 删一个号并落盘。
func (p *Pool) Remove(id string) error {
	p.mu.Lock()
	if p.slots[id] == nil {
		p.mu.Unlock()
		return fmt.Errorf("account %q not found", id)
	}
	delete(p.slots, id)
	p.order = slices.DeleteFunc(p.order, func(s string) bool { return s == id })
	p.mu.Unlock()
	return p.save()
}

// SetDisabled 停用 / 启用一个号。启用时清掉冷却与备注。
func (p *Pool) SetDisabled(id string, disabled bool, note string) error {
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return fmt.Errorf("account %q not found", id)
	}
	s.acct.Disabled, s.acct.Note = disabled, note
	if !disabled {
		s.cooldown, s.cooldownWhy, s.strikes, s.refreshFails = time.Time{}, "", 0, 0
	}
	p.mu.Unlock()
	return p.save()
}

// enabledCountLocked 是当前启用（未停用）的号数。
func (p *Pool) enabledCountLocked() int {
	n := 0
	for _, s := range p.slots {
		if !s.acct.Disabled {
			n++
		}
	}
	return n
}

// disableLocked 在 p.mu 下停用失效账号；保留记录供重新登录。
func (p *Pool) disableLocked(s *slot, note string) {
	s.acct.Disabled, s.acct.Note = true, note
}

// SetMaxConcurrent 改一个号的并发上限并落盘；0 用号池默认。
func (p *Pool) SetMaxConcurrent(id string, n int) error {
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return fmt.Errorf("account %q not found", id)
	}
	s.acct.MaxConcurrent = max(0, n)
	p.freed.Broadcast()
	p.mu.Unlock()
	return p.save()
}

// List 是所有号的脱敏快照，按文件顺序。
func (p *Pool) List() []View {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]View, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, p.viewLocked(p.slots[id]))
	}
	return out
}

// Get 是一个号的快照。
func (p *Pool) Get(id string) (View, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slots[id]
	if s == nil {
		return View{}, false
	}
	return p.viewLocked(s), true
}

func (p *Pool) viewLocked(s *slot) View {
	v := View{
		ID: s.acct.ID, Label: s.acct.Label, Method: s.acct.Cred.Method, Source: s.acct.Source,
		Region: s.acct.Cred.APIRegion(), Disabled: s.acct.Disabled, Note: s.acct.Note,
		Reregister: s.acct.Reregister, RefreshFails: s.refreshFails,
		ExpiresAt: s.acct.Cred.ExpiresAt, InFlight: s.inflight,
		Limits: s.limits, LimitsAt: s.limitsAt, Stats: s.stats, HitRate: s.stats.HitRate(),
		Recon: s.recon,
	}
	if p.now().Before(s.cooldown) {
		v.CooldownUntil, v.CooldownWhy = s.cooldown, s.cooldownWhy
	}
	return v
}

// Totals 是全池累计。
func (p *Pool) Totals() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	var t Stats
	for _, s := range p.slots {
		t.Requests += s.stats.Requests
		t.Errors += s.stats.Errors
		t.InputTokens += s.stats.InputTokens
		t.OutputTokens += s.stats.OutputTokens
		t.CacheRead += s.stats.CacheRead
		t.CacheWrite += s.stats.CacheWrite
		t.Credits += s.stats.Credits
		t.CostUSD += s.stats.CostUSD
	}
	return t
}

type sessionLease struct {
	account string
	leases  int
}

// Lease 是一次选号的结果。用完必须 Release。
type Lease struct {
	ID      string
	Pinned  bool // 是否命中会话粘滞
	pool    *Pool
	once    sync.Once
	key     string
	session *sessionLease
}

// Release 归还并发名额。可重复调用。
func (l *Lease) Release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		if s := l.pool.slots[l.ID]; s != nil && s.inflight > 0 {
			s.inflight--
		}
		if l.session != nil {
			l.session.leases--
			if l.session.leases == 0 && l.pool.pending[l.key] == l.session {
				delete(l.pool.pending, l.key)
			}
		}
		l.pool.freed.Broadcast()
		l.pool.mu.Unlock()
	})
}

// DefaultPinWait 是会话钉的号并发打满时等它空出名额的默认最长时间。换号会丢掉该会话在原号上的上游缓存。
const DefaultPinWait = 3 * time.Second

// Acquire 为会话 key 选一个号，跳过 exclude。key 为空时不粘滞。
// 钉的号只是并发打满时先排队，超时返回可重试错误，保留原号缓存。
func (p *Pool) Acquire(key string, exclude []string) (*Lease, error) {
	return p.AcquireContext(context.Background(), key, exclude)
}

// AcquireContext 同 Acquire；ctx 结束时停止排队。
func (p *Pool) AcquireContext(ctx context.Context, key string, exclude []string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, pinned := p.pins.Lookup(key)
	if !pinned && p.pending[key] != nil {
		id = p.pending[key].account
	}
	if id != "" && !slices.Contains(exclude, id) {
		if s := p.slots[id]; s != nil {
			if p.busyLocked(s, now) {
				p.waitFreeLocked(ctx, s)
				now = p.now()
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if p.slots[id] != s {
				l, err := p.pickLocked(now, exclude)
				return p.trackLeaseLocked(key, l, err)
			}
			if p.usableLocked(s, now) {
				return p.trackLeaseLocked(key, p.leaseLocked(s, now, pinned), nil)
			}
			if p.busyLocked(s, now) {
				return nil, &UnavailableError{RetryAt: now.Add(time.Second), Reason: "session account busy; retry on the same account"}
			}
		}
	}
	l, err := p.pickLocked(now, exclude)
	return p.trackLeaseLocked(key, l, err)
}

func (p *Pool) trackLeaseLocked(key string, l *Lease, err error) (*Lease, error) {
	if l == nil || key == "" {
		return l, err
	}
	s := p.pending[key]
	if s == nil || s.account != l.ID {
		s = &sessionLease{account: l.ID}
		p.pending[key] = s
	}
	s.leases++
	l.key, l.session = key, s
	return l, err
}

// busyLocked 报告号本身可用、只是并发名额已满。
func (p *Pool) busyLocked(s *slot, now time.Time) bool {
	limit := p.limitLocked(s)
	return !s.acct.Disabled && !now.Before(s.cooldown) && limit > 0 && s.inflight >= limit
}

// limitLocked 是号的生效并发上限：号上单独设了就用它，否则用号池默认；0 不限。
func (p *Pool) limitLocked(s *slot) int {
	return cmp.Or(s.acct.MaxConcurrent, p.maxConc)
}

// waitFreeLocked 在 p.mu 下等 s 空出名额，最多 pinWait。
func (p *Pool) waitFreeLocked(ctx context.Context, s *slot) {
	wait := p.pinWait
	if wait <= 0 {
		return
	}
	deadline := time.Now().Add(wait)
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.freed.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	timer := time.AfterFunc(wait, func() {
		p.mu.Lock()
		p.freed.Broadcast()
		p.mu.Unlock()
	})
	defer timer.Stop()
	for p.busyLocked(s, p.now()) && ctx.Err() == nil && time.Now().Before(deadline) {
		p.freed.Wait()
	}
}

func (p *Pool) pickLocked(now time.Time, exclude []string) (*Lease, error) {
	var candidates []*slot
	for _, id := range p.order {
		if s := p.slots[id]; !slices.Contains(exclude, id) && p.usableLocked(s, now) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil, p.unavailableLocked(now, exclude)
	}
	// 并发最少优先；再按剩余额度分档（每 20% 一档），取最高档，档内随机。
	// 不按精确剩余排序：那样新号 / 刚重置额度的号会把所有新会话吸走，流量集中到一个号上。
	// 会话粘号不受影响（Acquire 先查钉号）。
	minIn := slices.MinFunc(candidates, func(a, b *slot) int { return cmp.Compare(a.inflight, b.inflight) }).inflight
	top := -1
	var tier []*slot
	for _, s := range candidates {
		if s.inflight != minIn {
			continue
		}
		switch t := quotaTier(s.limits.Remaining()); {
		case t > top:
			top, tier = t, []*slot{s}
		case t == top:
			tier = append(tier, s)
		}
	}
	best := tier[p.pick(len(tier))]
	return p.leaseLocked(best, now, false), nil
}

// quotaTier 把剩余额度比例 [0,1] 分成 5 档：0 = <20% … 4 = ≥80%。
func quotaTier(remaining float64) int {
	return min(4, max(0, int(remaining*5)))
}

func (p *Pool) usableLocked(s *slot, now time.Time) bool {
	limit := p.limitLocked(s)
	return !s.acct.Disabled && !now.Before(s.cooldown) && (limit <= 0 || s.inflight < limit)
}

func (p *Pool) leaseLocked(s *slot, now time.Time, pinned bool) *Lease {
	s.inflight++
	s.lastUsed = now
	return &Lease{ID: s.acct.ID, Pinned: pinned, pool: p}
}

func (p *Pool) unavailableLocked(now time.Time, exclude []string) error {
	if len(p.order) == 0 {
		return &UnavailableError{Reason: "the pool is empty; add an account"}
	}
	var soonest time.Time
	var why string
	for _, id := range p.order {
		s := p.slots[id]
		if s.acct.Disabled || slices.Contains(exclude, id) {
			continue
		}
		if now.Before(s.cooldown) && (soonest.IsZero() || s.cooldown.Before(soonest)) {
			soonest, why = s.cooldown, s.cooldownWhy
		}
	}
	if soonest.IsZero() {
		return &UnavailableError{Reason: "all accounts are disabled, busy or already tried"}
	}
	return &UnavailableError{RetryAt: soonest, Reason: "all accounts cooling down: " + why}
}

// Pin 把会话钉到号上。成功向下游输出后调用。
func (p *Pool) Pin(key, id string) { p.pins.Remember(key, id) }

// Pins 是会话粘滞表（落盘 / 载入用）。
func (p *Pool) Pins() *sessionpin.Table { return p.pins }

// Unpin 解除会话粘滞。
func (p *Pool) Unpin(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, key)
	p.pins.Forget(key)
}

// Success 记一次成功，清掉该号的限流 strikes。
// 注意：成功不清全局熔断的失败历史。窗口内的失败记录靠时间自然过期，
// 另外把这次成功记进窗口，让「失败比例」的分母反映窗口内的活跃号：
// 单个号成功不该抹掉其它号的整段失败历史。
func (p *Pool) Success(id string, u kiro.Usage, costUSD float64, estimated bool) {
	p.mu.Lock()
	now := p.now()
	if s := p.slots[id]; s != nil {
		s.stats.Add(u, costUSD)
		if estimated {
			s.estimated += u.Credits
		} else {
			s.metered += u.Credits
		}
		s.strikes = 0
	}
	p.recordSuccessLocked(id, now)
	p.mu.Unlock()
	p.saveState()
}

// suspectLocked 记一次可能是全局故障的失败结果，返回这种失败此刻是否熔断。
// 若这次失败让熔断首次触发，则回退窗口内已计入各号的 refreshFails：
// 触发前的失败是全局故障的征兆，不该算成某个号自己的连续失败。
func (p *Pool) suspectLocked(kind, id string, now time.Time) bool {
	p.recordLocked(kind, id, now, true)
	_, was := p.brk.since[kind]
	open := p.trippedLocked(kind, now)
	if open && !was {
		p.rollbackStrikesLocked(kind, now)
	}
	return open
}

// recordSuccessLocked 记一次成功的请求结果（也进比例窗口的分母）。
func (p *Pool) recordSuccessLocked(id string, now time.Time) {
	p.recordLocked("", id, now, false)
}

func (p *Pool) recordLocked(kind, id string, now time.Time, fail bool) {
	p.brk.outcomes = append(p.brk.outcomes, outcome{Kind: kind, ID: id, At: now, Fail: fail})
	// 结果按时间追加：从队首丢掉窗口外的旧结果，避免没有熔断查询时无限增长。
	cut := 0
	for cut < len(p.brk.outcomes) && now.Sub(p.brk.outcomes[cut].At) > p.brk.window {
		cut++
	}
	if cut > 0 {
		p.brk.outcomes = append(p.brk.outcomes[:0], p.brk.outcomes[cut:]...)
	}
}

// countStrikeLocked 记下这个号在熔断触发前已经被计入的失败时刻（refreshFails）。
// 熔断一旦触发就按时间窗回退：这些是全局故障，不该算成某个号自己的连续失败。
func (p *Pool) countStrikeLocked(kind, id string, now time.Time) {
	m := p.brk.counted[kind]
	if m == nil {
		m = map[string][]time.Time{}
		p.brk.counted[kind] = m
	}
	m[id] = append(m[id], now)
}

// rollbackStrikesLocked 把某种失败在触发前计入各号的 refreshFails 回退，仅回退仍在窗口内的记录：
// 过期的旧失败不在本窗口内，不该被回退，也不该继续记在 counted 里。
func (p *Pool) rollbackStrikesLocked(kind string, now time.Time) {
	m := p.brk.counted[kind]
	for id, times := range m {
		s := p.slots[id]
		for _, at := range times {
			if now.Sub(at) <= p.brk.window && s != nil {
				s.refreshFails = max(0, s.refreshFails-1) // 仅回退仍在窗口内的失败
			}
		}
		delete(m, id) // 触发后清空记账；过期的记录一并丢弃
	}
}

// trippedLocked 报告一种失败是否熔断：滑动窗口内该种类的失败结果数 / 全部请求结果数 > 1/2，
// 且失败覆盖 >= 2 个不同的号。只有一个号失败时按单号逻辑走。
func (p *Pool) trippedLocked(kind string, now time.Time) bool {
	fail, total, distinct := p.breakerWindowLocked(kind, now)
	open := distinct >= 2 && 2*fail > total
	_, was := p.brk.since[kind]
	switch {
	case open && !was:
		p.brk.since[kind] = now
		p.log.Error("global breaker open: most requests fail the same way; cooling only, not disabling accounts",
			"kind", kind, "failures", fail, "total", total, "accounts", distinct, "window", p.brk.window)
	case !open && was:
		delete(p.brk.since, kind)
		clear(p.brk.counted[kind]) // 熔断解除：触发前的记账作废，重新开始累计
		p.log.Info("global breaker closed", "kind", kind)
	}
	return open
}

// breakerWindowLocked 丢弃窗口外的结果，返回窗口内该种类的失败数、全部结果数、失败的 distinct 号数。
// 只统计仍启用、仍存在的号：手动停用后残留的旧失败记录不再参与判断，保证熔断要覆盖 >=2 个
// 「启用」号——只剩最后一个启用号时绝不用熔断（与 last-account 保护一致）。
func (p *Pool) breakerWindowLocked(kind string, now time.Time) (fail, total, distinct int) {
	keep := p.brk.outcomes[:0]
	ids := map[string]struct{}{}
	for _, o := range p.brk.outcomes {
		if now.Sub(o.At) > p.brk.window {
			continue // 过期：丢掉
		}
		s := p.slots[o.ID]
		if s == nil || s.acct.Disabled {
			continue // 号已删除 / 已停用：它的历史不再影响全局判断
		}
		keep = append(keep, o)
		total++
		if o.Fail && o.Kind == kind {
			fail++
			ids[o.ID] = struct{}{}
		}
	}
	// 清掉可能残留的 nil
	for i := len(keep); i < len(p.brk.outcomes); i++ {
		p.brk.outcomes[i] = outcome{}
	}
	p.brk.outcomes = keep
	return fail, total, len(ids)
}

// Breaker 是全局熔断的当前状态。
func (p *Pool) Breaker() BreakerView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	v := BreakerView{Window: p.brk.window.String()}
	for _, kind := range []string{FailForbidden, FailNetwork, FailRefresh} {
		if !p.trippedLocked(kind, now) {
			continue
		}
		fail, total, distinct := p.breakerWindowLocked(kind, now)
		ratio := 0.0
		if total > 0 {
			ratio = float64(fail) / float64(total)
		}
		v.Open = true
		v.Kinds = append(v.Kinds, BreakerKind{
			Kind: kind, Accounts: distinct, Enabled: p.enabledCountLocked(),
			Failures: fail, Total: total, Ratio: ratio, Since: p.brk.since[kind],
		})
	}
	return v
}

// Charge 记一次没有成功的尝试已消耗的量（中途断开 / 客户端中断）：上游已扣 credits。
// estimated 表示 u.Credits 是按 token 估的（上游没报），对账里单独记。不计请求数、不清失败计数。
func (p *Pool) Charge(id string, u kiro.Usage, costUSD float64, estimated bool) {
	p.mu.Lock()
	if s := p.slots[id]; s != nil {
		if estimated {
			s.estimated += u.Credits
		} else {
			s.metered += u.Credits
		}
		st := &s.stats
		st.Credits += u.Credits
		st.CostUSD += costUSD
		st.InputTokens += int64(u.Input)
		st.OutputTokens += int64(u.Output)
		st.CacheRead += int64(u.CacheRead)
		st.CacheWrite += int64(u.CacheWrite)
	}
	p.mu.Unlock()
	p.saveState()
}

// Fail 按失败类别给号冷却。返回是否值得换号重试。
func (p *Pool) Fail(id string, f kiro.Failure) (retry bool) {
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return f.Class != kiro.ClassFatal
	}
	s.stats.Errors++
	now := p.now()
	switch f.Class {
	case kiro.ClassQuota:
		until := now.Add(time.Hour)
		if s.limits.ResetAt.After(now) {
			until = s.limits.ResetAt
		}
		if f.RetryAfter > 0 && !s.limits.ResetAt.After(now) {
			until = now.Add(f.RetryAfter)
		}
		p.coolLocked(s, until, "usage limit: "+kiro.Diagnostic(f.Message))
	case kiro.ClassSoftBan:
		// 风控 429：不是普通限流，长冷却并换号（告警在下面统一打）
		p.coolLocked(s, now.Add(softBanCooldown), "suspicious activity (soft ban): "+kiro.Diagnostic(f.Message))
	case kiro.ClassThrottle:
		backoff := min(15*time.Second<<s.strikes, 5*time.Minute)
		if f.RetryAfter > 0 {
			backoff = f.RetryAfter // 上游给了窗口就按它，不自己猜
		}
		s.strikes = min(s.strikes+1, 8)
		p.coolLocked(s, now.Add(backoff), "throttled: "+kiro.Diagnostic(f.Message))
	case kiro.ClassTransient:
		if f.Network {
			p.suspectLocked(FailNetwork, id, now)
		}
		p.coolLocked(s, now.Add(10*time.Second), "upstream: "+kiro.Diagnostic(f.Message))
	case kiro.ClassAuth:
		p.coolLocked(s, now.Add(time.Minute), "auth: "+kiro.Diagnostic(f.Message))
	case kiro.ClassForbidden:
		p.suspectLocked(FailForbidden, id, now)
		p.coolLocked(s, now.Add(time.Minute), "forbidden: "+kiro.Diagnostic(f.Message))
	case kiro.ClassBanned:
		p.disableLocked(s, kiro.Diagnostic(f.Message))
	}
	banned := f.Class == kiro.ClassBanned
	p.mu.Unlock()
	if banned {
		p.log.Error("account suspended upstream", "account", id, "note", kiro.Diagnostic(f.Message))
	}
	if f.Class == kiro.ClassSoftBan {
		p.log.Error("account soft-banned by upstream risk control; cooling 24h", "account", id, "status", f.Status, "msg", kiro.Diagnostic(f.Message))
	}
	p.saveState()
	if banned {
		if err := p.save(); err != nil {
			p.log.Error("save pool", "err", err)
		}
	}
	p.log.Warn("account failure", "account", id, "status", f.Status, "class", f.Class, "msg", kiro.Diagnostic(f.Message))
	return f.Class != kiro.ClassFatal
}

// reconLocked 用一次新拉到的额度更新对账。
func (s *slot) reconLocked(l kiro.Limits, now time.Time) {
	r := &s.recon
	used := l.Consumed()
	if r.Since.IsZero() {
		r.Since, r.baseUsed, r.baseMeter, r.baseEst = now, used, s.metered, s.estimated
		return
	}
	if used+1e-9 < r.baseUsed {
		// 额度重置：结清旧段（重置前的那点已用无从得知），从新基线继续
		r.Resets++
		r.baseUsed, r.baseMeter, r.baseEst = used, s.metered, s.estimated
		return
	}
	r.Upstream += used - r.baseUsed
	r.Reported += s.metered - r.baseMeter
	r.Estimated += s.estimated - r.baseEst
	r.Recorded = r.Reported + r.Estimated
	r.baseUsed, r.baseMeter, r.baseEst = used, s.metered, s.estimated
	r.Unassigned = r.Upstream - r.Recorded
}

func (p *Pool) coolLocked(s *slot, until time.Time, why string) {
	if until.After(s.cooldown) {
		s.cooldown, s.cooldownWhy = until, why
	}
}

// Cred 返回号的可用凭证：快过期就刷新，缺 profile 就补。
// stale 非空表示上游刚拒了这个 access token（403）：若号上仍是它就强制刷新，
// 若已被并发请求刷新过则直接用新的，不重复花 refresh token。
// refresh 被拒（凭证失效）时停用该号。
func (p *Pool) Cred(ctx context.Context, id, stale string) (kiro.Cred, error) {
	p.mu.Lock()
	s := p.slots[id]
	p.mu.Unlock()
	if s == nil {
		return kiro.Cred{}, fmt.Errorf("account %q not found", id)
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	p.mu.Lock()
	acct := s.acct
	p.mu.Unlock()
	cred := acct.Cred

	if acct.Source == SourceIDE && acct.SourcePath != "" {
		// IDE 可能自己刷新过：用文件里更新的 token，避免拿旧 refresh token 去换
		if fromFile, err := kiro.ReadIDE(acct.SourcePath); err == nil && fromFile.ExpiresAt.After(cred.ExpiresAt) {
			fromFile.ProfileArn = cmp.Or(fromFile.ProfileArn, cred.ProfileArn)
			fromFile.MachineID = cmp.Or(fromFile.MachineID, cred.MachineID)
			fromFile.ClientSecretExpiresAt = cmp.Or(fromFile.ClientSecretExpiresAt, cred.ClientSecretExpiresAt)
			cred = fromFile
		}
	}

	// 旧号没有机器码：第一次用时生成并持久化，之后固定
	if cred.MachineID == "" {
		cred.MachineID = cmp.Or(acct.Cred.MachineID, kiro.NewMachineID())
	}
	// 只有 refresh token 的号（手动加的 idc / social）没有 access token，也得先换
	force := cred.AccessToken == "" || (stale != "" && stale == cred.AccessToken)
	changed := cred != acct.Cred
	if force || !cred.Fresh(refreshLead) {
		select {
		case p.refreshSem <- struct{}{}:
		case <-ctx.Done():
			return kiro.Cred{}, ctx.Err()
		}
		n, err := p.client.Refresh(ctx, cred)
		<-p.refreshSem
		if err != nil {
			p.refreshFailed(ctx, id, s, err)
			return kiro.Cred{}, err
		}
		p.refreshOK(s)
		cred, changed = n, true
		if acct.Source == SourceIDE && acct.SourcePath != "" {
			if err := kiro.WriteBackIDE(acct.SourcePath, cred); err != nil {
				p.log.Warn("write back IDE token", "account", id, "err", err)
			}
		}
		p.log.Info("token refreshed", "account", id, "expires", cred.ExpiresAt.Format(time.RFC3339))
	}
	if cred.ProfileArn == "" {
		arn, err := p.client.Profile(ctx, cred)
		if err != nil {
			return kiro.Cred{}, fmt.Errorf("find profile: %w", err)
		}
		cred.ProfileArn, changed = arn, true
	}
	if changed {
		p.mu.Lock()
		s.acct.Cred = cred
		p.mu.Unlock()
		if err := p.save(); err != nil {
			p.log.Error("save pool", "err", err)
		}
	}
	return cred, nil
}

// refreshFailed 按失败类别处理一次刷新失败：
//   - 封号文字、invalid_grant（Gone）：立即停号；
//   - IdC client 注册过期：标 Reregister（管理台提示重新登录）并冷却，不停号；
//   - 其它 4xx（防火墙页、短暂 401 / 403……）：计一次失败并冷却，连续 maxRefreshFailures 次才停号；
//   - 5xx / 网络：冷却，不计次数（不是凭证的问题）。
//
// 封号与 invalid_grant 之外的失败都进全局熔断：多数号同时出同一种失败时只冷却、不计次数、不停号。
func (p *Pool) refreshFailed(ctx context.Context, id string, s *slot, err error) {
	e, isKiro := errors.AsType[*kiro.Error](err)
	switch {
	case isKiro && kiro.Banned(e.Status, []byte(e.Body)):
		p.mu.Lock()
		p.disableLocked(s, "account suspended: "+kiro.Diagnostic(err.Error()))
		p.mu.Unlock()
		p.log.Error("account suspended upstream", "account", id, "err", err)
		p.saveState()
		if serr := p.save(); serr != nil {
			p.log.Error("save pool", "err", serr)
		}
		return
	case kiro.IsGone(err):
		p.mu.Lock()
		p.disableLocked(s, "sign-in expired: "+kiro.Diagnostic(err.Error()))
		p.mu.Unlock()
		p.log.Error("account disabled", "account", id, "err", err)
		p.saveState()
		if serr := p.save(); serr != nil {
			p.log.Error("save pool", "err", serr)
		}
		return
	case kiro.IsReregister(err):
		p.mu.Lock()
		s.acct.Reregister = true
		p.coolLocked(s, p.now().Add(time.Hour), "sign in again: the AWS client registration expired")
		p.mu.Unlock()
		p.saveState()
		if serr := p.save(); serr != nil {
			p.log.Error("save pool", "err", serr)
		}
		p.log.Error("IdC client registration expired; sign in again", "account", id, "err", err)
		return
	case !isKiro || e.Status == 0 || e.Status >= 500:
		p.mu.Lock()
		if ctx.Err() == nil { // 调用方走了不算上游故障
			kind := FailRefresh
			if !isKiro || e.Status == 0 {
				kind = FailNetwork
			}
			p.suspectLocked(kind, id, p.now())
		}
		p.coolLocked(s, p.now().Add(10*time.Second), "refresh: "+kiro.Diagnostic(err.Error()))
		p.mu.Unlock()
		p.saveState()
		return
	}
	kind := FailRefresh
	if e.Status == 403 {
		kind = FailForbidden
	}
	p.mu.Lock()
	if p.suspectLocked(kind, id, p.now()) {
		p.coolLocked(s, p.now().Add(refreshCooldown), "refresh failed (global breaker open, not counted): "+kiro.Diagnostic(err.Error()))
		p.mu.Unlock()
		p.saveState()
		p.log.Warn("token refresh failed during global breaker; not counted", "account", id, "kind", kind, "err", err)
		return
	}
	s.refreshFails++
	n := s.refreshFails
	p.countStrikeLocked(kind, id, p.now()) // 记录：若之后熔断触发，这些失败要整体回退
	p.coolLocked(s, p.now().Add(refreshCooldown), fmt.Sprintf("refresh failed (%d/%d): %v", n, maxRefreshFailures, kiro.Diagnostic(err.Error())))
	if n >= maxRefreshFailures {
		p.disableLocked(s, fmt.Sprintf("refresh failed %d times in a row: %v", n, kiro.Diagnostic(err.Error())))
	}
	p.mu.Unlock()
	if n >= maxRefreshFailures {
		p.saveState()
		if err := p.save(); err != nil {
			p.log.Error("save pool", "err", err)
		}
		p.log.Error("account disabled after repeated refresh failures", "account", id, "failures", n, "err", err)
		return
	}
	p.saveState()
	p.log.Warn("token refresh failed", "account", id, "failures", n, "of", maxRefreshFailures, "err", err)
}

// refreshOK 在刷新成功后清零失败计数与 Reregister。
// 它不清全局熔断的失败历史：刷新成功只说明这个号的凭证能用，
// 其它号在窗口内的失败记录要靠时间自然过期，不能被一次成功抹掉。
func (p *Pool) refreshOK(s *slot) {
	p.mu.Lock()
	s.refreshFails = 0
	re := s.acct.Reregister
	s.acct.Reregister = false
	p.mu.Unlock()
	p.saveState()
	if re {
		_ = p.save()
	}
}

// ForceRefresh 无论是否快过期都刷新一次 token。
func (p *Pool) ForceRefresh(ctx context.Context, id string) (kiro.Cred, error) {
	p.mu.Lock()
	s := p.slots[id]
	var current string
	if s != nil {
		current = s.acct.Cred.AccessToken
	}
	p.mu.Unlock()
	return p.Cred(ctx, id, cmp.Or(current, "-"))
}

// Client 是号池用的 Kiro 客户端。
func (p *Pool) Client() *kiro.Client { return p.client }

// RefreshLimits 拉一个号的额度；用尽则冷却到重置时间，没有名字就用邮箱。
func (p *Pool) RefreshLimits(ctx context.Context, id string) (kiro.Limits, error) {
	cred, err := p.Cred(ctx, id, "")
	if err != nil {
		return kiro.Limits{}, err
	}
	l, err := p.client.UsageLimits(ctx, cred)
	if err != nil {
		return kiro.Limits{}, err
	}
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return l, nil
	}
	s.limits, s.limitsAt = l, p.now()
	s.reconLocked(l, s.limitsAt)
	if l.Limit > 0 && l.Remaining() <= 0 {
		until := l.ResetAt
		if !until.After(p.now()) {
			until = p.now().Add(time.Hour)
		}
		p.coolLocked(s, until, "credits exhausted")
	}
	relabel := s.acct.Label == "" && l.Email != ""
	if relabel {
		s.acct.Label = l.Email
	}
	p.mu.Unlock()
	if relabel {
		return l, p.save()
	}
	return l, nil
}

// PollLimits 每隔 every 刷新所有启用号的额度，直到 ctx 结束。第一轮立即跑。
func (p *Pool) PollLimits(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, v := range p.List() {
			if v.Disabled {
				continue
			}
			if _, err := p.RefreshLimits(ctx, v.ID); err != nil && ctx.Err() == nil {
				p.log.Warn("usage limits", "account", v.ID, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Pool) save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	f := file{Accounts: make([]Account, 0, len(p.order))}
	for _, id := range p.order {
		f.Accounts = append(f.Accounts, p.slots[id].acct)
	}
	p.mu.Unlock()
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return kiro.WriteFileAtomic(p.path, raw)
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "acc_" + hex.EncodeToString(b[:])
}
