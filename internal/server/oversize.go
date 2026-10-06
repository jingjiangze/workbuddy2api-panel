// oversize.go 上下文超限请求的入站 fail-fast（学习式闸门，进程内状态、重启清零）。
//
// 背景（2026-10-06 线上实测）：长会话把 prompt 顶到 1.15–1.19M tokens 之后，上游对
// 这种请求一律回 400 11115「prompt is too long」。网关每次都真打上游、中位 7.3s
// 才拿到这句拒绝（一天 134 次），客户端在这 7s 里只能等，ZCode 侧于是先掐流、
// 把一次必然失败的 turn 报成 network_error 并按 11 次上限重试放大。
//
// 判定只学不猜：某模型回过一次 11115，就从那句原文里拿到 (实际 token 数, 上限)，
// 加上我们这边的 body 字节数，得到该次请求的 token 密度；TTL 内对同模型、
// **body 不小于已被拒规模、且按该密度估算仍超上限**的请求就地拒绝。
//
// 残余风险（刻意接受的）：估算假设新请求与证据请求的 token 密度相同，若客户端
// 换了内容构成（中文↔代码），同样字节数的真实 token 数可能差 20–30%，理论上会
// 误挡一个本可成功的请求。两条护栏：① 必须 bodyBytes ≥ 已被拒规模（更小的请求
// 永不挡）；② TTL 10min（压缩上下文/换会话后最迟 10 分钟自动放行）。相比之下
// 收益是同一天 134 次 × 7.3s 的上游往返与客户端等待。
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
// 上下文/换会话通常在分钟级，窗太长会继续挡掉已经合法的请求，窗太短省不下上游调用。
var oversizeGateTTL = 10 * time.Minute

// promptTooLongRe 上游 11115 原文里的真实数字：
// "prompt is too long: 1187903 tokens > 1048576 maximum"。
var promptTooLongRe = regexp.MustCompile(`prompt is too long:\s*([0-9]+)\s*tokens\s*>\s*([0-9]+)\s*maximum`)

// parsePromptTooLong 取上游声明的 (实际 token 数, token 上限)；文案变体解析不出 → ok=false
// （没数字就没法估密度，闸门宁可不学）。
func parsePromptTooLong(body string) (tokens, limit int, ok bool) {
	m := promptTooLongRe.FindStringSubmatch(body)
	if len(m) < 3 {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(m[1])
	b, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || a <= 0 || b <= 0 || a <= b {
		return 0, 0, false
	}
	return a, b, true
}

// oversizeEntry 某模型最近一次真实超限拒绝的证据。
type oversizeEntry struct {
	rejectedBytes  int // 被拒请求在本网关收到的 body 字节数
	rejectedTokens int // 上游原文里的实际 token 数
	limit          int // 上游原文里的 token 上限
	until          time.Time
}

// oversizeGate 按裸模型名（bareModel，realm 前缀已剥）记录证据。零值可用。
type oversizeGate struct {
	mu    sync.Mutex
	state map[string]*oversizeEntry
}

// note 记一次上游 11115。证据始终被最新一次覆盖（每次都是真实拒绝，最新者反映
// 客户端当前的内容构成），并续期 TTL。
func (g *oversizeGate) note(model string, bodyBytes, tokens, limit int) {
	if model == "" || bodyBytes <= 0 || tokens <= 0 || limit <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == nil {
		g.state = map[string]*oversizeEntry{}
	}
	g.state[model] = &oversizeEntry{
		rejectedBytes:  bodyBytes,
		rejectedTokens: tokens,
		limit:          limit,
		until:          time.Now().Add(oversizeGateTTL),
	}
}

// blocked 判定 bodyBytes 是否该本地拒绝，并返回估算 token 数与上游上限。
// 两条与条件：不小于已被拒规模（更小的请求永不挡）+ 按证据密度估算仍超上限。
func (g *oversizeGate) blocked(model string, bodyBytes int) (estTokens, limit int, hit bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.state[model]
	if !ok {
		return 0, 0, false
	}
	if time.Now().After(e.until) {
		delete(g.state, model)
		return 0, 0, false
	}
	if bodyBytes < e.rejectedBytes {
		return 0, e.limit, false
	}
	est := bodyBytes * e.rejectedTokens / e.rejectedBytes
	if est <= e.limit {
		return est, e.limit, false
	}
	return est, e.limit, true
}

// rejectOversize 本地拒绝（不经过上游）：code 与 message 口径对齐上游 11115
// （含真实数字），并在 hint 里说明这次是网关按先前证据挡下的。
func (h *Handler) rejectOversize(w http.ResponseWriter, st *chatStat, model string, estTokens, limit int) {
	msg := "prompt is too long: " + strconv.Itoa(estTokens) + " tokens > " + strconv.Itoa(limit) + " maximum"
	log.Printf("WARN: [server] oversize gate: model=%s rejected locally (estimated %d tokens > %d limit)", model, estTokens, limit)
	writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", msg,
		"gateway rejected this request without calling upstream: an equal-or-larger body for the same model was already rejected as prompt too long; compress the conversation and retry")
	st.status = http.StatusBadRequest
	st.outcome = reqlog.OutcomeHTTPError
}
