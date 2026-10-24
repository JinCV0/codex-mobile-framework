package taskwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dailySample = `# 日报-2026-10-05（每日文献收集 · 定时任务）

> 检索时间：2026-10-05 08:00–08:20（Asia/Shanghai，周一）
> 使用到的数据源：PubMed / NCBI E-utilities
> 未能使用的数据源：Zotero 本地接口

## 病虫害检测（2 条）

**1. Identification of spectral biomarkers for early fungal decay in navel oranges**
- 作者：S. Pan, W. Luo（北京市农林科学院）
- 年份：2026（Food Chemistry，2026-08-21 在线）
- 刊源：Food Chemistry（Elsevier，JCR Q1，食品科学）
- 一句话要点：用 WGAN-GP 缓解样本稀缺，分类准确率 97.10%。
- 链接：https://pubmed.ncbi.nlm.nih.gov/42632366/

**2. Hyperspectral imaging-based early stress and disease detection**
- 刊源：Planta（Springer，JCR Q2，植物科学）
- 链接：https://pubmed.ncbi.nlm.nih.gov/42829355/

### 视觉伺服与目标跟踪（1 条）

**1. Airborne Intelligent System for Abnormal Pig Behavior Identification**
- 刊源：Animals（MDPI，JCR Q1/Q2，动物科学）
- 链接：https://pubmed.ncbi.nlm.nih.gov/42193799/

---

**检索备注**
- 去重：最近 14 天日报已读。
`

func TestDailyBlocks(t *testing.T) {
	blocks := dailyBlocks(dailySample)
	var all []string
	for _, b := range blocks {
		all = append(all, b.Text)
	}
	joined := strings.Join(all, "\n")

	for _, want := range []string{
		"## 病虫害检测（2 条）",
		"### 视觉伺服与目标跟踪（1 条）",
		"1. Identification of spectral biomarkers for early fungal decay in navel oranges 【Food Chemistry】",
		"https://pubmed.ncbi.nlm.nih.gov/42632366/",
		"2. Hyperspectral imaging-based early stress and disease detection 【Planta】",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("摘要里缺少：%s\n---- 实际 ----\n%s", want, joined)
		}
	}
	// 检索备注那一节不该混进条目
	if strings.Contains(joined, "去重") {
		t.Errorf("非条目内容混进来了：\n%s", joined)
	}
	if n := countEntries(blocks); n != 3 {
		t.Errorf("条目数应该是 3，实际 %d", n)
	}
	// 头部那几行只在本地版本里出现
	if _, dropped := fit(blocks, maxPushBytes, true); dropped != 0 {
		t.Errorf("示例日报不该被截断，dropped=%d", dropped)
	}
	if !strings.Contains(joined, "检索时间") {
		t.Errorf("本地版本应该保留检索时间")
	}
	if strings.Contains(mustFit(t, blocks, maxPushBytes, true), "检索时间") {
		t.Errorf("推送版本不该带上检索时间那几行")
	}
}

func mustFit(t *testing.T, blocks []block, limit int, skipMeta bool) string {
	t.Helper()
	out, _ := fit(blocks, limit, skipMeta)
	return out
}

func TestBuildTitleAndPush(t *testing.T) {
	now := time.Unix(1791158400, 0)
	item := build("文献日报", []pending{{
		r:    rule{Label: "文献日报", Prefix: "日报-", Ext: ".md", Kind: kindDaily},
		full: `C:\kb\00-收件箱\日报-2026-10-05.md`,
		name: "日报-2026-10-05.md",
		mod:  now.Unix(),
		text: dailySample,
	}}, now)
	if item.Title != "文献日报 · 2026-10-05（3 条）" {
		t.Errorf("标题不对：%q", item.Title)
	}
	if bytes := len([]byte(item.Push)); bytes > maxPushBytes+200 {
		t.Errorf("推送版本太长了：%d 字节", bytes)
	}
	if !strings.HasPrefix(item.ID, "task-") {
		t.Errorf("ID 应该带 task- 前缀：%q", item.ID)
	}
}

func TestNoteDigestStripsFrontmatter(t *testing.T) {
	note := `---
type: 文献笔记
title: "A Review of Robotic Weeding Modalities"
rating: 4
---

# A Review of Robotic Weeding Modalities

> 一句话概括：一篇「执行器视角」的定点除草机器人综述。

## 背景与问题

- 农业痛点：杂草与作物争水争肥争光。
`
	got := noteDigest(note, 600)
	if strings.Contains(got, "type: 文献笔记") {
		t.Errorf("frontmatter 没去掉：%s", got)
	}
	if !strings.Contains(got, "执行器视角") {
		t.Errorf("缺少一句话概括：%s", got)
	}
}

// TestRealDaily 是给自己看的：拿本机真实的日报量一量推送版本会不会超长。
func TestRealDaily(t *testing.T) {
	root := os.Getenv("TASKWATCH_SAMPLE_DIR")
	if root == "" {
		t.Skip("没有设置 TASKWATCH_SAMPLE_DIR，跳过")
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("读不到 %s: %v", root, err)
	}
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "日报-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		blocks := dailyBlocks(string(raw))
		push, dropped := fit(blocks, maxPushBytes, true)
		body, _ := fit(blocks, maxLocalBytes, false)
		t.Logf("%s：条目 %d，本机 %d 字节，推送 %d 字节，丢掉 %d 段",
			e.Name(), countEntries(blocks), len([]byte(body)), len([]byte(push)), dropped)
	}
}
