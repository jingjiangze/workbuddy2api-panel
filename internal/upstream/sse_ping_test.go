package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const pingTestFrame = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`

const pingTestInput = pingTestFrame + "\n\ndata: [DONE]\n\n"

// pingSlowReader 每次 Read 前固定睡眠：模拟「上游 200 已开流、首帧要憋很久」。
type pingSlowReader struct {
	parts []string
	i     int
	delay time.Duration
}

func (s *pingSlowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.parts) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	n := copy(p, s.parts[s.i])
	s.i++
	return n, nil
}

// pingStripComments 去掉网关自己补的注释帧（priming + 心跳），只看真正的 SSE 事件字节。
func pingStripComments(s string) string {
	s = strings.ReplaceAll(s, ": wb2api\n\n", "")
	return strings.ReplaceAll(s, ": ping\n\n", "")
}

// 心跳只加注释帧：剥掉注释之后，透传字节必须与不开心跳**逐字节一致**
// （normalizeFrame 会重写帧内容，所以这里比的是"两条路径一致"，不是"与输入一致"）。
func TestStreamHintPingKeepsPayloadByteIdentical(t *testing.T) {
	off := httptest.NewRecorder()
	if err := StreamHint(off, strings.NewReader(pingTestInput), nil); err != nil {
		t.Fatalf("StreamHint(no ping): %v", err)
	}
	on := httptest.NewRecorder()
	if err := StreamHint(on, strings.NewReader(pingTestInput), nil, WithStreamPing(10*time.Millisecond)); err != nil {
		t.Fatalf("StreamHint(ping): %v", err)
	}
	if a, b := off.Body.String(), pingStripComments(on.Body.String()); a != b {
		t.Fatalf("ping altered payload bytes:\n off=%q\n on =%q", a, b)
	}
	if !strings.Contains(off.Body.String(), "data: [DONE]") {
		t.Fatalf("baseline output unexpected:\n%q", off.Body.String())
	}
}

// 上游憋着不出首帧时，客户端也要立刻有字节可收：先 priming 注释帧，心跳在
// 第一个 data: 帧之前就得出现。
func TestStreamHintPingsBeforeFirstDataFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	r := &pingSlowReader{
		parts: []string{pingTestFrame + "\n\n", "data: [DONE]\n\n"},
		delay: 120 * time.Millisecond,
	}
	if err := StreamHint(rec, r, nil, WithStreamPing(10*time.Millisecond)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, ": wb2api\n\n") {
		t.Fatalf("first bytes must be the priming comment:\n%q", body)
	}
	dataAt := strings.Index(body, "data: ")
	if dataAt < 0 {
		t.Fatalf("no data frame at all:\n%q", body)
	}
	if pings := strings.Count(body[:dataAt], ": ping\n\n"); pings == 0 {
		t.Fatalf("no ping before first data frame:\n%q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream must end with [DONE]:\n%q", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count=%d want 1:\n%q", n, body)
	}
}

// 不传 WithStreamPing 时零扰动：默认不写任何注释帧（既有调用方字节不变）。
func TestStreamHintNoPingByDefault(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(pingTestInput), nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if got := rec.Body.String(); strings.Contains(got, ": wb2api") || strings.Contains(got, ": ping") {
		t.Fatalf("unexpected comment frames without WithStreamPing:\n%q", got)
	}
}
