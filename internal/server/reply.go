package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"kiro-proxy/internal/anthropic"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
)

const pingEvery = 15 * time.Second

// timingSink 记录首个内容事件时间，用于 TTFT / TPS。
type timingSink struct {
	turn.Sink
	start time.Time
	first time.Time
}

func (t *timingSink) mark() {
	if t.first.IsZero() {
		t.first = time.Now()
	}
}

func (t *timingSink) Text(s string)             { t.mark(); t.Sink.Text(s) }
func (t *timingSink) Thinking(s string)         { t.mark(); t.Sink.Thinking(s) }
func (t *timingSink) ToolStart(id, name string) { t.mark(); t.Sink.ToolStart(id, name) }

func (t *timingSink) firstMS() int64 {
	if t.first.IsZero() {
		return 0
	}
	return t.first.Sub(t.start).Milliseconds()
}

// reply 是一次回复的结局。ok 与 failure 至多一个为真；都不是表示下游自己走了。
type reply struct {
	// usage 是本地计量。成功时完整；中途失败 / 下游走了时是已消耗的部分：
	// 上游已开始回复就说明整段输入已处理，再加上已收到的输出与已报的 credits。
	usage   turn.Usage
	kiro    kiro.Usage // 上游报的：credits、上下文百分比
	ok      bool
	failure *kiro.Failure // 上游中途报错或断流，应记到号上
}

func failed(text string) reply {
	f := kiro.ClassifyStream(text)
	return reply{failure: &f}
}

const brokeOff = "the reply ended before it was complete"

// toolCallTokens 是每个工具调用在输出侧的包装开销。
const toolCallTokens = 8

// pump 把解码事件写进 sink。first 是已经读出的第一条事件。
// input 是输入侧用量；settle 给完整用量填费用并记账，在 sink.End 之前调用：
// 客户端收到结尾时账已记上，紧接着的下一个请求做预算判断能看到它。输出 token 在本地按实际内容计。
// 上游中途失败时调 sink.Fail 并把失败返回；下游走了则不再写 sink。
// pumpOpts 是上游报了 tokenUsage 时的口径。
type pumpOpts struct {
	mode      string // kiro.Reported*
	maxTokens int
	stops     []string
	abort     func()
}

// reportedUsage 合并上游已报的记账；未报的部分保留本地计量。
// 首条内容前失败时传入零用量，不能假定本地整段输入已被上游处理。
func reportedUsage(u turn.Usage, k kiro.Usage, po pumpOpts) turn.Usage {
	if k.Reported && po.mode != kiro.ReportedIgnore {
		// 上游报的输入是整段上下文，仍含 Kiro 自带的隐藏基线（口径未测定，按配置基线保守切分）。
		// 把隐藏 token 从客户端输入里切出来单独记账：先切未命中，再切写入，最后切读，
		// 客户端各项都不为负，且 Input+CacheRead+CacheWrite+KiroInput 守恒。
		in, read, write := k.Input, k.CacheRead, k.CacheWrite
		hidden := min(max(u.KiroInput, 0), in+read+write)
		rem := hidden
		if cut := min(rem, in); cut > 0 {
			in, rem = in-cut, rem-cut
		}
		if cut := min(rem, write); cut > 0 {
			write, rem = write-cut, rem-cut
		}
		if cut := min(rem, read); cut > 0 {
			read -= cut
		}
		// 上游不分 TTL：1h 按本地拆分的占比搬到切分后的 cacheWrite 上。
		u.CacheWrite1h = meter.Scale1h(u.CacheWrite1h, u.CacheWrite, write)
		u.Input, u.CacheRead, u.CacheWrite = in, read, write
		u.KiroInput = hidden
	}
	if k.OutputReported && po.mode != kiro.ReportedIgnore {
		u.Output = max(k.Output, u.Reasoning)
	}
	u.Credits = k.Credits
	return u
}

