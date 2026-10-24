package sysmon

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 这台机器上 Win32 那几个调用是不是真的读到了东西。
// 读不到的字段是 0，所以每项都断言一下。
func TestSnapshot(t *testing.T) {
	c := New()
	c.sample()
	time.Sleep(400 * time.Millisecond)
	c.sample()
	s := c.Snapshot()

	t.Logf("host=%s cores=%d uptime=%ds cpu=%.1f%% mem=%d/%d disk=%d proc=%d hist=%d rxTotal=%d txTotal=%d",
		s.Host, s.Cores, s.Uptime, s.CPU, s.MemUsed, s.MemTotal,
		len(s.Disks), len(s.Procs), len(s.History), s.NetRxTotal, s.NetTxTotal)
	t.Logf("perCore=%d %v", len(s.PerCore), roundAll(s.PerCore))
	t.Logf("io read=%d write=%d note=%q", s.IORead, s.IOWrite, s.IONote)
	for _, n := range s.NICs {
		t.Logf("nic %-10s rx=%d tx=%d speed=%d", n.Name, n.Rx, n.Tx, n.Speed)
	}
	for _, g := range s.GPUs {
		t.Logf("gpu %s util=%.0f%% mem=%d/%d temp=%.0f", g.Name, g.Util, g.MemUsed, g.MemTotal, g.Temp)
	}
	if s.GPUNote != "" {
		t.Logf("gpu note: %s", s.GPUNote)
	}
	for _, d := range s.Disks {
		t.Logf("disk %s total=%d free=%d", d.Name, d.Total, d.Free)
	}
	for _, p := range s.Procs {
		t.Logf("proc %s x%d mem=%d", p.Name, p.N, p.Mem)
	}

	switch {
	case s.Host == "":
		t.Error("机器名读不到")
	case s.Cores == 0:
		t.Error("CPU 核数读不到")
	case s.MemTotal == 0:
		t.Error("内存总量读不到")
	case s.Uptime == 0:
		t.Error("开机时长读不到")
	case len(s.Disks) == 0:
		t.Error("磁盘读不到")
	case len(s.Procs) == 0:
		t.Error("进程列表读不到")
	case len(s.History) < 1:
		t.Error("历史采样为空")
	case len(s.PerCore) == 0:
		t.Error("每核占用读不到")
	case len(s.NICs) == 0:
		t.Error("网卡列表读不到")
	}
}

func roundAll(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		out = append(out, float64(int(v*10))/10)
	}
	return out
}

// 把系统报的每一张网卡都打出来（含被过滤掉的伪接口），
// 换机器时看这个就知道过滤规则要不要调。
func TestListAllInterfaces(t *testing.T) {
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil {
		t.Fatalf("GetIfTable2Ex: %v", err)
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	for _, row := range unsafe.Slice(&table.Table[0], int(table.NumEntries)) {
		name := windows.UTF16ToString(row.Alias[:])
		t.Logf("%-46s op=%d flags=%#02x type=%d tunnel=%d speed=%d rx=%d",
			name, row.OperStatus, row.InterfaceAndOperStatusFlags,
			row.Type, row.TunnelType, row.ReceiveLinkSpeed, row.InOctets)
	}
}
