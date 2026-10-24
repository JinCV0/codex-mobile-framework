package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"codexmobile/internal/sysmon"
)

// /api/sysinfo 是手机看电脑状态的唯一入口，这里连着网页一起过一遍：
// 接口要真的量出数字，首页里要有那个入口。
func TestSysinfoAndServicesTile(t *testing.T) {
	mon := sysmon.New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mon.Run(ctx)

	srv := &Server{Token: "tk", Web: os.DirFS("../../web"), Mon: mon}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	get := func(path string) []byte {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
		raw, _ := io.ReadAll(resp.Body)
		return raw
	}

	var snap sysmon.Snapshot
	if err := json.Unmarshal(get("/api/sysinfo?k=tk"), &snap); err != nil {
		t.Fatalf("解析 sysinfo 失败: %v", err)
	}
	t.Logf("host=%s cores=%d mem=%d/%d disks=%d procs=%d hist=%d",
		snap.Host, snap.Cores, snap.MemUsed, snap.MemTotal, len(snap.Disks), len(snap.Procs), len(snap.History))
	switch {
	case snap.Host == "":
		t.Error("机器名为空")
	case snap.Cores == 0 || snap.MemTotal == 0:
		t.Error("CPU/内存读数不完整")
	case len(snap.Disks) == 0:
		t.Error("没有读到磁盘")
	case len(snap.Procs) == 0:
		t.Error("没有读到进程")
	case len(snap.PerCore) == 0:
		t.Error("没有读到每核占用")
	case len(snap.NICs) == 0:
		t.Error("没有读到网卡")
	}

	// 口令不对时不该漏数据
	if resp, err := http.Get(ts.URL + "/api/sysinfo?k=wrong"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("口令不对也放行了")
		}
	}

	page := string(get("/?k=tk"))
	for _, want := range []string{"电脑状态", "mon-open", "sysinfo", "chart.umd.min.js", "cpu-cores"} {
		if !strings.Contains(page, want) {
			t.Errorf("首页里没有 %q", want)
		}
	}

	// 页面会去拉这个脚本，口令不对或者路径不对都会 403/404，图表就退化成纯数字
	if js := get("/chart.umd.min.js?k=tk&v=4.5.1"); !bytes.Contains(js, []byte("Chart.js")) {
		t.Error("图表库没能发到手机上")
	}
}
