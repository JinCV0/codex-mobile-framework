package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 手机端调试：日志和截图从手机传回电脑，落在数据目录里，出问题时能直接看。
//
// 手机那边由原生直接发（HttpURLConnection），不经过网页脚本——网页自己坏了的时候
// 也得能把日志和截图送出来，那正是最需要它们的时刻。

const (
	phoneLogFile   = "手机调试.log"
	phoneShotDir   = "手机截图"
	phoneLogMaxLen = 4 << 20 // 日志文件超过 4MB 就重开一份
	phoneShotsKeep = 30      // 截图只留最近这么多张
)

// phoneCmd 是电脑这边排队、等手机来取的一条调试指令。
var phoneCmd struct {
	mu   sync.Mutex
	list []string
}

func enqueuePhoneCmd(cmd string) {
	phoneCmd.mu.Lock()
	phoneCmd.list = append(phoneCmd.list, cmd)
	phoneCmd.mu.Unlock()
}

func dequeuePhoneCmd() string {
	phoneCmd.mu.Lock()
	defer phoneCmd.mu.Unlock()
	if len(phoneCmd.list) == 0 {
		return ""
	}
	cmd := phoneCmd.list[0]
	phoneCmd.list = phoneCmd.list[1:]
	return cmd
}

// phoneDebug 接收手机传来的日志正文或截图。
func (s *Server) phoneDebug(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string `json:"kind"` // log / shot
		Name string `json:"name"` // 截图时的说明
		Data string `json:"data"` // shot: base64 PNG；log: 直接是文本
		Note string `json:"note"` // 附加信息（机型、版本……）
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 24<<20))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读不到请求体"})
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式不对"})
		return
	}

	switch body.Kind {
	case "log":
		path, err := s.savePhoneLog(body.Data, body.Note)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": path})
	case "shot":
		path, err := s.savePhoneShot(body.Name, body.Data, body.Note)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": path})
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind 只能是 log 或 shot"})
	}
}

func (s *Server) savePhoneLog(text, note string) (string, error) {
	if s.DataDir == "" {
		return "", fmt.Errorf("没有数据目录")
	}
	path := filepath.Join(s.DataDir, phoneLogFile)
	if info, err := os.Stat(path); err == nil && info.Size() > phoneLogMaxLen {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	header := fmt.Sprintf("\n===== 手机端日志 %s ===== %s\n", time.Now().Format("2006-01-02 15:04:05"), note)
	if _, err := f.WriteString(header); err != nil {
		return "", err
	}
	if _, err := f.WriteString(strings.TrimRight(text, "\n") + "\n"); err != nil {
		return "", err
	}
	return path, nil
}

func (s *Server) savePhoneShot(name, dataB64, note string) (string, error) {
	if s.DataDir == "" {
		return "", fmt.Errorf("没有数据目录")
	}
	raw := strings.TrimSpace(dataB64)
	if i := strings.Index(raw, ","); i >= 0 && strings.HasPrefix(raw, "data:") {
		raw = raw[i+1:] // 容忍 data:image/png;base64,xxx 这种写法
	}
	png, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(png) == 0 {
		return "", fmt.Errorf("截图数据不对")
	}
	dir := filepath.Join(s.DataDir, phoneShotDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	label := safeName(name)
	if label == "" {
		label = "手机"
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+label+".png")
	if err := os.WriteFile(path, png, 0o600); err != nil {
		return "", err
	}
	if note != "" {
		_ = os.WriteFile(strings.TrimSuffix(path, ".png")+".txt", []byte(note), 0o600)
	}
	prunePhoneShots(dir)
	return path, nil
}

func safeName(s string) string {
	s = strings.TrimSpace(s)
	// **后缀必须留着**：程序靠它判断"这是不是图片"（.jpg/.png…）。
	// 2026-10-05 踩过：这里把点过滤掉了，图片存成 48279jpg，
	// 于是被当成普通文件发给模型，用户传的图谁也没看见。
	ext := filepath.Ext(s)
	if len(ext) > 8 {
		ext = ""
	}
	stem := strings.TrimSuffix(s, filepath.Ext(s))
	var b strings.Builder
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r > 127:
			b.WriteRune(r) // 中文留着，文件名本来就没问题
		}
	}
	out := b.String() + ext
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func prunePhoneShots(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= phoneShotsKeep {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	cut := len(names) - phoneShotsKeep
	if cut <= 0 {
		return
	}
	for _, n := range names[:cut] {
		_ = os.Remove(filepath.Join(dir, n))
		_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(n, ".png")+".txt"))
	}
}

// phoneDebugList 列出手机上送过来的东西，界面上可以直接看。
func (s *Server) phoneDebugList(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		At   string `json:"at"`
	}
	out := []item{}
	if s.DataDir == "" {
		s.writeJSON(w, http.StatusOK, map[string]any{"items": out})
		return
	}
	logPath := filepath.Join(s.DataDir, phoneLogFile)
	if info, err := os.Stat(logPath); err == nil {
		out = append(out, item{Name: phoneLogFile, Size: info.Size(), At: info.ModTime().Format("01-02 15:04")})
	}
	dir := filepath.Join(s.DataDir, phoneShotDir)
	if entries, err := os.ReadDir(dir); err == nil {
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
				names = append(names, e.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		for _, n := range names {
			if info, err := os.Stat(filepath.Join(dir, n)); err == nil {
				out = append(out, item{Name: n, Size: info.Size(), At: info.ModTime().Format("01-02 15:04")})
			}
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// phoneCommand 是"电脑让手机做件事"的入口：GET 取一条（取走即清），
// POST 排队一条。截图、传日志都走它——人坐在电脑前也能让手机拍一张。
func (s *Server) phoneCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			Cmd    string `json:"cmd"`
			Action string `json:"action"`
			Sel    string `json:"sel"`
			Text   string `json:"text"`
			Name   string `json:"name"`
		}
		s.readJSON(r, &body)
		switch body.Cmd {
		case "shot", "log", "clear":
			enqueuePhoneCmd(body.Cmd)
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": body.Cmd})
		case "do":
			// 让手机执行一个页面动作（点按钮、开抽屉、切标签…）。
			// 电脑这边就能完全模拟用户操作来验证界面。
			payload, _ := json.Marshal(map[string]any{
				"action": body.Action, "sel": body.Sel, "text": body.Text, "name": body.Name,
			})
			enqueuePhoneCmd(string(payload))
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queued": "do", "action": body.Action})
		default:
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cmd 只能是 shot / log / clear / do"})
		}
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"cmd": dequeuePhoneCmd()})
}
