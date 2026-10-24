// Package sysmon 采集这台电脑的运行状态：CPU、每核占用、内存、磁盘、磁盘 IO、
// 分网卡流量、显卡、开机时长、占内存的进程。
//
// 手机隔着公网看不到电脑里发生了什么，这些数字只能在电脑上量，量完由
// /api/sysinfo 交给手机显示。全部走 Win32，不引第三方运行时依赖：
// x/sys/windows 里有的直接用，没有的用 kernel32 / ntdll / pdh 的懒加载调用。
package sysmon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// 采样间隔：1.5 秒一次，既够看趋势，又几乎不吃 CPU。
	// PDH 的速率计数器也要求两次采集间隔在 1 秒以上，正好合上。
	sampleInterval = 1500 * time.Millisecond
	// 留 40 个点，正好是最近一分钟。
	historyLen = 40
	// 进程榜和显卡状态每次现算太费，各自缓存几秒。
	procCacheFor = 5 * time.Second
	gpuCacheFor  = 5 * time.Second
	// 进程榜只列占用最高的几个。
	procTopN = 6

	// DRIVE_FIXED：本地固定磁盘。网络盘和光驱不碰，免得卡住。
	driveFixed = 3
	// 要跳过的接口类型：回环、虚拟（Tailscale / VPN 那种）、隧道。
	// 它们的流量都从物理网卡上走一遍，算进来就翻倍。
	ifTypeLoopback    = 24
	ifTypePropVirtual = 53
	ifTypeTunnel      = 131
	// InterfaceAndOperStatusFlags 里的两位：这是个真网卡 / 这是个 NDIS 过滤层。
	// 过滤层（各家的 WFP / QoS 伪接口）报的是同一份流量，算进来会翻好几倍。
	ifFlagHardware = 0x01
	ifFlagFilter   = 0x02
	// PROCESS_QUERY_LIMITED_INFORMATION
	processQueryLimitedInformation = 0x1000
	// NtQuerySystemInformation 的信息类别：每个逻辑处理器的占用时间
	systemProcessorPerformanceInformation = 8
	// PDH_FMT_DOUBLE
	pdhFmtDouble = 0x00000200
	// CREATE_NO_WINDOW：叫 nvidia-smi 时别弹出黑窗
	createNoWindow = 0x08000000
)

// Sample 是一个时间点上的几个关键数字，攒起来画曲线。
type Sample struct {
	At       int64   `json:"at"`
	CPU      float64 `json:"cpu"`
	MemUsed  uint64  `json:"memUsed"`
	MemTotal uint64  `json:"memTotal"`
	NetRx    uint64  `json:"netRx"`
	NetTx    uint64  `json:"netTx"`
	IORead   uint64  `json:"ioRead"`
	IOWrite  uint64  `json:"ioWrite"`
}

