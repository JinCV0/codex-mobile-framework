package httpapi

import (
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
)

// 界面外壳（index.html，195 KB）以前是 no-store：手机每次冷启动都要原样搬
// 一遍，走公网 Funnel 实测 2.3 秒。改成 no-cache + ETag 以后，内容没变就回一个
// 304 —— 一样永远拿到最新的界面，但只花一个来回，不用再传 195 KB。
//
// 不用 max-age 是故意的：以前缓存住旧外壳，"服务端换了界面手机却一直看旧的"
// 那个坑就是这么来的。no-cache 的含义是"每次都要问，但问了没变就别重传"。

// etagOf 用内容算一个稳定的校验值。同一份内容永远同一个值，
// 内容一变值就变，不需要额外维护版本号。
func etagOf(b []byte) string {
	sum := fnv.New64a()
	_, _ = sum.Write(b)
	return `"` + strconv.FormatUint(sum.Sum64(), 16) + `"`
}

// etagMatches 看客户端带回来的 If-None-Match 里有没有我们这一版。
func etagMatches(r *http.Request, etag string) bool {
	header := r.Header.Get("If-None-Match")
	if header == "" {
		return false
	}
	for _, want := range strings.Split(header, ",") {
		want = strings.TrimSpace(want)
		if want == "*" {
			return true
		}
		if strings.TrimPrefix(want, "W/") == etag {
			return true
		}
	}
	return false
}
