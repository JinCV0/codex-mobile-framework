package appserver

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ListAllThreads 一次把电脑上的对话全部拉回来（自动翻页），并缓存 30 秒。
// 手机端列表因此可以秒开，不必每次切页都问一遍协议。
func (c *Client) ListAllThreads(ctx context.Context, refresh bool) ([]ThreadSummary, error) {
	c.cacheMu.Lock()
	if !refresh && c.cacheList != nil && time.Since(c.cacheAt) < 30*time.Second {
		out := c.cacheList
		c.cacheMu.Unlock()
		return out, nil
	}
	c.cacheMu.Unlock()

	all := make([]ThreadSummary, 0, 64)
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 8; page++ { // 最多 8 页，防止极端情况无限翻
		list, next, err := c.ListThreads(ctx, 100, cursor, "", false)
		if err != nil {
			if len(all) > 0 {
				break
			}
			return nil, err
		}
		for _, t := range list {
			// 同一条 thread 可能同时出现在多页里，跨页也要去重
			if t.ID == "" || seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			all = append(all, t)
		}
		if next == "" {
			break
		}
		cursor = next
	}

	c.cacheMu.Lock()
	c.cacheList = all
	c.cacheAt = time.Now()
	c.cacheMu.Unlock()
	return all, nil
}

// ThreadSummary 是"电脑上的一条对话"的摘要，用于列表展示。
type ThreadSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Preview   string `json:"preview"`
	Cwd       string `json:"cwd"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ThreadMessage 是从电脑上读回来的一条消息。
type ThreadMessage struct {
	Role       string   `json:"role"`
	Text       string   `json:"text"`
	Activities []string `json:"activities,omitempty"`
	Ts         int64    `json:"ts"`
	// 这一轮用了多久（毫秒）。桌面端每条回复下面都写着用时，手机照抄一份：
	// 由电脑端给，手机有没有盯着过程都不影响。
	DurationMs int64 `json:"durationMs,omitempty"`
	// 这一轮改了几个文件、增删多少行（汇总成一张卡，不给每个文件都发一条）
	Files   int `json:"files,omitempty"`
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
}

// ListThreads 列出电脑上 Codex 自己的对话（不只是这个 App 建的）。
// 这就是"同步电脑上的聊天记录"的来源：桌面端、CLI 里聊过的，这里都能看到。
func (c *Client) ListThreads(ctx context.Context, limit int, cursor, search string, archived bool) ([]ThreadSummary, string, error) {
	if err := c.Ensure(ctx, ""); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	params := map[string]any{
		"limit":         limit,
		"sortKey":       "updated_at",
		"sortDirection": "desc",
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if search != "" {
		params["searchTerm"] = search
	}
	if archived {
		params["archived"] = true
	}

	var res struct {
		Data []struct {
			ID             string  `json:"id"`
			Name           *string `json:"name"`
			Preview        string  `json:"preview"`
			Cwd            string  `json:"cwd"`
			CreatedAt      int64   `json:"createdAt"`
			UpdatedAt      int64   `json:"updatedAt"`
			ParentThreadID *string `json:"parentThreadId"`
			AgentNickname  *string `json:"agentNickname"`
			Ephemeral      bool    `json:"ephemeral"`
		} `json:"data"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := c.call(ctx, "thread/list", params, &res); err != nil {
		return nil, "", err
	}

	out := make([]ThreadSummary, 0, len(res.Data))
	seen := make(map[string]bool, len(res.Data))
	for _, t := range res.Data {
		// 子代理线程不是用户对话，列表里不显示
		if t.ParentThreadID != nil || t.AgentNickname != nil || t.Ephemeral {
			continue
		}
		// 电脑上的会话库里同一条 thread 可能被记了多份，只留最新的那条
		if t.ID == "" || seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		name := ""
		if t.Name != nil {
			name = strings.TrimSpace(*t.Name)
		}
		out = append(out, ThreadSummary{
			ID: t.ID, Name: name, Preview: strings.TrimSpace(t.Preview),
			Cwd: t.Cwd, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		})
	}
	next := ""
	if res.NextCursor != nil {
		next = *res.NextCursor
	}
	return out, next, nil
}

