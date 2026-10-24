package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 文本响应要真的被压过，而且解出来必须和原文一模一样。
func TestGzipCompressesText(t *testing.T) {
	body := strings.Repeat("农业深度学习 ", 300)
	rec := httptest.NewRecorder()
	gw := newGzipWriter(rec)
	gw.Header().Set("Content-Type", "application/json; charset=utf-8")
	if _, err := io.WriteString(gw, body); err != nil {
		t.Fatal(err)
	}
	gw.close()

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q，期望 gzip", got)
	}
	if rec.Body.Len() >= len(body) {
		t.Fatalf("压完 %d 字节，原文 %d 字节，没有变小", rec.Body.Len(), len(body))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatal("解压出来的内容和原文不一致")
	}
}

// 图片和 SSE 都不能压：前者本来就压过，后者压了会攒在缓冲区里。
func TestGzipLeavesBinaryAndSSEAlone(t *testing.T) {
	cases := []struct {
		name string
		ct   string
	}{
		{"png", "image/png"},
		{"sse", "text/event-stream; charset=utf-8"},
		{"ndjson", "application/x-ndjson; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			gw := newGzipWriter(rec)
			gw.Header().Set("Content-Type", tc.ct)
			if _, err := io.WriteString(gw, strings.Repeat("x", 5000)); err != nil {
				t.Fatal(err)
			}
			gw.close()
			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("%s 不该被压，却有 Content-Encoding=%q", tc.ct, got)
			}
			if rec.Body.Len() != 5000 {
				t.Fatalf("没压缩时长度应该原样，得到 %d", rec.Body.Len())
			}
		})
	}
}

// SSE 那条路径必须绕开压缩通道：压了就不是"一条条顶出去"了。
func TestGzipWantedSkipsEvents(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	if gzipWanted(req) {
		t.Fatal("/api/events 不该进压缩通道")
	}
	req = httptest.NewRequest(http.MethodGet, "/api/threads", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	if !gzipWanted(req) {
		t.Fatal("/api/threads 应该进压缩通道")
	}
	req = httptest.NewRequest(http.MethodPost, "/api/send", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	if gzipWanted(req) {
		t.Fatal("POST 不该进压缩通道")
	}
}

// 同一份内容算出同一个 ETag，带回来的 If-None-Match 要认。
func TestETagRoundTrip(t *testing.T) {
	etag := etagOf([]byte("同一份界面"))
	if etag != etagOf([]byte("同一份界面")) {
		t.Fatal("同样的内容算出了不同的 ETag")
	}
	if etag == etagOf([]byte("换了一份界面")) {
		t.Fatal("内容变了 ETag 却没变")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if etagMatches(req, etag) {
		t.Fatal("没带 If-None-Match 却被判成命中")
	}
	req.Header.Set("If-None-Match", `W/`+etag+`, "other"`)
	if !etagMatches(req, etag) {
		t.Fatal("带回来的 ETag 没认出来")
	}
}