func pump(ctx context.Context, dec *kiro.Decoder, first kiro.Event, sink turn.Sink, input turn.Usage, settle func(turn.Usage, kiro.Usage) turn.Usage, po pumpOpts) reply {
	ctx, cancel := context.WithCancel(ctx)
	// The accepted calibration range begins at 0.5; this is an estimate, not an upstream guarantee.
	initial := input
	initial.Input /= 2
	initial.CacheRead /= 2
	initial.CacheWrite /= 2
	initial.CacheWrite1h = min(initial.CacheWrite, initial.CacheWrite1h/2)
	sink.Begin(initial)

	// 上游在长 thinking 时可能很久不出字，定时 ping 防止客户端 / 代理断开
	events := make(chan kiro.Event)
	readerDone := make(chan struct{})
	// 返回前等读协程退出：调用方随后会 drain/关闭同一个 body，不能并发读
	stopReader := func() {
		cancel()
		if po.abort != nil {
			po.abort()
		}
		<-readerDone
	}
	defer stopReader()
	go func() {
		defer close(readerDone)
		defer close(events)
		ev, err := first, error(nil)
		for err == nil {
			select {
			case events <- ev:
			case <-ctx.Done():
				return
			}
			ev, err = dec.Next()
		}
	}()
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()

	var out, think strings.Builder
	tools := 0
	pending := ""
	limited := false
	sequence := ""
	// measure 是到目前为止的用量：k 是上游报的记账。
	measure := func(k kiro.Usage) turn.Usage {
		u := input
		u.Reasoning = meter.Count(think.String())
		u.Output = meter.Count(out.String()) + u.Reasoning + tools*toolCallTokens
		localOutput := u.Output
		u = reportedUsage(u, k, po)
		if limited {
			u.Output = localOutput
			u.Credits = 0
			u.Limited = true
			u.StopSequence = sequence
		}
		return u
	}
	// partial 在没正常结束时取已消耗的量。先等读协程退出：Decoder 不能并发访问。
	partial := func(r reply) reply {
		stopReader()
		r.usage = measure(dec.Partial())
		return r
	}
	fail := func(text string) reply {
		r := failed(text)
		sink.Fail(r.failure.Status, r.failure.Message)
		return partial(r)
	}
	finish := func(stop string, k kiro.Usage) reply {
		stopReader()
		u := settle(measure(k), k)
		sink.End(stop, u)
		return reply{usage: u, kiro: k, ok: true}
	}
	deliver := func(text string, thinking bool, send func(string)) bool {
		count := func(s string) int {
			if thinking {
				return meter.Count(out.String()) + meter.Count(think.String()+s) + tools*toolCallTokens
			}
			return meter.Count(out.String()+s) + meter.Count(think.String()) + tools*toolCallTokens
		}
		truncated := false
		if po.maxTokens > 0 && count(text) > po.maxTokens {
			end := 0
			for end < len(text) {
				_, n := utf8.DecodeRuneInString(text[end:])
				if count(text[:end+n]) > po.maxTokens {
					break
				}
				end += n
			}
			text = text[:end]
			truncated = true
		}
		if text != "" {
			if thinking {
				think.WriteString(text)
			} else {
				out.WriteString(text)
			}
			send(text)
		}
		return truncated || (po.maxTokens > 0 && count("") >= po.maxTokens)
	}
	flushText := func(final bool) (string, bool) {
		at := -1
		for _, stop := range po.stops {
			if n := strings.Index(pending, stop); n >= 0 && (at < 0 || n < at) {
				at = n
				sequence = stop
			}
		}
		if at >= 0 {
			prefix := pending[:at]
			pending = ""
			return prefix, true
		}
		keep := 0
		if !final {
			for _, stop := range po.stops {
				for n := 1; n < len(stop) && n <= len(pending); n++ {
					if strings.HasSuffix(pending, stop[:n]) {
						keep = max(keep, n)
					}
				}
			}
		}
		text := pending[:len(pending)-keep]
		pending = pending[len(pending)-keep:]
		return text, false
	}

	for {
		var ev kiro.Event
		var more bool
		select {
		case <-ctx.Done():
			return fail("upstream timeout or client interruption")
		case <-ping.C:
			sink.Ping()
			continue
		case ev, more = <-events:
		}
		if !more {
			if ctx.Err() != nil {
				return fail("upstream timeout or client interruption")
			}
			return fail(brokeOff)
		}
		// A different block ends the text run. Flush a possible stop prefix
		// before opening thinking/tools, rather than reordering it after them.
		if ev.Kind == kiro.EvThink || ev.Kind == kiro.EvToolStart {
			text, stopped := flushText(true)
			if deliver(text, false, sink.Text) {
				limited = true
				return finish(turn.StopMaxTokens, kiro.Usage{})
			}
			if stopped {
				limited = true
				return finish(turn.StopSequence, kiro.Usage{})
			}
		}
		switch ev.Kind {
		case kiro.EvText:
			pending += ev.Text
			text, stopped := flushText(false)
			if deliver(text, false, sink.Text) {
				limited = true
				return finish(turn.StopMaxTokens, kiro.Usage{})
			}
			if stopped {
				limited = true
				return finish(turn.StopSequence, kiro.Usage{})
			}
		case kiro.EvThink:
			if deliver(ev.Text, true, sink.Thinking) {
				limited = true
				return finish(turn.StopMaxTokens, kiro.Usage{})
			}
		case kiro.EvSig:
			sink.Signature(ev.Text)
		case kiro.EvToolStart:
			if po.maxTokens > 0 && meter.Count(out.String()+ev.Name)+meter.Count(think.String())+(tools+1)*toolCallTokens > po.maxTokens {
				limited = true
				return finish(turn.StopMaxTokens, kiro.Usage{})
			}
			tools++
			out.WriteString(ev.Name)
			sink.ToolStart(ev.ID, ev.Name)
		case kiro.EvToolArgs:
			if deliver(ev.Text, false, sink.ToolArgs) {
				limited = true
				return finish(turn.StopMaxTokens, kiro.Usage{})
			}
		case kiro.EvError:
			return fail(ev.Text)
		case kiro.EvStop:
			text, stopped := flushText(true)
			if deliver(text, false, sink.Text) {
				limited = true
				return finish(turn.StopMaxTokens, kiro.Usage{})
			}
			if stopped {
				limited = true
				return finish(turn.StopSequence, kiro.Usage{})
			}
			if po.mode != kiro.ReportedIgnore && po.maxTokens > 0 && ev.Usage.Output > po.maxTokens {
				limited = true
			}
			return finish(ev.Stop, ev.Usage)
		}
	}
}