// Disk 是一个本地磁盘的容量。
type Disk struct {
	Name  string `json:"name"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

// Proc 是进程榜上的一行。同名进程（浏览器那种）合并成一条，N 是进程数。
type Proc struct {
	Name string `json:"name"`
	Mem  uint64 `json:"mem"`
	N    int    `json:"n"`
}

// NIC 是一张在用的网卡。Rx / Tx 是每秒字节，Speed 是链路速度（bit/s）。
type NIC struct {
	Index     uint32 `json:"-"`
	Name      string `json:"name"`
	Rx        uint64 `json:"rx"`
	Tx        uint64 `json:"tx"`
	RxTotal   uint64 `json:"rxTotal"`
	TxTotal   uint64 `json:"txTotal"`
	Speed     uint64 `json:"speed"`
	InOctets  uint64 `json:"-"`
	OutOctets uint64 `json:"-"`
}

// GPU 是一张显卡的即时状态。读不到时 Snapshot.GPUNote 会写明原因。
type GPU struct {
	Name     string  `json:"name"`
	Util     float64 `json:"util"`
	MemUsed  uint64  `json:"memUsed"`
	MemTotal uint64  `json:"memTotal"`
	Temp     float64 `json:"temp"`
}

// Snapshot 是手机一次请求拿到的全部内容。
type Snapshot struct {
	At         int64     `json:"at"`
	Host       string    `json:"host"`
	Uptime     int64     `json:"uptime"` // 秒
	Cores      int       `json:"cores"`
	CPU        float64   `json:"cpu"` // 0-100
	PerCore    []float64 `json:"perCore"`
	MemTotal   uint64    `json:"memTotal"`
	MemUsed    uint64    `json:"memUsed"`
	SwapTotal  uint64    `json:"swapTotal"`
	SwapUsed   uint64    `json:"swapUsed"`
	NetRx      uint64    `json:"netRx"` // 每秒字节
	NetTx      uint64    `json:"netTx"`
	NetRxTotal uint64    `json:"netRxTotal"` // 开机以来累计
	NetTxTotal uint64    `json:"netTxTotal"`
	IORead     uint64    `json:"ioRead"` // 每秒字节
	IOWrite    uint64    `json:"ioWrite"`
	IONote     string    `json:"ioNote,omitempty"` // 磁盘 IO 读不到时说明原因
	Disks      []Disk    `json:"disks"`
	Procs      []Proc    `json:"procs"`
	NICs       []NIC     `json:"nics"`
	GPUs       []GPU     `json:"gpus"`
	GPUNote    string    `json:"gpuNote,omitempty"`
	History    []Sample  `json:"history"`
}

// Collector 常驻在电脑端，隔一会儿采一次；手机来问就直接把现成的给它。
type Collector struct {
	mu       sync.Mutex
	hist     []Sample
	lastIdle uint64
	lastKern uint64
	lastUser uint64
	lastRx   uint64
	lastTx   uint64
	lastAt   time.Time
	primed   bool
	host     string

	corePrev []processorPerf
	coreCPU  []float64
	nicPrev  map[uint32]nicCount
	nicRates []NIC
	ioRead   uint64
	ioWrite  uint64
	ioNote   string

	pdh pdhDisk

	gpuMu   sync.Mutex
	gpuAt   time.Time
	gpus    []GPU
	gpuNote string
	gpuDone bool

	procMu sync.Mutex
	procAt time.Time
	procs  []Proc
}

func New() *Collector {
	host, _ := os.Hostname()
	return &Collector{host: host, nicPrev: map[uint32]nicCount{}}
}

// Run 一直采到进程退出为止。
func (c *Collector) Run(ctx context.Context) {
	tick := time.NewTicker(sampleInterval)
	defer tick.Stop()
	c.sample()
	for {
		select {
		case <-ctx.Done():
			c.pdh.close()
			return
		case <-tick.C:
			c.sample()
		}
	}
}

// Snapshot 取当前状态。历史不够时先补一个点，保证手机第一次打开就有内容。
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	if len(c.hist) == 0 {
		c.mu.Unlock()
		c.sample()
		c.mu.Lock()
	}
	last := c.hist[len(c.hist)-1]
	hist := append([]Sample(nil), c.hist...)
	perCore := append([]float64(nil), c.coreCPU...)
	nics := append([]NIC(nil), c.nicRates...)
	ioNote := c.ioNote
	c.mu.Unlock()

	usage := memStatus{}
	_ = memoryStatus(&usage)
	rxTotal, txTotal := netTotalBytes()
	uptime, _ := tickCount64()
	gpus, gpuNote := c.readGPUs()

	return Snapshot{
		At:         time.Now().UnixMilli(),
		Host:       c.host,
		Uptime:     int64(uptime / 1000),
		Cores:      runtime.NumCPU(),
		CPU:        last.CPU,
		PerCore:    perCore,
		MemTotal:   usage.TotalPhys,
		MemUsed:    usage.TotalPhys - usage.AvailPhys,
		SwapTotal:  usage.TotalPageFile,
		SwapUsed:   usage.TotalPageFile - usage.AvailPageFile,
		NetRx:      last.NetRx,
		NetTx:      last.NetTx,
		NetRxTotal: rxTotal,
		NetTxTotal: txTotal,
		IORead:     last.IORead,
		IOWrite:    last.IOWrite,
		IONote:     ioNote,
		Disks:      disks(),
		Procs:      c.topProcs(),
		NICs:       nics,
		GPUs:       gpus,
		GPUNote:    gpuNote,
		History:    hist,
	}
}

func (c *Collector) sample() {
	now := time.Now()
	idle, kern, user, err := systemTimes()
	if err != nil {
		return
	}
	usage := memStatus{}
	_ = memoryStatus(&usage)
	rows, rx, tx := ifRows()
	cores := coreTimes()
	ioRead, ioWrite, ioNote := c.pdh.read()

	c.mu.Lock()
	defer c.mu.Unlock()

	var cpu float64
	var rxRate, txRate uint64
	secs := now.Sub(c.lastAt).Seconds()
	if c.primed {
		dIdle := idle - c.lastIdle
		dBusy := (kern - c.lastKern) + (user - c.lastUser)
		if dBusy > 0 {
			cpu = clampPct(100 * float64(dBusy-dIdle) / float64(dBusy))
		}
		if secs > 0.2 {
			if rx >= c.lastRx {
				rxRate = uint64(float64(rx-c.lastRx) / secs)
			}
			if tx >= c.lastTx {
				txRate = uint64(float64(tx-c.lastTx) / secs)
			}
		}
	}
	if secs <= 0.2 {
		secs = sampleInterval.Seconds()
	}

	// 每核占用：和总量一样是"两次采样之差"。
	c.coreCPU = make([]float64, len(cores))
	for i := range cores {
		var v float64
		if i < len(c.corePrev) {
			prev := c.corePrev[i]
			dIdle := cores[i].IdleTime - prev.IdleTime
			dBusy := (cores[i].KernelTime - prev.KernelTime) + (cores[i].UserTime - prev.UserTime)
			if dBusy > 0 {
				v = clampPct(100 * float64(dBusy-dIdle) / float64(dBusy))
			}
		}
		c.coreCPU[i] = v
	}
	c.corePrev = cores

	// 分网卡速率：按接口序号对上一条，算差值。
	next := make(map[uint32]nicCount, len(rows))
	c.nicRates = c.nicRates[:0]
	for _, row := range rows {
		cur := nicCount{rx: row.InOctets, tx: row.OutOctets}
		var rxB, txB uint64
		if prev, ok := c.nicPrev[row.Index]; ok {
			if cur.rx >= prev.rx {
				rxB = uint64(float64(cur.rx-prev.rx) / secs)
			}
			if cur.tx >= prev.tx {
				txB = uint64(float64(cur.tx-prev.tx) / secs)
			}
		}
		next[row.Index] = cur
		c.nicRates = append(c.nicRates, NIC{
			Index: row.Index, Name: row.Name, Rx: rxB, Tx: txB,
			RxTotal: row.InOctets, TxTotal: row.OutOctets, Speed: row.Speed,
		})
	}
	c.nicPrev = next
	sort.Slice(c.nicRates, func(i, j int) bool {
		return c.nicRates[i].Rx+c.nicRates[i].Tx > c.nicRates[j].Rx+c.nicRates[j].Tx
	})

	c.lastIdle, c.lastKern, c.lastUser = idle, kern, user
	c.lastRx, c.lastTx = rx, tx
	c.lastAt = now
	c.primed = true
	c.ioRead, c.ioWrite, c.ioNote = ioRead, ioWrite, ioNote

	c.hist = append(c.hist, Sample{
		At: now.UnixMilli(), CPU: cpu,
		MemUsed: usage.TotalPhys - usage.AvailPhys, MemTotal: usage.TotalPhys,
		NetRx: rxRate, NetTx: txRate,
		IORead: c.ioRead, IOWrite: c.ioWrite,
	})
	if len(c.hist) > historyLen {
		c.hist = append([]Sample(nil), c.hist[len(c.hist)-historyLen:]...)
	}
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// readGPUs 读显卡状态。用 NVIDIA 驱动自带的 nvidia-smi：免安装、免驱动、
// 输出稳定，是 Windows 上拿 GPU 占用/显存/温度最省事的办法。
// 没有 N 卡（或没装驱动）就返回一句说明，界面据此隐藏这张卡。
func (c *Collector) readGPUs() ([]GPU, string) {
	c.gpuMu.Lock()
	defer c.gpuMu.Unlock()
	if c.gpuDone && time.Since(c.gpuAt) < gpuCacheFor {
		return c.gpus, c.gpuNote
	}
	list, note := queryGPUs()
	c.gpus, c.gpuNote, c.gpuAt, c.gpuDone = list, note, time.Now(), true
	return list, note
}

func queryGPUs() ([]GPU, string) {
	exe := findNvidiaSMI()
	if exe == "" {
		return nil, "没找到 nvidia-smi（只有 NVIDIA 显卡才有）"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe,
		"--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu",
		"--format=csv,noheader,nounits")
	// 电脑端是 GUI 程序，不设这个的话每问一次就闪一个黑窗
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	out, err := cmd.Output()
	if err != nil {
		return nil, "nvidia-smi 没跑起来"
	}
	var list []GPU
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 5 {
			continue
		}
		g := GPU{
			Name:     strings.TrimSpace(fields[0]),
			Util:     parseFloat(fields[1]),
			MemUsed:  uint64(parseFloat(fields[2])) << 20, // nvidia-smi 给的是 MiB
			MemTotal: uint64(parseFloat(fields[3])) << 20,
			Temp:     parseFloat(fields[4]),
		}
		if g.Name != "" {
			list = append(list, g)
		}
	}
	if len(list) == 0 {
		return nil, "没读到显卡数据"
	}
	return list, ""
}

func findNvidiaSMI() string {
	if p, err := exec.LookPath("nvidia-smi.exe"); err == nil {
		return p
	}
	for _, p := range []string{
		filepath.Join(os.Getenv("SystemRoot"), "System32", "nvidia-smi.exe"),
		`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
		`C:\Program Files\NVIDIA Corporation\NVIDIA App\nvidia-smi.exe`,
	} {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

// topProcs 列出占内存最多的几个程序。同名合并，省得一屏全是 chrome.exe。
func (c *Collector) topProcs() []Proc {
	c.procMu.Lock()
	if time.Since(c.procAt) < procCacheFor && c.procs != nil {
		list := c.procs
		c.procMu.Unlock()
		return list
	}
	c.procMu.Unlock()

	list := readProcs()
	c.procMu.Lock()
	c.procs, c.procAt = list, time.Now()
	c.procMu.Unlock()
	return list
}

// ---------- Win32 ----------

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	ntdll    = windows.NewLazySystemDLL("ntdll.dll")
	psapi    = windows.NewLazySystemDLL("psapi.dll")
	pdhDLL   = windows.NewLazySystemDLL("pdh.dll")

	procSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procMemoryStatus      = kernel32.NewProc("GlobalMemoryStatusEx")
	procTickCount64       = kernel32.NewProc("GetTickCount64")
	procProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	procQuerySystemInfo   = ntdll.NewProc("NtQuerySystemInformation")

	procPdhOpenQuery         = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounter = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData  = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFmtCounter     = pdhDLL.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery        = pdhDLL.NewProc("PdhCloseQuery")
)

