package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowReader 每次 Read 前固定睡眠：模拟「上游 200 已开流、首帧要憋很久」。
type slowReader struct {
	parts []string
	i     int
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.i >= len(s.parts) {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	n := copy(p, s.parts[s.i])
	s.i++
	return n, nil
}

// 心跳开着时，上游首帧之前就必须有字节写给客户端（否则 ai-sdk 一类客户端把
// "只有响应头、零字节"判成死连接 → ECONNRESET → 重试风暴）；数据帧一字不改。
func TestStreamHintPrimesAndPingsBeforeFirstFrame(t *testing.T) {
	const frame = `{"id":"chatcmpl-1","choices":[{"delta":{"content":"hi"}}]}`
	rec := httptest.NewRecorder()
	r := &slowReader{parts: []string{"data: " + frame + "\n\n", "data: [DONE]\n\n"}, delay: 90 * time.Millisecond}

	if err := StreamHint(rec, r, nil, WithStreamPing(15*time.Millisecond)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, ": ") {
		t.Fatalf("first bytes must be an SSE comment (priming), got %q", body)
	}
	dataAt := strings.Index(body, "data: "+frame)
	if dataAt < 0 {
		t.Fatalf("data frame missing/unmodified:\n%s", body)
	}
	if pings := strings.Count(body[:dataAt], "\n: ping\n\n"); pings == 0 {
		t.Fatalf("no ping before first data frame:\n%s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream must still end with [DONE]:\n%s", body)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count=%d want 1:\n%s", n, body)
	}
}

// 不传 WithStreamPing 时零扰动：既有调用方与测试看到的字节完全不变。
func TestStreamHintNoPingByDefault(t *testing.T) {
	rec := httptest.NewRecorder()
	r := strings.NewReader("data: {\"x\":1}\n\ndata: [DONE]\n\n")
	if err := StreamHint(rec, r, nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if got, want := rec.Body.String(), "data: {\"x\":1}\n\ndata: [DONE]\n\n"; got != want {
		t.Fatalf("default bytes changed:\n got %q\nwant %q", got, want)
	}
}
