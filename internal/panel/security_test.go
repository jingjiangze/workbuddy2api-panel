package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPanel() *Panel {
	// 启用鉴权：未带 key 的请求一律 401，不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/app.js"},
		{"GET", "/panel/api/overview"}, // 401（未提供 key）
		{"POST", "/panel/api/config"},  // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
// 引用串允许带版本查询串（app.js?v=<version>）——那是长期边缘缓存的失效手段。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `<script src="app.js`) || !strings.Contains(body, `"></script>`) {
		t.Error("index.html must load app.js externally (inline script is blocked by CSP)")
	}
	if strings.Contains(body, appScriptTag) {
		t.Error(`versioned panel must point app.js at a versioned URL, got plain src="app.js"`)
	}
	if !strings.Contains(body, `app.js?v=test`) {
		t.Error("script src must carry the panel version")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// 版本串为空（裸用/测试）时不得伪造版本号：宁可不改写引用。
func TestIndexWithoutVersionKeepsPlainScriptTag(t *testing.T) {
	p := New(Config{APIKey: "test-key"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if !strings.Contains(rec.Body.String(), appScriptTag) {
		t.Error("without a version the page must keep the plain app.js reference")
	}
}

// 静态脚本走一年 immutable 边缘缓存的前提：URL 已按版本分叉 + 支持条件请求。
func TestAppScriptImmutableCaching(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control=%q want one-year immutable", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	if rec.Body.Len() == 0 {
		t.Fatal("first fetch must return the script body")
	}

	for _, match := range []string{etag, "*", "W/" + etag, `other, ` + etag} {
		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("GET", "/panel/app.js", nil)
		req2.Header.Set("If-None-Match", match)
		p.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: code=%d body=%d want 304/empty", match, rec2.Code, rec2.Body.Len())
		}
	}
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/panel/app.js", nil)
	req3.Header.Set("If-None-Match", `"stale"`)
	p.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK || rec3.Body.Len() == 0 {
		t.Errorf("stale ETag must serve the body again: code=%d bytes=%d", rec3.Code, rec3.Body.Len())
	}
}

// 没有版本串就不能长期缓存（否则升级后前端永远拿不回新的）。
func TestAppScriptNoVersionIsNotCached(t *testing.T) {
	p := New(Config{APIKey: "test-key"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control=%q want no-cache without a version", cc)
	}
}

// 页面必须每次校验（里面嵌着版本串），但校验要能用 304 省掉整页传输。
func TestIndexRevalidatesWithEtag(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index Cache-Control=%q want no-cache", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("index must publish an ETag for revalidation")
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/", nil)
	req2.Header.Set("If-None-Match", etag)
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Errorf("revalidated index: code=%d bytes=%d want 304/empty", rec2.Code, rec2.Body.Len())
	}
}

func TestEtagMatches(t *testing.T) {
	etag := `"v1"`
	for _, h := range []string{`"v1"`, `*`, `W/"v1"`, `"a", "v1"`, `"v1", "b"`} {
		if !etagMatches(h, etag) {
			t.Errorf("etagMatches(%q) = false, want true", h)
		}
	}
	for _, h := range []string{``, `"v2"`, `W/"v2"`, `"a","b"`} {
		if etagMatches(h, etag) {
			t.Errorf("etagMatches(%q) = true, want false", h)
		}
	}
}


// app.js 必须能作为同源脚本取到且类型正确（否则页面白屏）。
func TestAppScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'use strict'") {
		t.Error("app.js body looks wrong")
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未带密钥的 API 请求必须 401；携带正确密钥则通过鉴权层（不再是 401）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: code=%d want 401", rec.Code)
	}
	// 用不存在的路由验证"带正确 key 已过鉴权"（避免触碰依赖 nil 的 handler）。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/nonexistent", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusUnauthorized {
		t.Error("valid key must pass the auth layer")
	}
}