// anthropicStream 是 Anthropic Messages SSE 的 Sink。
type anthropicStream struct {
	w         http.ResponseWriter
	rc        *http.ResponseController
	id, model string
	index     int
	open      string
	initial   turn.Usage
}

func newAnthropicStream(w http.ResponseWriter, id, model string) turn.Sink {
	return &anthropicStream{w: w, rc: http.NewResponseController(w), id: id, model: model, index: -1}
}

func (a *anthropicStream) send(typ string, data map[string]any) {
	data["type"] = typ
	b, _ := json.Marshal(data)
	fmt.Fprintf(a.w, "event: %s\ndata: %s\n\n", typ, b)
	_ = a.rc.Flush()
}

func (a *anthropicStream) closeBlock() {
	if a.open != "" {
		a.send("content_block_stop", map[string]any{"index": a.index})
		a.open = ""
	}
}

func (a *anthropicStream) begin(kind string, block map[string]any) {
	a.closeBlock()
	a.index++
	a.open = kind
	a.send("content_block_start", map[string]any{"index": a.index, "content_block": block})
}

func (a *anthropicStream) delta(d map[string]any) {
	a.send("content_block_delta", map[string]any{"index": a.index, "delta": d})
}

func (a *anthropicStream) Begin(u turn.Usage) {
	a.initial = u
	h := a.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	a.w.WriteHeader(http.StatusOK)
	a.send("message_start", map[string]any{"message": map[string]any{
		"id": a.id, "type": "message", "role": "assistant", "model": a.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": anthropicUsage(u),
	}})
}

