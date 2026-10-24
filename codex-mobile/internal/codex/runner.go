// Package codex 负责在本机调用 codex CLI，并把它的输出翻译成界面能懂的事件流。
//
// 调用方式是文档里已经实测过的：
//
//	首轮：codex exec --json --skip-git-repo-check -c sandbox_mode="..." -
//	续轮：codex exec resume --json --skip-git-repo-check -c sandbox_mode="..." <线程号> -
//
// 注意两点（开发文档 §9 踩过的坑）：
//   - resume 子命令不接受 -s/--sandbox，沙箱只能用 -c 传
//   - 提示词从 stdin 写入，末尾的 "-" 就是"从 stdin 读"的意思
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"codexmobile/internal/netenv"
)

// 单轮最长允许跑多久，超时自动停。
const turnTimeout = 15 * time.Minute

// Event 是要发给手机界面的一条事件。
type Event struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	ThreadID string           `json:"threadId,omitempty"`
	Usage    *json.RawMessage `json:"usage,omitempty"`
	// 审批相关（type == "approval" 时使用）
	ApprovalID string `json:"approvalId,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Title      string `json:"title,omitempty"`
	Detail     string `json:"detail,omitempty"`
	// 选择题（type == "question" 时使用）：Codex 让用户在几个选项里挑
	Questions []Question `json:"questions,omitempty"`
	// 文件改动卡片（type == "fileChange" 时使用）：几个文件、加了多少行、删了多少行
	Files   int `json:"files,omitempty"`
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
}

// Question 是 Codex 抛给用户的一道选择题。手机端直接把它画成可点的选项。
type Question struct {
	ID      string           `json:"id"`
	Header  string           `json:"header"`
	Text    string           `json:"text"`
	Options []QuestionOption `json:"options,omitempty"`
	Other   bool             `json:"other,omitempty"`
}

// QuestionOption 是选择题里的一个选项。
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Turn 是一轮对话的结果。
type Turn struct {
	ThreadID   string
	Text       string
	Activities []string
	OK         bool
}

// Runner 串行执行对话轮次：同一时间只允许一轮在跑。
type Runner struct {
	bin string

	mu      sync.Mutex
	current *exec.Cmd
	stopped bool
}

func NewRunner(bin string) *Runner { return &Runner{bin: bin} }

// FindBinary 找到 codex 可执行文件。
// 优先用环境变量指定，其次找 PATH，最后去 Codex 桌面版的安装目录里翻。
func FindBinary() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_BIN")); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	root := filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin")
	matches, _ := filepath.Glob(filepath.Join(root, "*", "codex.exe"))
	if len(matches) > 0 {
		// 目录名是版本哈希，取最新的一个。
		sort.Slice(matches, func(i, j int) bool {
			fi, err1 := os.Stat(matches[i])
			fj, err2 := os.Stat(matches[j])
			if err1 != nil || err2 != nil {
				return matches[i] > matches[j]
			}
			return fi.ModTime().After(fj.ModTime())
		})
		return matches[0]
	}
	return "codex"
}

// Busy 表示此刻是否有轮次在跑。
func (r *Runner) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current != nil
}

// Stop 中断当前轮次，返回是否真的停掉了什么。
func (r *Runner) Stop() bool {
	r.mu.Lock()
	cmd := r.current
	r.stopped = true
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return false
	}
	_ = cmd.Process.Kill()
	return true
}

func (r *Runner) args(threadID, sandbox string) []string {
	args := []string{"exec"}
	if threadID != "" {
		args = append(args, "resume", "--json", "--skip-git-repo-check")
	} else {
		args = append(args, "--json", "--skip-git-repo-check")
	}
	args = append(args, "-c", `sandbox_mode="`+sandbox+`"`)
	if sandbox == "workspace-write" {
		args = append(args, "-c", "sandbox_workspace_write.network_access=true")
	}
	if threadID != "" {
		args = append(args, threadID)
	}
	args = append(args, "-")
	return args
}

// Run 执行一轮对话，过程中通过 emit 把事件推给调用方。
func (r *Runner) Run(cwd, sandbox, threadID, prompt string, emit func(Event)) Turn {
	turn := Turn{}
	rawEmit := emit
	if rawEmit == nil {
		rawEmit = func(Event) {}
	}
	emit = newLogFilter(rawEmit)
	if !validSandbox(sandbox) {
		sandbox = "workspace-write"
	}

	cmd := exec.Command(r.bin, r.args(threadID, sandbox)...)
	cmd.Dir = cwd
	// 不走系统代理（这台机器上的 Clash）。
	cmd.Env = netenv.WithoutProxy()
	// 桌面版可能把 CODEX_CLI_PATH 指向了桥接程序，这里要指回真正的 codex。
	cmd.Env = netenv.Set(cmd.Env, "CODEX_CLI_PATH", r.bin)
	// 不要弹出黑色控制台窗口。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		emit(Event{Type: "error", Text: "无法读取 codex 输出: " + err.Error()})
		return turn
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		emit(Event{Type: "error", Text: "无法读取 codex 日志: " + err.Error()})
		return turn
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		emit(Event{Type: "error", Text: "无法写入 codex 输入: " + err.Error()})
		return turn
	}

	r.mu.Lock()
	r.stopped = false
	r.current = cmd
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.current = nil
		r.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		emit(Event{Type: "error", Text: "启动 codex 失败: " + err.Error()})
		return turn
	}

	timer := time.AfterFunc(turnTimeout, func() {
		emit(Event{Type: "error", Text: "超过 15 分钟，已自动停止。"})
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer timer.Stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				emit(Event{Type: "log", Text: line})
			}
		}
	}()

	go func() {
		_, _ = io.WriteString(stdin, prompt)
		_ = stdin.Close()
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 256*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		handleLine(line, emit, &turn)
	}
	_ = cmd.Wait()
	wg.Wait()

	r.mu.Lock()
	stopped := r.stopped
	r.stopped = false
	r.mu.Unlock()

	turn.OK = !stopped
	return turn
}

// 这些是 codex 内部组件的常规告警，对使用者没有意义，直接丢掉。
var noiseMarkers = []string{
	"codex_core_plugins::manifest",
	"codex_core_plugins::manager",
	"codex_core::shell_snapshot",
	"codex_models_manager::model_info",
	"codex_mcp::rmcp_client",
	"codex_skills::interface",
	"rmcp::transport::worker",
	"failed to initialize MCP client",
	"MCP server startup failed",
	"unrecognized configuration setting",
	"Under-development features",
}

func isNoiseLog(text string) bool {
	for _, marker := range noiseMarkers {
		if strings.Contains(strings.ToLower(text), strings.ToLower(marker)) {
			return true
		}
	}
	return isInternalWarn(text)
}

// codex 的内部模块会把 WARN/INFO/DEBUG 直接写到 stderr，
// 这些对使用者没有意义；真正的错误走 ERROR 或专门的 error 事件，不会误伤。
func isInternalWarn(text string) bool {
	upper := strings.ToUpper(text)
	for _, level := range []string{" WARN ", " INFO ", " DEBUG ", " TRACE "} {
		idx := strings.Index(upper, level)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(text[idx+len(level):])
		module := rest
		if cut := strings.IndexAny(rest, " :"); cut >= 0 {
			module = rest[:cut]
		}
		low := strings.ToLower(module)
		if strings.HasPrefix(low, "codex_") || strings.HasPrefix(low, "rmcp") || strings.HasPrefix(low, "mcp") {
			return true
		}
	}
	return false
}

// newLogFilter 去掉噪音、重复行，并给日志条数设上限，
// 免得手机上被几百行内部日志刷屏。
func newLogFilter(next func(Event)) func(Event) {
	const maxLogs = 30
	seen := map[string]bool{}
	count := 0
	return func(evt Event) {
		if evt.Type == "log" || evt.Type == "error" {
			if isNoiseLog(evt.Text) {
				return
			}
			if evt.Type == "log" {
				if seen[evt.Text] {
					return
				}
				seen[evt.Text] = true
				count++
				if count > maxLogs {
					return
				}
			}
		}
		next(evt)
	}
}

func validSandbox(mode string) bool {
	switch mode {
	case "read-only", "workspace-write", "danger-full-access":
		return true
	}
	return false
}

// handleLine 解析 codex 吐出的一行 JSON。
func handleLine(line string, emit func(Event), turn *Turn) {
	var evt struct {
		Type     string          `json:"type"`
		ThreadID string          `json:"thread_id"`
		Usage    json.RawMessage `json:"usage"`
		Item     *struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Path    string `json:"path"`
			Message string `json:"message"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		return
	}

	switch evt.Type {
	case "thread.started":
		if evt.ThreadID != "" {
			turn.ThreadID = evt.ThreadID
			emit(Event{Type: "thread", ThreadID: evt.ThreadID})
		}
	case "turn.started":
		emit(Event{Type: "working"})
	case "turn.completed":
		if len(evt.Usage) > 0 {
			usage := evt.Usage
			emit(Event{Type: "turn_done", Usage: &usage})
		} else {
			emit(Event{Type: "turn_done"})
		}
	case "item.completed", "item.updated":
		if evt.Item == nil {
			return
		}
		switch evt.Item.Type {
		case "agent_message":
			if evt.Item.Text != "" {
				turn.Text = evt.Item.Text
				emit(Event{Type: "message", Text: evt.Item.Text})
			}
		case "reasoning":
			emit(Event{Type: "reasoning"})
		case "command_execution":
			cmdText := evt.Item.Command
			if cmdText == "" {
				cmdText = "执行命令"
			}
			turn.Activities = append(turn.Activities, cmdText)
			emit(Event{Type: "tool", Text: cmdText})
		case "file_change":
			text := "修改文件"
			if evt.Item.Path != "" {
				text += "：" + evt.Item.Path
			}
			turn.Activities = append(turn.Activities, text)
			emit(Event{Type: "tool", Text: text})
		case "error":
			msg := evt.Item.Message
			// 这两类是无害噪音，过滤掉。
			if strings.Contains(msg, "unrecognized configuration setting") ||
				strings.Contains(msg, "Under-development features") {
				return
			}
			if msg != "" {
				emit(Event{Type: "log", Text: msg})
			}
		}
	}
}

var errNoBin = errors.New("找不到 codex 可执行文件")

// Check 在启动时做一次体检，早点把问题说清楚。
func (r *Runner) Check() error {
	if r.bin == "" {
		return errNoBin
	}
	if _, err := exec.LookPath(r.bin); err != nil {
		if _, statErr := os.Stat(r.bin); statErr != nil {
			return fmt.Errorf("%w: %s", errNoBin, r.bin)
		}
	}
	return nil
}
