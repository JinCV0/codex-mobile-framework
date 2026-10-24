package httpapi

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	// 解码器按需注册：webp 是手机端最常见的图源（pic.re 之类都发 webp），
	// jpeg / png / gif 走标准库。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// imageMIME 是"能当图片喂给手机"的白名单。
// 只看后缀不看内容 —— 这里拦的是顺手读个 .exe 的情况，真正的信任边界是整个
// /api/ 都要带访问口令。
var imageMIME = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// fsImageMaxBytes 是单张图的上限。整个文件要先读进内存再写出去，
// 而手机屏幕再大也用不着几十兆的原图。
const fsImageMaxBytes = 32 << 20

// thumbMaxWidth 是"不额外指定 w"时默认缩到多宽。
// 手机屏幕按 3 倍密度也就 1200 物理像素宽，再宽纯属浪费隧道带宽。
const thumbMaxWidth = 1200

// fsImage 把本机上的一张图片读出来发给手机。
//
// 为什么需要它：Codex 在回复里经常直接写图片的绝对路径（比如
// H:\Project\xxx\out\fig1.png），手机上那串字既点不开也看不懂。
// 页面认出"这是张图"之后，图本身还得电脑来喂 —— 手机碰不到这台机器的盘。
//
// 参数：
//
//	path  要读的文件（绝对路径）
//	w     缩到这么宽再发；不传就按 thumbMaxWidth 缩。w=0 表示"要原图"
//	      （手机端点开大图后按「查看原图」就是这个，用来放大看细节）。
//
// 为什么默认要缩：原来直接发原图，四张 2K 插画加起来 3.5MB，走公网隧道
// 手机滚动时一直在等（2026-10-11 用户反馈"传这个图变得很卡"）。
// 缩到 1200 宽通常只有几十 KB，肉眼在手机上看不出差别；真要抠细节再点
// 「查看原图」。
func (s *Server) fsImage(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if p == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 path"})
		return
	}
	if !filepath.IsAbs(p) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "只接受绝对路径"})
		return
	}
	origMime, ok := imageMIME[strings.ToLower(filepath.Ext(p))]
	if !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不是支持的图片格式"})
		return
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
		return
	}
	if info.Size() > fsImageMaxBytes {
		s.writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "图片太大"})
		return
	}

	// 要原图就原样返回（也省掉一次解码）
	want := r.URL.Query().Get("w")
	if want == "0" {
		raw, err := os.ReadFile(p)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读不出来"})
			return
		}
		w.Header().Set("Content-Type", origMime)
		// 同一张图在列表里可能被画好几次，让手机自己缓存一会儿
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = w.Write(raw)
		return
	}

	targetW := thumbMaxWidth
	if want != "" {
		if n, err := strconv.Atoi(want); err == nil && n > 0 {
			targetW = n
		}
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读不出来"})
		return
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		// 解不了（罕见的编码）就退回原图，总比给个破图强
		w.Header().Set("Content-Type", origMime)
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = w.Write(raw)
		return
	}

	scaled := fitWidth(src, targetW)
	w.Header().Set("Cache-Control", "private, max-age=300")
	if strings.ToLower(filepath.Ext(p)) == ".png" {
		// PNG 留着，可能有透明背景
		w.Header().Set("Content-Type", "image/png")
		if err := png.Encode(w, scaled); err != nil {
			_, _ = w.Write(raw)
		}
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	if err := jpeg.Encode(w, scaled, &jpeg.Options{Quality: 82}); err != nil {
		_, _ = w.Write(raw)
	}
}

// fitWidth 把图等比缩到不超过 targetW 宽。本来就比它小就不动（不放大，
// 放大只会糊还更占带宽）。
func fitWidth(src image.Image, targetW int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if targetW <= 0 || sw <= 0 || sh <= 0 || sw <= targetW {
		return src
	}
	nh := sh * targetW / sw
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetW, nh))
	// CatmullRom 比默认的最近邻好太多，缩插画不会糊成一团；
	// 单张图的耗时也就几十毫秒，够用。
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}