func (a *anthropicStream) Text(s string) {
	if a.open != "text" {
		a.begin("text", map[string]any{"type": "text", "text": ""})
	}
	a.delta(map[string]any{"type": "text_delta", "text": s})
}

func (a *anthropicStream) Thinking(s string) {
	if a.open != "thinking" {
		a.begin("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
	}
	a.delta(map[string]any{"type": "thinking_delta", "thinking": s})
}

func (a *anthropicStream) Signature(s string) {
	if a.open == "thinking" {
		a.delta(map[string]any{"type": "signature_delta", "signature": s})
	}
}

func (a *anthropicStream) ToolStart(id, name string) {
	a.begin("tool", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
}

func (a *anthropicStream) ToolArgs(s string) {
	if a.open == "tool" {
		a.delta(map[string]any{"type": "input_json_delta", "partial_json": s})
	}
}

func (a *anthropicStream) Ping() { a.send("ping", map[string]any{}) }

func (a *anthropicStream) End(stop string, u turn.Usage) {
	a.closeBlock()
	actual := anthropicUsage(u)
	u.Input = max(u.Input, a.initial.Input)
	u.CacheRead = max(u.CacheRead, a.initial.CacheRead)
	u.CacheWrite = max(u.CacheWrite, a.initial.CacheWrite)
	u.CacheWrite1h = min(u.CacheWrite, max(u.CacheWrite1h, a.initial.CacheWrite1h))
	u.KiroInput = max(u.KiroInput, a.initial.KiroInput)
	a.send("message_delta", map[string]any{
		"delta":         map[string]any{"stop_reason": stop, "stop_sequence": stopSequence(u)},
		"usage":         anthropicUsage(u),
		"settled_usage": actual,
	})
	a.send("message_stop", map[string]any{})
}

func (a *anthropicStream) Fail(status int, message string) {
	a.closeBlock()
	a.send("error", map[string]any{"error": map[string]any{"type": anthropic.ErrorType(status), "message": message}})
}

// anthropicMessage 是非流式 Messages 响应。
func anthropicMessage(t *turn.Turn) anthropic.Response {
	blocks := make([]anthropic.ResponseBlock, 0, len(t.Blocks))
	for _, b := range t.Blocks {
		switch b.Kind {
		case "text":
			blocks = append(blocks, anthropic.ResponseBlock{Type: "text", Text: b.Text})
		case "thinking":
			blocks = append(blocks, anthropic.ResponseBlock{Type: "thinking", Thinking: &b.Text, Signature: &b.Signature})
		case "tool_use":
			blocks = append(blocks, anthropic.ResponseBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
		}
	}
	return anthropic.Response{
		ID: t.ID, Type: "message", Role: "assistant", Model: t.Model, Content: blocks,
		StopReason: t.Stop, StopSequence: stopSequence(t.Usage), Usage: anthropicUsage(t.Usage),
	}
}

func anthropicUsage(u turn.Usage) anthropic.Usage {
	w5 := u.CacheWrite5m()
	return anthropic.Usage{
		InputTokens:              u.Input,
		OutputTokens:             u.Output,
		CacheReadInputTokens:     u.CacheRead,
		CacheCreationInputTokens: u.CacheWrite,
		CacheCreation:            &anthropic.CacheCreation{Ephemeral5m: w5, Ephemeral1h: u.CacheWrite - w5},
		Credits:                  u.Credits,
		HiddenTokens:             u.KiroInput,
		HiddenChargedUSD:         u.HiddenChargedUSD,
	}
}

func stopSequence(u turn.Usage) *string {
	if u.StopSequence == "" {
		return nil
	}
	return &u.StopSequence
}
