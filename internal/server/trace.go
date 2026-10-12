package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kiro-proxy/internal/kiro"
)

type requestTrace struct {
	Account  string            `json:"account"`
	Request  string            `json:"request"`
	Attempt  int               `json:"attempt"`
	Previous string            `json:"previous_request,omitzero"`
	Prefix   *kiro.PrefixCheck `json:"prefix,omitzero"`
}

// traceFilenamePad 是估算快照大小时为 request 文件名（CreateTemp 后才确定）预留的上界。
const traceFilenamePad = 128

// isHex64 / isDigits 用于识别代理自己写的快照命名，避免误删共享目录里的其它文件。
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// traceSnapshotGroup 解析调试快照文件名，返回同属一次快照的组键（含 meta 配对）。
// 只识别代理的命名（<64hex>.latest.json[/.meta.json]、<64hex>-<digits>.request.json[/.meta.json]）；
// 其它名字返回 ok=false，永不清理。
func traceSnapshotGroup(name string) (group string, isMeta, ok bool) {
	isMeta = strings.HasSuffix(name, ".meta.json")
	n := name
	if isMeta {
		n = strings.TrimSuffix(name, ".meta.json")
		// latest 的 meta 是 <stem>.latest.meta.json：剥掉 .meta.json 后是 <stem>.latest。
		if strings.HasSuffix(n, ".latest") {
			if stem := strings.TrimSuffix(n, ".latest"); isHex64(stem) {
				return stem + "|latest", true, true
			}
			return "", true, false
		}
		// 其余（request 的 meta）继续走下面 .request.json 的分支。
	}
	switch {
	case !isMeta && strings.HasSuffix(n, ".latest.json"):
		if stem := strings.TrimSuffix(n, ".latest.json"); isHex64(stem) {
			return stem + "|latest", false, true
		}
	case strings.HasSuffix(n, ".request.json"):
		base := strings.TrimSuffix(n, ".request.json")
		if i := strings.LastIndexByte(base, '-'); i > 0 && isHex64(base[:i]) && isDigits(base[i+1:]) {
			return base, isMeta, true
		}
	}
	return "", isMeta, false
}