// systemTimes 返回开机以来的 空闲 / 内核 / 用户 时间（100ns 单位）。
// GetSystemTimes 的 kernel 时间里含 idle，算占用率时要减掉。
func systemTimes() (idle, kern, user uint64, err error) {
	var i, k, u windows.Filetime
	r, _, e := procSystemTimes.Call(
		uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, 0, e
	}
	return filetimeToUint64(i), filetimeToUint64(k), filetimeToUint64(u), nil
}

func filetimeToUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// processorPerf 对应 PROCESSOR_PERFORMANCE_INFORMATION，48 字节，字段顺序别动。
type processorPerf struct {
	IdleTime       int64
	KernelTime     int64
	UserTime       int64
	DpcTime        int64
	InterruptTime  int64
	InterruptCount uint32
	_              uint32
}

// coreTimes 一次问出所有逻辑核的累计时间。GetSystemTimes 只给总量，
// 想看每核就得走 NtQuerySystemInformation 这条路（任务管理器也这么干）。
func coreTimes() []processorPerf {
	n := runtime.NumCPU()
	if n <= 0 {
		return nil
	}
	// 缓冲区给大一点，返回多少个就认多少个（最多认 n 个）。
	buf := make([]processorPerf, n+64)
	var ret uint32
	r, _, _ := procQuerySystemInfo.Call(
		systemProcessorPerformanceInformation,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf))*unsafe.Sizeof(buf[0]),
		uintptr(unsafe.Pointer(&ret)))
	if r != 0 {
		return nil
	}
	got := int(ret) / int(unsafe.Sizeof(buf[0]))
	if got > n {
		got = n
	}
	if got <= 0 {
		return nil
	}
	return buf[:got]
}

