package main

import (
	"net/http"
	"strings"
)

// 终端页更新频繁，嵌入式浏览器可能长期复用旧模块；只对该入口及依赖禁用缓存。
func terminalStaticFiles(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		terminalAsset := path == "/cmux.html" || path == "/cmux.css" ||
			path == "/cmux.mjs" || strings.HasPrefix(path, "/cmux-") &&
			strings.HasSuffix(path, ".mjs") ||
			path == "/lib/xterm.min.js" || path == "/lib/xterm-addon-fit.min.js" ||
			path == "/lib/xterm.css" || path == "/lib/marked/marked.esm.js"
		if terminalAsset {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
			// 避免嵌入式浏览器持有旧响应时收到 304，强制取回当前模块内容。
			r = r.Clone(r.Context())
			r.Header.Del("If-Modified-Since")
			r.Header.Del("If-None-Match")
		}
		next.ServeHTTP(w, r)
	})
}
