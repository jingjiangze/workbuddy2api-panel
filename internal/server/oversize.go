// oversize.go 上下文超限请求的入站 fail-fast（学习式闸门，进程内状态、重启清零）。
//
// 背景（2026-10-06 线上实测）：长会话把 prompt 顶到 1.15–1.19M tokens 之后，上游对
// 这种请求一律回 400 11115「prompt is too long」。网关每次都真打上游、中位 7.3s
// 才拿到这句拒绝（一天 134 次），而这 7s 里流式客户端只有响应头、零字节，
// ai-sdk 一类客户端把它判成 ECONNRESET（retryable）并按 11 次上限重试，
// 于是一次必然失败的 turn 被放大成十几个请求，用户看到的还是"网络错误"。
//
// 判定只学不猜：某模型回过一次 11115，就记下「该模型下这个 body 规模必失败」，
// TTL 内对同模型、body 不小于该规模的请求就地拒绝。被拦下的每个请求背后都有一次
// 真实的上游拒绝作证据，所以不存在误杀；客户端压缩上下文或换会话后，最迟 TTL 到期
// 自动放行（且任何一次真实成功都会顺带把条目留在原地，不再影响更小的请求）。
//
// 归属层与 wafIPGate 同理：这是「请求规模」状态，跨账号、不属于任何账号，
// 放 server 局部（pool 是账号级记账层）。
package server

import (
	"log"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// oversizeGateTTL 学习窗口：到期解除（var 供测试注入短窗）。取 10min —— 客户端压缩
// 上下文/换会话通常在分钟级，窗太长会继续挡掉已经合法的请求，窗太短则省不下上游调用。
var oversizeGateTTL = 10 * time.Minute

// promptTooLongRe 上游 11115 原文里的真实数字：
// "prompt is too long: 1187903 tokens > 1048576 maximum"。
var promptTooLongRe = regexp.MustCompile(`prompt is too long:\s*[0-9]+\s*tokens\s*>\s*([0-9]+)\s*maximum`)

// promptTooLongLimit 取上游声明的 token 上限；解析不出（文案变体）→ 0 = 未知。
func promptTooLongLimit(body string) int {
	m := promptTooLongRe.FindStringSubmatch(body)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// oversizeEntry 某模型的已知必拒规模下界 + 上游声明的 token 上限。
type oversizeEntry struct {
	minBytes  int
	maxTokens int
	until     time.Time
}

// oversizeGate 按裸模型名（bareModel，realm 前缀已剥）记录已知必拒规模。零值可用。
type oversizeGate struct {
	mu    sync.Mutex
	state map[string]*oversizeEntry
}

// note 记一次上游超限拒绝（bodyBytes = 被拒请求的 body 字节数）。
// 只收紧不放宽：新证据更小才下调 minBytes（更大的证据已被现有条目覆盖，
// 上调只会放走已知必失败的请求）。limit 为 0（未解析出）时保留已知的上限值。
func (g *oversizeGate) note(model string, bodyBytes, limit int) {
	if model == "" || bodyBytes <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		g.state = map[string]*oversizeEntry{}
	}
	e, ok := g.state[model]
	if !ok {
		g.state[model] = &oversizeEntry{minBytes: bodyBytes, maxTokens: limit, until: time.Now().Add(oversizeGateTTL)}
		return
	}
	if bodyBytes < e.minBytes {
		e.minBytes = bodyBytes
	}
	if limit > 0 {
		e.maxTokens = limit
	}
	e.until = time.Now().Add(oversizeGateTTL)
}

// blocked 判定 bodyBytes 是否落在该模型已知必拒区。
// 返回 (上游声明的 token 上限, 是否拦)；上限 0 表示只知"这个规模会被拒"。
func (g *oversizeGate) blocked(model string, bodyBytes int) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.state[model]
	if !ok {
		return 0, false
	}
	if time.Now().After(e.until) {
		delete(g.state, model)
		return 0, false
	}
	if bodyBytes < e.minBytes {
		return 0, false
	}
	return e.maxTokens, true
}

// rejectOversize 本地拒绝（不经过上游）：沿用上游同款 code（prompt_too_long，
// 400 非重试语义）与"含真实数字"的 message 口径，并在 hint 里说清这次是网关
// 依据先前证据挡下的、上游声明的上限是多少。
func (h *Handler) rejectOversize(w http.ResponseWriter, st *chatStat, model string, bodyBytes, limit int) {
	msg := "prompt is too long: model " + model + " already rejected an equal-or-larger request body"
	if limit > 0 {
		msg += " as exceeding its context window (" + strconv.Itoa(limit) + " tokens maximum)"
	}
	msg += "; compress the conversation (or start a new one) and retry"
	log.Printf("WARN: [server] oversize gate: model=%s body=%d bytes rejected locally (learned limit=%d tokens)", model, bodyBytes, limit)
	writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", msg,
		"gateway rejected this request without calling upstream: an equal-or-larger body for the same model was already rejected as prompt too long")
	st.status = http.StatusBadRequest
	st.outcome = reqlog.OutcomeHTTPError
}