// memStatus 对应 MEMORYSTATUSEX。字段顺序和 Windows 头文件一致，别改。
type memStatus struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memoryStatus(m *memStatus) error {
	m.Length = uint32(unsafe.Sizeof(*m))
	r, _, e := procMemoryStatus.Call(uintptr(unsafe.Pointer(m)))
	if r == 0 {
		return e
	}
	return nil
}

func tickCount64() (uint64, error) {
	r, _, e := procTickCount64.Call()
	if r == 0 {
		return 0, e
	}
	return uint64(r), nil
}

// nicCount 是上一轮的网卡累计值，用来算速率。
type nicCount struct{ rx, tx uint64 }

// ifRows 列出在用的网卡，并给出总收发字节。
// 回环和隧道网卡跳过：隧道（Tailscale 那种）走的是同一份流量，算进来就翻倍。
func ifRows() ([]NIC, uint64, uint64) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil {
		return nil, 0, 0
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var out []NIC
	var rx, tx uint64
	for _, row := range unsafe.Slice(&table.Table[0], int(table.NumEntries)) {
		if row.OperStatus != windows.IfOperStatusUp {
			continue
		}
		if row.Type == ifTypeLoopback || row.TunnelType != 0 {
			continue
		}
		if row.Type == ifTypePropVirtual || row.Type == ifTypeTunnel {
			continue
		}
		if row.InterfaceAndOperStatusFlags&ifFlagFilter != 0 {
			continue
		}
		// Wi-Fi Direct 那种虚拟适配器链路速度是 0，没有真实流量
		if row.ReceiveLinkSpeed == 0 {
			continue
		}
		name := windows.UTF16ToString(row.Alias[:])
		if name == "" {
			name = windows.UTF16ToString(row.Description[:])
		}
		rx += row.InOctets
		tx += row.OutOctets
		out = append(out, NIC{
			Index: row.InterfaceIndex, Name: name,
			InOctets: row.InOctets, OutOctets: row.OutOctets,
			Speed: row.ReceiveLinkSpeed,
		})
	}
	return out, rx, tx
}

