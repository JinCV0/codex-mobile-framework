// Package notify 把"一轮跑完了"推送到手机。
// 四个渠道都在开发文档 §10.4 里列过，全部不依赖电脑的公网地址。
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codexmobile/internal/store"
)

// Target 是解析好的推送目标。
type Target struct {
	URL    string
	Format string
	Token  string
}

// Resolve 把界面上的配置翻译成实际的请求目标；没配置就返回 nil。
func Resolve(cfg store.Notify) *Target {
	channel := cfg.Channel
	value := strings.TrimSpace(cfg.Value)
	if value == "" {
		value = strings.TrimSpace(cfg.URL)
	}
	if channel == "" {
		if cfg.URL != "" {
			channel = "custom"
		} else {
			channel = "none"
		}
	}
	if value == "" || channel == "none" {
		return nil
	}
	switch channel {
	case "serverchan":
		return &Target{URL: "https://sctapi.ftqq.com/" + value + ".send", Format: "form"}
	case "pushplus":
		return &Target{URL: "https://www.pushplus.plus/send", Format: "pushplus", Token: value}
	case "wecom":
		return &Target{URL: value, Format: "wecom"}
	case "ntfy":
		return &Target{URL: value, Format: "text"}
	default:
		format := cfg.Format
		if format == "" {
			format = "json"
		}
		return &Target{URL: value, Format: format}
	}
}

// Send 发送一条通知，返回 HTTP 状态码；配置为空时返回 0。
func Send(cfg store.Notify, title, body string) (int, error) {
	if !cfg.OnDone {
		return 0, nil
	}
	target := Resolve(cfg)
	if target == nil {
		return 0, nil
	}
	return deliver(target, nil, title, body)
}

// SendApproval 推送"跑一半停下来等你拍板"，和完成通知互相独立。
// ntfy 通道会附动作按钮：通知栏里直接点"允许 / 拒绝"就 POST 回 callback
// （即 /api/approval 的公网地址），人不用打开手机；其余通道只发文字。
func SendApproval(cfg store.Notify, id, title, body, callback string) (int, error) {
	if !cfg.OnApproval {
		return 0, nil
	}
	target := Resolve(cfg)
	if target == nil {
		return 0, nil
	}
	headers := map[string]string{}
	if target.Format == "text" && callback != "" {
		// ntfy：优先级拉满，再挂两个动作按钮。body/headers 含逗号，必须加引号。
		headers["X-Priority"] = "5"
		accept := fmt.Sprintf(`http, 允许, POST, %q, body=%q, headers={"Content-Type":"application/json"}`,
			callback, fmt.Sprintf(`{"id":%q,"decision":"accept"}`, id))
		decline := fmt.Sprintf(`http, 拒绝, POST, %q, body=%q, headers={"Content-Type":"application/json"}`,
			callback, fmt.Sprintf(`{"id":%q,"decision":"decline"}`, id))
		headers["X-Actions"] = accept + "; " + decline
	}
	return deliver(target, headers, title, body)
}

// SendQuestion 推送"跑一半停下来等你选一下"，和审批一个性质。
// ntfy 通道给一个"去回答"按钮直达手机端页面；其余通道只发文字。
func SendQuestion(cfg store.Notify, title, body, openURL string) (int, error) {
	if !cfg.OnApproval {
		return 0, nil
	}
	target := Resolve(cfg)
	if target == nil {
		return 0, nil
	}
	headers := map[string]string{}
	if target.Format == "text" && openURL != "" {
		headers["X-Priority"] = "4"
		headers["X-Actions"] = fmt.Sprintf(`view, 去回答, %q, clear=true`, openURL)
	}
	return deliver(target, headers, title, body)
}

// deliver 按目标格式组装报文并发送；headers 只在目标认它们时才有意义（ntfy）。
func deliver(target *Target, headers map[string]string, title, body string) (int, error) {

	var payload []byte
	contentType := "application/json"
	switch target.Format {
	case "text":
		payload = []byte(title + "\n" + body)
		contentType = "text/plain; charset=utf-8"
	case "form":
		form := url.Values{}
		form.Set("title", title)
		form.Set("text", body)
		form.Set("desp", body)
		form.Set("content", body)
		payload = []byte(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case "pushplus":
		payload, _ = json.Marshal(map[string]string{
			"token": target.Token, "title": title,
			"content": title + "\n" + body, "template": "txt",
		})
	case "wecom":
		payload, _ = json.Marshal(map[string]any{
			"msgtype": "text",
			"text":    map[string]string{"content": title + "\n" + body},
		})
	default:
		payload, _ = json.Marshal(map[string]string{
			"title": title, "body": body, "text": body,
			"message": title + " " + body, "topic": "",
		})
	}

	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequest(http.MethodPost, target.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// Summarize 从助手回复里抽出一小段摘要用于推送。
func Summarize(text string) string {
	var b strings.Builder
	inCode := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
			if !inCode {
				b.WriteString("[代码] ")
			}
			continue
		}
		if inCode {
			continue
		}
		b.WriteString(line)
		b.WriteString(" ")
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	runes := []rune(out)
	if len(runes) > 180 {
		out = string(runes[:180])
	}
	return out
}

// Title 组装推送标题。
func Title(ok bool, name string) string {
	if name == "" {
		name = "Codex"
	}
	if ok {
		return "✅ " + name
	}
	return "⚠️ " + name
}
