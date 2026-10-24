package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 读 Codex 自己的状态库，拿"每条对话最近一轮跑成什么样"。
//
// 副屏（MagicMirror 的 MMM-CodexStatus）就是这么干的，这里照做：
//
//	state_5.sqlite          -> threads       每条对话一行（用来挑最近的）
//	thread_history_1.sqlite -> thread_turns  每一轮一行，带 status / 耗时
//
// 只读、走索引、结果缓存 20 秒 —— /api/status 手机每 4 秒问一次，不能每次都读盘。
//
// 为什么用系统里的 sqlite3 命令行、而不是引一个 Go 的 sqlite 驱动：
//   - 带 CGO 的驱动会让"单个 exe 免安装"这条线断掉；
//   - 纯 Go 的驱动很大（exe 从 23MB 涨到 35MB 上下，每次构建还要慢一截）。
//
// 本机有两份 sqlite3（miniconda / android platform-tools）任取其一；一个都找不到
// 就静默退回"手机自己记的那份状态"，列表照样能用，只是新对话要打开过才显示结局。

type codexTurnStatus struct {
	Status      string `json:"status"`
	DurationMs  int64  `json:"durationMs,omitempty"`
	CompletedAt int64  `json:"completedAt,omitempty"`
	StartedAt   int64  `json:"startedAt,omitempty"`
}

var codexStatusCache struct {
	sync.Mutex
	at       time.Time
	statuses map[string]codexTurnStatus
	reading  bool
	bin      string // sqlite3 的路径，空字符串表示已经找过、没有
	probed   bool
}

const codexStatusTTL = 20 * time.Second

func codexHome() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// sqliteBin 找 sqlite3 命令行：先 PATH，再几个已知位置。
func sqliteBin() string {
	codexStatusCache.Lock()
	if codexStatusCache.probed {
		bin := codexStatusCache.bin
		codexStatusCache.Unlock()
		return bin
	}
	codexStatusCache.Unlock()

	cands := []string{}
	if p, err := exec.LookPath("sqlite3"); err == nil {
		cands = append(cands, p)
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands,
			filepath.Join(home, "miniconda3", "Library", "bin", "sqlite3.exe"),
			filepath.Join(home, "scoop", "shims", "sqlite3.exe"),
		)
	}
	found := ""
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			found = c
			break
		}
	}

	codexStatusCache.Lock()
	codexStatusCache.probed = true
	codexStatusCache.bin = found
	codexStatusCache.Unlock()
	return found
}

// queryJSON 用 sqlite3 只读跑一条查询，拿 JSON 出来。
func queryJSON(ctx context.Context, bin, db, sql string) ([]byte, error) {
	if _, err := os.Stat(db); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "-readonly", "-json", db, sql).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readCodexTurnStatus 真去读一遍两个库。
//
// thread_turns 只有几百行（那个 176MB 的大表是 thread_items，不碰），
// 整表取"每条第最近一轮"实测 20 多毫秒。
func readCodexTurnStatus(ctx context.Context) (map[string]codexTurnStatus, error) {
	bin := sqliteBin()
	home := codexHome()
	if bin == "" || home == "" {
		return nil, os.ErrNotExist
	}

	// 最近活跃的那批对话（我们只关心这一批，省得把 178 条全下发给手机）
	rawIDs, err := queryJSON(ctx, bin, filepath.Join(home, "state_5.sqlite"),
		"SELECT id FROM threads WHERE archived=0 ORDER BY updated_at_ms DESC LIMIT 60;")
	if err != nil {
		return nil, err
	}
	var idRows []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rawIDs, &idRows); err != nil {
		return nil, err
	}
	recent := make(map[string]bool, len(idRows))
	for _, r := range idRows {
		recent[r.ID] = true
	}
	if len(recent) == 0 {
		return map[string]codexTurnStatus{}, nil
	}

	rawTurns, err := queryJSON(ctx, bin, filepath.Join(home, "thread_history_1.sqlite"),
		"SELECT thread_id, status, duration_ms, started_at, completed_at FROM "+
			"(SELECT *, ROW_NUMBER() OVER (PARTITION BY thread_id ORDER BY rollout_ordinal DESC) AS rn "+
			"FROM thread_turns) WHERE rn=1;")
	if err != nil {
		return nil, err
	}
	var turnRows []struct {
		ThreadID    string `json:"thread_id"`
		Status      string `json:"status"`
		DurationMs  int64  `json:"duration_ms"`
		StartedAt   int64  `json:"started_at"`
		CompletedAt int64  `json:"completed_at"`
	}
	if err := json.Unmarshal(rawTurns, &turnRows); err != nil {
		return nil, err
	}

	out := make(map[string]codexTurnStatus, len(turnRows))
	for _, r := range turnRows {
		if r.ThreadID == "" || !recent[r.ThreadID] {
			continue
		}
		out[r.ThreadID] = codexTurnStatus{
			Status:      r.Status,
			DurationMs:  r.DurationMs,
			StartedAt:   toMillis(r.StartedAt),
			CompletedAt: toMillis(r.CompletedAt),
		}
	}
	return out, nil
}

// toMillis 把 Codex 库里两种时间单位统一成毫秒。
//
// 坑在这里：threads 表用的是 updated_at_ms（毫秒），而 thread_turns 的
// started_at / completed_at 是**秒**（duration_ms 又是毫秒）。混着用的结果就是
// 手机上显示"20715 天前"（拿秒去减毫秒，等于从 1970 年算起）。
// 10^12 毫秒大约是 2001 年，比它小的都当秒处理。
func toMillis(v int64) int64 {
	if v > 0 && v < 1_000_000_000_000 {
		return v * 1000
	}
	return v
}

// turnStatuses 给 /api/status 用：先把上一次读到的给出去，过期了再后台换新的。
// 读盘失败不报错，只是这一次没有新数据（手机那边会退回自己记的那份）。
func (s *Server) turnStatuses() map[string]codexTurnStatus {
	// 先在锁外面问：sqliteBin() 自己也要拿同一把锁，放在下面会自死锁
	// （Go 的 sync.Mutex 不可重入，2026-10-10 就是这么卡死一个测试的）。
	bin := sqliteBin()
	codexStatusCache.Lock()
	stale := time.Since(codexStatusCache.at) > codexStatusTTL
	out := codexStatusCache.statuses
	busy := codexStatusCache.reading
	if stale && !busy && bin != "" {
		codexStatusCache.reading = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			m, err := readCodexTurnStatus(ctx)
			codexStatusCache.Lock()
			if err == nil {
				codexStatusCache.statuses = m
				codexStatusCache.at = time.Now()
			} else {
				codexStatusCache.at = time.Now() // 失败也退避 20 秒，别每 4 秒就去戳一次
			}
			codexStatusCache.reading = false
			codexStatusCache.Unlock()
		}()
	}
	codexStatusCache.Unlock()
	return out
}
