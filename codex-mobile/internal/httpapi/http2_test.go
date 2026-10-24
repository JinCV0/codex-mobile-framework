package httpapi

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexmobile/internal/codex"
	"codexmobile/internal/store"
)

// 手机端在 ALPN 打开之后走的就是 HTTP/2。这条链子必须和 1.1 一样能干活：
// gzip 要照压，SSE 要能立刻把响应头顶出去（说明 gzip 包装层没把 Flush 吞掉）。
// 它坏了等于整个界面打不开，所以在本地就把它钉住。
func TestHTTP2GzipAndStream(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db, Token: "tk", Runner: &codex.Runner{}}

	srv := httptest.NewUnstartedServer(s)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client := srv.Client()

	// 显式带上 Accept-Encoding：不然 Go 的客户端会自己加、再自己解开，
	// 这里就看不到服务端到底压没压
	req0, err := http.NewRequest(http.MethodGet, srv.URL+"/api/status?k=tk", nil)
	if err != nil {
		t.Fatal(err)
	}
	req0.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req0)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("请求没走 HTTP/2，实际是 %s", resp.Proto)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("HTTP/2 下 gzip 没生效：Content-Encoding=%q", got)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "busy") {
		got := string(body)
		if len(got) > 80 {
			got = got[:80]
		}
		t.Fatalf("解压出来的内容不对：%s", got)
	}

	// SSE：只要 Flush 能透传，客户端就能立刻拿到响应头
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events?k=tk", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	stream, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.ProtoMajor != 2 {
		t.Fatalf("SSE 没走 HTTP/2，实际是 %s", stream.Proto)
	}
	if ct := stream.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE 的 Content-Type 不对：%q", ct)
	}
	if enc := stream.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("SSE 不该被压缩，却带了 Content-Encoding=%q", enc)
	}
}
