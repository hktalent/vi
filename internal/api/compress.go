package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
)

// 这一层做两件对「Web 端响应速度」最直接有效的事：
//  1. gzip 压缩文本响应（/api/* 的 JSON、html/js/css），标签列表 20KB → 约 1.5KB；
//  2. 给 GET 响应加弱 ETag，浏览器重复请求命中 304，连重新序列化的传输都省掉。
//
// 只缓冲文本类路径，图片/模型/wasm 直接透传，不占内存。

func sumBytes(b []byte) [sha1.Size]byte { return sha1.Sum(b) }

var gzPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// bufResponseWriter 把响应先攒在内存里，便于决定是否压缩/是否 304。
type bufResponseWriter struct {
	hdr    http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufResponseWriter) Header() http.Header { return b.hdr }

func (b *bufResponseWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (b *bufResponseWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}

var compressibleExt = map[string]bool{
	".html": true, ".htm": true, ".js": true, ".mjs": true, ".css": true,
	".json": true, ".md": true, ".txt": true, ".svg": true, ".xml": true,
}

// compressible 判断该路径是否需要 gzip/ETag 处理。
func compressible(p string) bool {
	if strings.HasPrefix(p, "/api/") {
		return true
	}
	return compressibleExt[strings.ToLower(path.Ext(p))]
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// withCompression 是文本响应的 gzip + ETag 中间件。
func withCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !compressible(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Accept-Encoding")

		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}

		bw := &bufResponseWriter{hdr: h}
		next.ServeHTTP(bw, r)

		body := bw.buf.Bytes()
		st := bw.status
		if st == 0 {
			st = http.StatusOK
		}

		// ETag：仅对成功 GET 且体积不大（API/静态文本都在这个范围）的响应
		if r.Method == http.MethodGet && st == http.StatusOK && len(body) > 0 && len(body) < 8<<20 {
			etag := fmt.Sprintf(`W/"%x"`, sumBytes(body))
			h.Set("ETag", etag)
			if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
				h.Del("Content-Length")
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		// 仅对完整 200 响应压缩：206（Range 分段）压缩会破坏 Content-Range 语义
		if st == http.StatusOK && len(body) >= 1024 {
			gz := gzPool.Get().(*gzip.Writer)
			defer gzPool.Put(gz)
			gz.Reset(w)
			h.Set("Content-Encoding", "gzip")
			h.Del("Content-Length")
			w.WriteHeader(st)
			_, _ = gz.Write(body)
			_ = gz.Close()
			return
		}

		w.WriteHeader(st)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	})
}
