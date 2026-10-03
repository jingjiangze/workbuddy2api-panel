package server

import (
	"io"
	"net/http"
	"time"
)

// readBodyIdle 按进度续期地读取请求体：连续 idle 时间内收不到任何字节才失败
// （返回的错误满足 net.Error.Timeout()==true），只要还在持续收到字节就一直等。
//
// 为什么不靠 http.Server.ReadTimeout：那是「整包」计时，慢链路上传大上下文时
// 「仍在推进、只是没传完」一样被判超时；而且它掐在 handler 之外，调用方拿不到
// 「这是超时还是客户端发了坏请求」的区分，只能统一回不可重试的 400。空闲计时
// 挪到这里之后，调用方可以据此返回 408，让 OpenAI 兼容客户端自动重试。
//
// w 不支持设置连接 deadline 时（httptest.ResponseRecorder 等）退化为普通整包读取，
// 语义与改动前完全一致。
func readBodyIdle(w http.ResponseWriter, r *http.Request, idle time.Duration) ([]byte, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(idle)); err != nil {
		return io.ReadAll(r.Body)
	}
	var body []byte
	buf := make([]byte, 64*1024)
	for {
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if rerr != nil {
			_ = rc.SetReadDeadline(time.Time{})
			if rerr == io.EOF {
				return body, nil
			}
			return body, rerr
		}
		// 有进展：把截止推后一个 idle 窗口。
		_ = rc.SetReadDeadline(time.Now().Add(idle))
	}
}
