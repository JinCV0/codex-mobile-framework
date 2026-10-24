package taskwatch

import (
	"path/filepath"
	"regexp"
	"strings"
)

// block 是消息里的一段。Meta 的段落只在电脑本地那份里出现，
// 推给 ntfy 的那份会省掉（省出来的字节留给正文）。
type block struct {
	Text string
	Meta bool
}

var entryRe = regexp.MustCompile(`^\*\*(\d+\.\s*)(.+?)\*\*$`)
var lineNumRe = regexp.MustCompile(`^\d+\.\s`)

// dailyBlocks 把日报压成"分类标题 + 条目 + 链接"的清单。
// 日报正文很长（每条约五行的要点、关注理由），手机上只需要能扫一遍标题和出处。
func dailyBlocks(text string) []block {
	var out []block
	var cur *strings.Builder
	var venue, link string

	flush := func() {
		if cur == nil {
			return
		}
		line := cur.String()
		if venue != "" {
			line += " 【" + venue + "】"
		}
		if link != "" {
			line += "\n" + link
		}
		out = append(out, block{Text: line})
		cur, venue, link = nil, "", ""
	}

	for _, raw := range strings.Split(text, "\n") {
		t := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if t == "" {
			continue
		}
		switch {
		case strings.HasPrefix(t, "# ") && !strings.HasPrefix(t, "##"):
			// 文件大标题，消息标题里已经有了
			continue
		case strings.HasPrefix(t, ">"):
			// 检索时间 / 数据源说明：本地那份留着，推送时省掉
			out = append(out, block{Text: cut(cleanInline(strings.TrimSpace(strings.TrimPrefix(t, ">"))), 160), Meta: true})
		case strings.HasPrefix(t, "##"), strings.HasPrefix(t, "###"):
			flush()
			out = append(out, block{Text: strings.TrimSpace(t)})
		case entryRe.MatchString(t):
			flush()
			m := entryRe.FindStringSubmatch(t)
			cur = &strings.Builder{}
			cur.WriteString(m[1] + cut(cleanInline(m[2]), 96))
		case strings.HasPrefix(t, "- 刊源"), strings.HasPrefix(t, "- 期刊"), strings.HasPrefix(t, "- 会议"):
			venue = shortVenue(strings.TrimSpace(afterColon(t)))
		case strings.HasPrefix(t, "- 链接"), strings.HasPrefix(t, "- 地址"):
			link = strings.TrimSpace(afterColon(t))
		}
	}
	flush()
	return out
}

// noteDigest 摘一条笔记的开头：一句话概括 + 前几行正文。
func noteDigest(text string, max int) string {
	body := stripFrontmatter(text)
	var lines []string
	total := 0
	for _, raw := range strings.Split(body, "\n") {
		t := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if t == "" || strings.HasPrefix(t, "#") || t == "---" {
			continue
		}
		if strings.HasPrefix(t, ">") {
			t = strings.TrimSpace(strings.TrimPrefix(t, ">"))
		}
		t = cleanInline(t)
		if t == "" {
			continue
		}
		t = cut(t, 130)
		lines = append(lines, t)
		total += len([]rune(t))
		if total >= max || len(lines) >= 6 {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// stripFrontmatter 去掉 YAML frontmatter（--- 开头的那一段）。
func stripFrontmatter(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return text
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return text
}

// cleanInline 把 markdown 标记去掉：手机上是纯文本显示，留着反而吵。
func cleanInline(s string) string {
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	for {
		i := strings.Index(s, "[[")
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], "]]")
		if j < 0 {
			break
		}
		inner := s[i+2 : i+j]
		if k := strings.IndexByte(inner, '|'); k >= 0 {
			inner = inner[:k]
		}
		s = s[:i] + inner + s[i+j+2:]
	}
	return strings.Join(strings.Fields(s), " ")
}

// fit 把段落拼起来，超过 limit 字节就停，返回丢掉了多少段。
func fit(blocks []block, limit int, skipMeta bool) (string, int) {
	kept := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if skipMeta && b.Meta {
			continue
		}
		if t := strings.TrimSpace(b.Text); t != "" {
			kept = append(kept, t)
		}
	}
	var sb strings.Builder
	for i, t := range kept {
		sep := ""
		if sb.Len() > 0 {
			sep = "\n"
		}
		if sb.Len()+len(sep)+len(t) > limit {
			return sb.String(), len(kept) - i
		}
		sb.WriteString(sep)
		sb.WriteString(t)
	}
	return sb.String(), 0
}

// countEntries 数一数有几个编号条目，用来在标题上写"（20 条）"。
func countEntries(blocks []block) int {
	n := 0
	for _, b := range blocks {
		if lineNumRe.MatchString(b.Text) {
			n++
		}
	}
	return n
}

// cleanName 把文件名变成人话：去掉前缀和扩展名。
func cleanName(name, prefix string) string {
	out := strings.TrimSuffix(name, filepath.Ext(name))
	out = strings.TrimPrefix(out, prefix)
	return cut(strings.TrimSpace(out), 28)
}

// shortVenue 刊源那一串通常带着一长段括号说明，只留刊名。
func shortVenue(s string) string {
	for _, sep := range []string{"（", "(", "，", ",", "；", ";", "：", ":"} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	return cut(strings.TrimSpace(s), 24)
}

// afterColon 取"刊源：xxx"里冒号后面那截。冒号有全角半角两种，按字符找。
func afterColon(s string) string {
	r := []rune(s)
	for i, c := range r {
		if c == '：' || c == ':' {
			return strings.TrimSpace(string(r[i+1:]))
		}
	}
	return ""
}

// cut 按字符（不是字节）截断，避免把中文切一半。
func cut(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
