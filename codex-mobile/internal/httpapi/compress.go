package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// 手机走的是公网 Funnel：一个来回 0.4 秒起步，有效带宽只有 160 KB/s 上下
// （2026-10-10 实测）。所以"少发一点"比"发得快一点"划算得多。
//
// 这里统一给文本类响应加 gzip：界面的 index.html 195 KB、对话列表 125 KB，
// 压完都只剩三分之一左右，手机端拿到内容的时间跟着变短。
//
// 三条不能碰的线：
//   - SSE（/api/events）要一条一条顶出去，压了会攒在缓冲区里，手机端看着像卡死
//   - 流式 ndjson（/api/send）同理，所以按 Content-Type 挡在门外
//   - 包装层必须把 Flush 透传下去，否则上面两条即使不压也会被吞掉

var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// compressibleType 只压文本。图片和 APK 本来就已经压过，再压只是白烧 CPU。
func compressibleType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	if strings.HasPrefix(ct, "text/") {
		// SSE 虽然也是 text/，但它要的是"立刻顶出去"，不是"压得小"
		return !strings.HasPrefix(ct, "text/event-stream")
	}
	switch {
	case strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "application/manifest+json"),
		strings.HasPrefix(ct, "application/xml"),
		strings.Contains(ct, "svg"):
		return true
	}
	return false
}

// gzipResponseWriter 看 Content-Type 决定压不压，并且把 Flush 原样透传。
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	compress    bool
	decided     bool
	wroteHeader bool
}

func newGzipWriter(w http.ResponseWriter) *gzipResponseWriter {
	return &gzipResponseWriter{ResponseWriter: w}
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if !w.decided {
		w.decide(code)
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

// decide 在头写出去之前定下来压不压。判断只看处理器自己设的 Content-Type，
// 这样二进制响应、已经压过的响应都会自动放过。
func (w *gzipResponseWriter) decide(code int) {
	w.decided = true
	// 304/204 没有响应体，声明 gzip 只会让客户端以为少收了东西
	if code == http.StatusNotModified || code == http.StatusNoContent {
		return
	}
	h := w.Header()
	if h.Get("Content-Encoding") != "" || !compressibleType(h.Get("Content-Type")) {
		return
	}
	w.compress = true
	h.Del("Content-Length") // 压完长度就变了，交回 net/http 自己算
	h.Add("Vary", "Accept-Encoding")
	h.Set("Content-Encoding", "gzip")
	w.gz = gzipPool.Get().(*gzip.Writer)
	w.gz.Reset(w.ResponseWriter)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.compress || w.gz == nil {
		return w.ResponseWriter.Write(p)
	}
	return w.gz.Write(p)
}

// Flush 必须透传：SSE 和 /api/send 的流式输出都靠它把内容顶到手机上。
func (w *gzipResponseWriter) Flush() {
	if w.compress && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// close 收尾：gzip 的尾巴必须写出去，否则客户端会认为这条响应被截断了。
func (w *gzipResponseWriter) close() {
	if w.compress && w.gz != nil {
		_ = w.gz.Close()
		gzipPool.Put(w.gz)
		w.gz = nil
	}
}

// gzipWanted 决定这条请求要不要进压缩通道。
func gzipWanted(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		return false
	}
	// SSE 单独放行，理由见文件头
	return !strings.HasPrefix(r.URL.Path, "/api/events")
}