// netTotalBytes 是开机以来的总收发字节。
func netTotalBytes() (rx, tx uint64) {
	_, rx, tx = ifRows()
	return rx, tx
}

// disks 列出有盘符的本地磁盘。网络盘不碰：断开的映射盘会让调用卡很久。
func disks() []Disk {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	var out []Disk
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + ":\\"
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if windows.GetDriveType(p) != driveFixed {
			continue
		}
		var freeToCaller, total, free uint64
		if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &free); err != nil || total == 0 {
			continue
		}
		out = append(out, Disk{Name: strings.TrimSuffix(root, "\\"), Total: total, Free: free})
	}
	return out
}

// ---------- 磁盘 IO（性能计数器）----------
//
// 进程 IO 计数里混着网络和设备 IO，拿它当"磁盘读写"会虚高，
// 所以走 PDH 的 PhysicalDisk 计数器，也就是性能监视器用的那套。

type pdhDisk struct {
	query   uintptr
	readC   uintptr
	writeC  uintptr
	tried   bool
	primed  bool
	lastErr string
}

type pdhFmtValue struct {
	Status uint32
	_      uint32
	Value  float64
}

func (p *pdhDisk) open() bool {
	if p.tried {
		return p.query != 0
	}
	p.tried = true
	var q uintptr
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		p.lastErr = "性能计数器打不开"
		return false
	}
	readPath, _ := windows.UTF16PtrFromString(`\PhysicalDisk(_Total)\Disk Read Bytes/sec`)
	writePath, _ := windows.UTF16PtrFromString(`\PhysicalDisk(_Total)\Disk Write Bytes/sec`)
	var rd, wr uintptr
	if r, _, _ := procPdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(readPath)), 0, uintptr(unsafe.Pointer(&rd))); r != 0 {
		procPdhCloseQuery.Call(q)
		p.lastErr = "这台机器没有磁盘计数器"
		return false
	}
	if r, _, _ := procPdhAddEnglishCounter.Call(q, uintptr(unsafe.Pointer(writePath)), 0, uintptr(unsafe.Pointer(&wr))); r != 0 {
		procPdhCloseQuery.Call(q)
		p.lastErr = "这台机器没有磁盘计数器"
		return false
	}
	p.query, p.readC, p.writeC = q, rd, wr
	return true
}