// pruneDebugDir 清理调试快照目录：先删过期的整组快照，再按最旧组优先把总量压到
// maxBytes 以内（maxBytes==0 表示保留 0 字节，即清掉全部托管快照，不是"不设限"）。
// 只处理代理命名的快照；其它文件、目录与符号链接一概不动。残缺的
// 半组快照（只有 request/latest 没有 meta）整组删掉，让 traceRequest 冷启动而不是读崩。
// 目录不存在时返回 0, nil。调用方需与 traceRequest 共用锁。
func pruneDebugDir(dir string, maxBytes int64, retention time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	type snapFile struct {
		path   string
		size   int64
		mod    time.Time
		isMeta bool
	}
	type groupInfo struct {
		files   []snapFile
		hasMain bool
		hasMeta bool
		newest  time.Time
	}
	groups := map[string]*groupInfo{}
	for _, e := range entries {
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			continue // 不动目录，也绝不跟随符号链接（可能指向目录外）
		}
		group, isMeta, ok := traceSnapshotGroup(e.Name())
		if !ok {
			continue // 不是代理快照
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		g := groups[group]
		if g == nil {
			g = &groupInfo{}
			groups[group] = g
		}
		g.files = append(g.files, snapFile{path: filepath.Join(dir, e.Name()), size: info.Size(), mod: info.ModTime(), isMeta: isMeta})
		if isMeta {
			g.hasMeta = true
		} else {
			g.hasMain = true
		}
		if info.ModTime().After(g.newest) {
			g.newest = info.ModTime()
		}
	}
	removed := 0
	removeGroup := func(g *groupInfo) error {
		for _, f := range g.files {
			if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed++
		}
		g.files = nil
		return nil
	}
	// 1) 残缺组（缺 main 或 meta）直接清掉。
	for _, g := range groups {
		if g.hasMain && g.hasMeta {
			continue
		}
		if err := removeGroup(g); err != nil {
			return removed, err
		}
	}
	// 2) 保留期：整组过期才删。
	var kept []*groupInfo
	for _, g := range groups {
		if g.files == nil {
			continue
		}
		if retention > 0 && now.Sub(g.newest) > retention {
			if err := removeGroup(g); err != nil {
				return removed, err
			}
			continue
		}
		kept = append(kept, g)
	}
	// 3) 字节上限：最旧的组先整组删。
	if maxBytes >= 0 {
		var total int64
		for _, g := range kept {
			for _, f := range g.files {
				total += f.size
			}
		}
		sort.Slice(kept, func(i, j int) bool { return kept[i].newest.Before(kept[j].newest) })
		for _, g := range kept {
			if total <= maxBytes {
				break
			}
			for _, f := range g.files {
				total -= f.size
			}
			if err := removeGroup(g); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

// pruneDir 间接引用 pruneDebugDir，便于测试注入清理失败。
var pruneDir = pruneDebugDir

// debugCleanupEvery 是调试目录周期清理的间隔。
const debugCleanupEvery = 10 * time.Minute

// pruneDebugRequests 在 traceMu 下清理调试目录：与 traceRequest 的写入串行，避免删掉正在写的快照。
func (s *Server) pruneDebugRequests() {
	s.traceMu.Lock()
	n, err := pruneDir(s.cfg.DebugRequestsDir, s.cfg.DebugRequestsCap(), s.cfg.DebugRequestsTTL(), time.Now())
	s.traceMu.Unlock()
	if err != nil {
		s.log.Warn("debug request cleanup", "err", err)
		return
	}
	if n > 0 {
		s.log.Debug("debug request cleanup removed snapshots", "count", n)
	}
}

// cleanupDebugDir 周期清理调试目录；bg 结束时退出。启动时已先清一次（见 New）。
func (s *Server) cleanupDebugDir() {
	t := time.NewTicker(debugCleanupEvery)
	defer t.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-t.C:
			s.pruneDebugRequests()
		}
	}
}

// traceRequest 保存真正传给 Generate 的（已脱敏）字节：Authorization、profileArn、图片
// 字节与显式命名的密钥都被替换为 [REDACTED]，对话正文保留供前缀诊断。仅在显式打开
// 调试开关时执行；写完立即按容量上限清理，避免目录无界增长。
//
// 容量边界：单快照合计超过上限时整条跳过（只 warn 尺寸）。写前按 cap-footprint 预清理，
// 失败即跳过写，保证稳态不超上限。限制：kiro.WriteFileAtomic 写每个文件时会先在同一
// 目录落一个 .tmp 副本再 rename，因此单次写盘的瞬时占用比 footprint 略高（每个 ~meta 一个
// 临时副本，都是短命文件）；pre-flight prune 无法把这一点完全算进去，故峰值上界为
// cap + 一点临时副本余量（单个小文件），不会无界增长。
func (s *Server) traceRequest(c *call, account string, payload []byte) error {
	dir := s.cfg.DebugRequestsDir
	if dir == "" {
		return nil
	}
	var err error
	payload, err = kiro.RedactPayload(payload)
	if err != nil {
		return err
	}
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	key := c.traceKey
	if key == "" {
		key = c.entry.Request
	}
	stem := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	latest := filepath.Join(dir, stem+".latest.json")
	metadata := filepath.Join(dir, stem+".latest.meta.json")
	trace := requestTrace{Account: account, Attempt: c.entry.Attempts}
	previous, err := os.ReadFile(latest)
	switch {
	case err == nil:
		var old requestTrace
		raw, err := os.ReadFile(metadata)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		check, err := kiro.CheckPrefix(previous, payload)
		if err != nil {
			return err
		}
		check.PreviousAccount, check.Account, check.SameAccount = old.Account, account, old.Account == account
		trace.Previous, trace.Prefix = old.Request, &check
		if block := check.DifferentBlock; block != nil {
			s.log.Debug("upstream prefix differs", "index", block.Index, "role", block.Role, "type", block.Type, "preview", block.Preview)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	// 先按序列化后的元数据估算单快照在盘上的实际字节：request + latest 各一份 payload，
	// 两个 .meta.json 各一份 raw。request 文件名要等 CreateTemp 才定，用上界补齐。
	// 单快照本身就超过上限时整条跳过，避免"写了又立刻删"并挤掉已有有效快照。
	rawBase, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return err
	}
	capBytes := s.cfg.DebugRequestsCap()
	footprint := int64(2*len(payload)) + 2*int64(len(rawBase)+traceFilenamePad)
	if capBytes > 0 && footprint > capBytes {
		// 只报尺寸，不落任何正文。
		s.log.Warn("debug request snapshot exceeds debug_requests_max_bytes; skipped",
			"payload_bytes", len(payload), "snapshot_bytes", footprint, "cap_bytes", capBytes)
		return nil
	}
	// 写前预留：先把已有快照压到 cap-footprint，保证加入新快照后仍在上限内（含启动后首个 tick 前）。
	// 余量 <=0（含 footprint 恰等于 cap 的边界）时按"保留 0 字节"清掉已有托管快照；
	// 清理失败则跳过写，避免在余量未知时继续写而超限。
	reserve := capBytes - footprint
	if reserve < 0 {
		reserve = 0
	}
	if _, err := pruneDir(dir, reserve, s.cfg.DebugRequestsTTL(), time.Now()); err != nil {
		s.log.Warn("debug request pre-write cleanup failed; skipping snapshot", "err", err)
		return nil
	}
	file, err := os.CreateTemp(dir, stem+"-*.request.json")
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	trace.Request = filepath.Base(file.Name())
	raw, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return err
	}
	if err := kiro.WriteFileAtomic(file.Name()+".meta.json", raw); err != nil {
		return err
	}
	if err := kiro.WriteFileAtomic(latest, payload); err != nil {
		return err
	}
	if err := kiro.WriteFileAtomic(metadata, raw); err != nil {
		return err
	}
	// 写后再按上限收拾一次（写前预留用的是估算），周期清理只作为兜底；这里失败不丢已写好的快照。
	if n, err := pruneDir(dir, capBytes, s.cfg.DebugRequestsTTL(), time.Now()); err != nil {
		s.log.Warn("debug request cleanup after write", "err", err)
	} else if n > 0 {
		s.log.Debug("debug request cleanup removed snapshots", "count", n)
	}
	return nil
}
