package httpapi

import (
	"context"
	"testing"
	"time"
)

// 时间单位必须统一成毫秒：库里 thread_turns 是秒、threads 是毫秒，混用就会
// 显示成"20715 天前"（2026-10-10 线上就是这么错的）。
func TestToMillisNormalizesSeconds(t *testing.T) {
	secs := int64(1791205522) // 2026-10-05 左右，库里的原样
	got := toMillis(secs)
	if got != secs*1000 {
		t.Fatalf("秒应当乘 1000：%d -> %d", secs, got)
	}
	if d := time.Since(time.UnixMilli(got)); d < 0 || d > 30*24*time.Hour {
		t.Fatalf("换算出来的时间不在最近一个月内，单位还是不对：%v", time.UnixMilli(got))
	}
	ms := int64(1791205522000)
	if toMillis(ms) != ms {
		t.Fatalf("毫秒不该被再乘 1000：%d -> %d", ms, toMillis(ms))
	}
	if toMillis(0) != 0 {
		t.Fatal("0 还是 0")
	}
}

// 真去读一遍本机 Codex 的状态库（跟副屏同一份数据）。
//
// 这台机器上没有 sqlite3 / 没有状态库时跳过，别让别的环境红。
func TestReadCodexTurnStatus(t *testing.T) {
	if sqliteBin() == "" {
		t.Skip("这台机器上没有 sqlite3 命令行")
	}
	m, err := readCodexTurnStatus(context.Background())
	if err != nil {
		t.Skipf("读不到 Codex 状态库（跳过）：%v", err)
	}
	if len(m) == 0 {
		t.Skip("状态库里没有可用的对话")
	}
	// 时间戳必须是毫秒量级，否则手机那边会算出"两万天前"
	for id, st := range m {
		for _, v := range []int64{st.CompletedAt, st.StartedAt} {
			if v > 0 && v < 1_000_000_000_000 {
				t.Fatalf("%s 的时间戳还是秒（%d），手机端会显示成 1970 年", id, v)
			}
		}
		break
	}
	valid := map[string]bool{"completed": true, "failed": true, "interrupted": true, "inProgress": true}
	for id, st := range m {
		if st.Status == "" {
			t.Fatalf("%s 读出来状态是空的", id)
		}
		if !valid[st.Status] {
			t.Logf("注意：出现没见过的轮次状态 %q（%s）", st.Status, id)
		}
	}
	t.Logf("读到 %d 条对话的最近一轮状态", len(m))
}