// read 返回每秒读写字节。速率计数器要两次采集才有值，
// 所以第一次（primed 之前）返回 0，界面看不出差别。
func (p *pdhDisk) read() (uint64, uint64, string) {
	if !p.open() {
		return 0, 0, p.lastErr
	}
	if r, _, _ := procPdhCollectQueryData.Call(p.query); r != 0 {
		return 0, 0, "磁盘计数器读取失败"
	}
	if !p.primed {
		p.primed = true
		return 0, 0, ""
	}
	return pdhValue(p.readC), pdhValue(p.writeC), ""
}

func pdhValue(counter uintptr) uint64 {
	var v pdhFmtValue
	r, _, _ := procPdhGetFmtCounter.Call(
		counter, pdhFmtDouble, 0, uintptr(unsafe.Pointer(&v)))
	if r != 0 || int32(v.Status) != 0 || v.Value <= 0 {
		return 0
	}
	return uint64(v.Value)
}

func (p *pdhDisk) close() {
	if p.query != 0 {
		procPdhCloseQuery.Call(p.query)
		p.query = 0
	}
}

// ---------- 进程 ----------

// processMemoryCounters 对应 PROCESS_MEMORY_COUNTERS，字段顺序同样不能改。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func readProcs() []Proc {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil
	}

	sum := map[string]*Proc{}
	for {
		// 0/4 是 Idle 和 System，没有工作集可看
		if entry.ProcessID > 4 {
			name := windows.UTF16ToString(entry.ExeFile[:])
			if name != "" {
				if mem := workingSet(uint32(entry.ProcessID)); mem > 0 {
					key := strings.ToLower(name)
					if cur, ok := sum[key]; ok {
						cur.Mem += mem
						cur.N++
					} else {
						sum[key] = &Proc{Name: name, Mem: mem, N: 1}
					}
				}
			}
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}

	list := make([]Proc, 0, len(sum))
	for _, p := range sum {
		list = append(list, *p)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Mem == list[j].Mem {
			return list[i].Name < list[j].Name
		}
		return list[i].Mem > list[j].Mem
	})
	if len(list) > procTopN {
		list = list[:procTopN]
	}
	return list
}

func workingSet(pid uint32) uint64 {
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0
	}
	defer windows.CloseHandle(h)
	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	r, _, _ := procProcessMemoryInfo.Call(
		uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if r == 0 {
		return 0
	}
	return uint64(pmc.WorkingSetSize)
}