// ReadThread 把一条对话的完整内容读回来，翻译成和本地会话一样的消息格式。
// ReadThread 读回一条对话的内容，外加**最后一轮的状态**
// （completed / interrupted / failed / inProgress）——手机靠它决定要不要
// 显示"继续"按钮：上一轮被打断了才需要。
func (c *Client) ReadThread(ctx context.Context, threadID string) ([]ThreadMessage, string, error) {
	if err := c.Ensure(ctx, ""); err != nil {
		return nil, "", err
	}
	var res struct {
		Thread struct {
			Turns []struct {
				Status      string `json:"status"`
				DurationMs  *int64 `json:"durationMs"`
				StartedAt   *int64 `json:"startedAt"`
				CompletedAt *int64 `json:"completedAt"`
				Items       []struct {
					Type    string            `json:"type"`
					Text    string            `json:"text"`
					Command string            `json:"command"`
					Status  string            `json:"status"`
					Content []json.RawMessage `json:"content"`
					Changes []struct {
						Path string `json:"path"`
						Diff string `json:"diff"`
					} `json:"changes"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	err := c.call(ctx, "thread/read", map[string]any{
		"threadId": threadID, "includeTurns": true,
	}, &res)
	if len(res.Thread.Turns) == 0 {
		// 线程没被加载时读不到内容，先 resume 一下再读
		if rerr := c.call(ctx, "thread/resume", map[string]any{"threadId": threadID}, nil); rerr == nil {
			_ = c.call(ctx, "thread/read", map[string]any{
				"threadId": threadID, "includeTurns": true,
			}, &res)
		}
	}
	if err != nil && len(res.Thread.Turns) == 0 {
		return nil, "", err
	}

	var out []ThreadMessage
	lastStatus := ""
	for _, turn := range res.Thread.Turns {
		if turn.Status != "" {
			lastStatus = turn.Status
		}
		// 这一轮用了多久：优先用协议给的 durationMs，没有就用起止时间算
		durMs := int64(0)
		if turn.DurationMs != nil {
			durMs = *turn.DurationMs
		} else if turn.StartedAt != nil && turn.CompletedAt != nil {
			durMs = (*turn.CompletedAt - *turn.StartedAt) * 1000
		}
		var activities []string
		turnFileCount, turnAdded, turnRemoved := 0, 0, 0
		ts := int64(0)
		if turn.StartedAt != nil {
			ts = *turn.StartedAt * 1000
		}

		// 把这一 turn 的 item 按"用户消息"切成若干段：一段 = 一条用户消息 +
		// 紧随其后的助手输出。这么切是两件事同时要满足：
		//   1. 一轮里助手可能分好几次说话（工具调用之间插的那些短句），
		//      手机只看这一段的最后一条完整回复，中间那些是过程噪音；
		//   2. 一个 turn 里也可能并进好几轮（手机连着发两条消息时
		//      app-server 会这么干），那几轮各自的回复都得留下 ——
		//      以前是"后一条覆盖前一条"，上一轮的回复会被这一轮的话顶掉，
		//      连回复里的图片一起消失（2026-10-11 报的 bug）。
		type segment struct {
			user     *ThreadMessage
			reply    string
			replyDur int64
		}
		var segs []*segment
		var cur *segment

		for _, it := range turn.Items {
			switch it.Type {
			case "userMessage":
				var parts []string
				for _, c := range it.Content {
					if t := textOf(c); t != "" {
						parts = append(parts, t)
					}
				}
				if len(parts) == 0 {
					continue
				}
				um := ThreadMessage{Role: "user", Text: strings.Join(parts, "\n"), Ts: ts}
				cur = &segment{user: &um}
				segs = append(segs, cur)
			case "agentMessage":
				if it.Text == "" {
					continue
				}
				if cur == nil {
					// 少见：turn 一开头就是助手输出，前面没有用户消息
					cur = &segment{}
					segs = append(segs, cur)
				}
				cur.reply = it.Text
				cur.replyDur = durMs
			case "commandExecution":
				if it.Command != "" {
					line := it.Command
					if len(line) > 160 {
						line = line[:160] + " …"
					}
					activities = append(activities, line)
				}
			case "fileChange":
				activities = append(activities, "修改文件")
				turnFileCount += len(it.Changes)
				// 只统计汇总（几个文件、加删多少行）给手机。
				// 每个文件的路径和 diff 不往手机送：公网一趟 0.4 秒起步，
				// 而手机端压根不展示这些明细，纯属白搬。
				for _, ch := range it.Changes {
					for _, line := range strings.Split(ch.Diff, "\n") {
						switch {
						case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
						case strings.HasPrefix(line, "+"):
							turnAdded++
						case strings.HasPrefix(line, "-"):
							turnRemoved++
						}
					}
				}
			case "reasoning":
				activities = append(activities, "思考中")
			case "webSearch":
				activities = append(activities, "联网搜索")
			}
		}
		// 按段落顺序输出：每段的用户消息 + 这一段的最终回复
		for _, sg := range segs {
			if sg.user != nil {
				out = append(out, *sg.user)
			}
			if sg.reply != "" {
				out = append(out, ThreadMessage{
					Role: "assistant", Text: sg.reply, Ts: ts, DurationMs: sg.replyDur,
				})
			}
		}
		if len(activities) > 0 {
			// 把这一轮里的工具调用挂到最后一条助手消息上
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].Role == "assistant" {
					out[i].Activities = append(out[i].Activities, activities...)
					out[i].Files = turnFileCount
					out[i].Added = turnAdded
					out[i].Removed = turnRemoved
					break
				}
			}
		}
	}
	return out, lastStatus, nil
}

// textOf 兼容两种序列化：内容项可能是 {"type":"text","text":"..."}，
// 也可能直接就是一个字符串。
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var o struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return o.Text
	}
	return ""
}

// RawCall 给需要直接调协议的场景用（比如以后加新接口）。
func (c *Client) RawCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.call(ctx, method, params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
