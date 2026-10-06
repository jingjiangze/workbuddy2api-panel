// index.go 面板静态资源与安全响应头。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//   - index.html  页面骨架
//   - app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
//
// 缓存策略（实测基线：源站原先不发任何 Cache-Control，CF 自己给 app.js 补了
// max-age=14400 并 brotli 到 50KB/次；浏览器则走启发式缓存，行为不可预测）：
//   - app.js 一年 immutable：URL 上挂版本号（见 indexHTMLVersioned），版本一变 URL 就变，
//     所以长期缓存不会把旧前端钉死；ETag 再兜一层条件请求。省下的是"每次开面板
//     都要重拉 50KB 压缩脚本"这一整跳。
//   - index.html no-cache + ETag：页面必须每次确认新鲜（里面嵌着版本串），但允许
//     用 304 省掉 72KB 传输——no-store 会把浏览器缓存也一起禁掉，反而更费。
//   - /panel/api/* 不加缓存头（JSON 不在边缘默认缓存类型里，且都是要鉴权的易变数据）。
package panel

import (
	"bytes"
	_ "embed"
	"net/http"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// csp 内容安全策略（严格版，无需 unsafe-inline）：
//   - default-src 'none'        默认全禁，逐个开口
//   - script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//   - style-src 'self' 'unsafe-inline'
//     style 的内联是设计取舍：页面有少量 style="..." 属性（进度条宽度、表格列宽），
//     允许内联样式不会导致脚本执行；仍禁止外部样式域与 @import 外链。
//   - connect-src 'self'        前端 fetch 只能打本服务
//   - img-src 'self' data:      图标/内联图
//   - form-action 'none'        页面无表单提交目标（配置页是 JS 提交）
//   - frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
//   - base-uri 'none'          禁止注入 <base> 改写相对路径
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// appScriptTag 与 index.html 里的引用串一字对应；改模板时同步改这里
// （TestIndexReferencesExternalScript 会把不一致直接测出来）。
const appScriptTag = `<script src="app.js"></script>`

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要，API 也含 JSON 数据）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // 禁 MIME 嗅探
	w.Header().Set("X-Frame-Options", "DENY")           // 老浏览器兜底（CSP frame-ancestors 的等价项）
	w.Header().Set("Referrer-Policy", "no-referrer")    // 不外泄面板地址给外部站点
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// indexHTMLVersioned 把 app.js 的引用改写成带版本号的 URL，让一年 immutable 安全可用。
// version 为空（裸用/测试未填）或模板里找不到引用串 → 原样返回，绝不因缓存改写弄白屏。
func indexHTMLVersioned(version string) []byte {
	if version == "" {
		return indexHTML
	}
	return bytes.Replace(indexHTML, []byte(appScriptTag),
		[]byte(`<script src="app.js?v=`+version+`"></script>`), 1)
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
// no-cache + ETag：每次必须回源校验（页面里嵌着版本串，不能拿旧的），但校验命中只回
// 304 头，72KB 的骨架不必再跨洋重传。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	body := indexHTMLVersioned(p.cfg.Version)
	if p.cfg.Version == "" {
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	etag := `"html-` + p.cfg.Version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Header.Get("If-None-Match") != "" && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// appScript 输出前端逻辑（同源脚本，供 CSP script-src 'self' 加载）。
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	if p.cfg.Version == "" {
		// 没有版本串就没法保证"升级后 URL 会变"，宁可不缓存也不留旧前端。
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(appJS)
		return
	}
	etag := `"` + p.cfg.Version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}

// etagMatches 按 RFC 9110 处理 If-None-Match：* 或逗号分隔列表里含本 ETag（W/ 前缀忽略）。
func etagMatches(header, etag string) bool {
	for _, cand := range strings.Split(header, ",") {
		cand = strings.TrimSpace(cand)
		if cand == "*" {
			return true
		}
		cand = strings.TrimPrefix(cand, "W/")
		if cand == etag {
			return true
		}
	}
	return false
}
