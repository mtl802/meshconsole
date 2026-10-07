// Package collect 基于 gopsutil 的系统指标采集。
//
// null 语义（SPEC §2/§3，DESIGN §4.1）：采集失败或尚未就绪的字段为 nil
// （JSON 序列化为 null），并在 Errors 里附带原因说明，禁止填 0。
package collect

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
)

// maxErrLen 限制单条错误说明长度（客户端上报字符串须有界）。
const maxErrLen = 200

// Snapshot 为一次心跳的指标快照。指针字段 nil = 不可用（上报 null）。
// JSON 结构对应 DESIGN §5 metrics 表字段。
type Snapshot struct {
	CPUPct    *float64          `json:"cpu_pct"`
	MemUsed   *int64            `json:"mem_used"`
	MemTotal  *int64            `json:"mem_total"`
	DiskUsed  *int64            `json:"disk_used"`
	DiskTotal *int64            `json:"disk_total"`
	NetRx     *int64            `json:"net_rx"`
	NetTx     *int64            `json:"net_tx"`
	UptimeS   *int64            `json:"uptime_s"`
	Load1     *float64          `json:"load1"`
	Errors    map[string]string `json:"collect_errors,omitempty"`
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// truncErr 截断并去除控制字符，保证错误说明有界可入库可入日志。
func truncErr(s string) string {
	var b []byte
	for _, r := range s {
		if r < 0x20 && r != '\t' || r == 0x7f {
			r = ' '
		}
		if len(b)+len(string(r)) > maxErrLen {
			break
		}
		b = append(b, string(r)...)
	}
	return string(b)
}

// Collector 周期采集系统指标。CPU 百分比需要采样窗口，异步在后台持续进行，
// Collect() 只取最近一次已完成窗口的结果，绝不阻塞心跳循环。
type Collector struct {
	mount       string
	cpuInterval time.Duration
	// staleAfter 超过该时长未产出新采样即视为 CPU 采样失效，
	// 上报 null+说明而非回填旧值（采样窗口的 2 倍 + 5s 余量）。
	staleAfter time.Duration

	cpuSample atomic.Pointer[cpuSample]
}

type cpuSample struct {
	pct float64
	at  time.Time
	// err 非空表示最近一次采样失败，Collect 据此报 null 而非沿用旧值。
	err string
}

// NewCollector 构造采集器；mount 为磁盘挂载点，cpuInterval 为 CPU 采样窗口
// （与心跳间隔一致即可）。
func NewCollector(mount string, cpuInterval time.Duration) *Collector {
	if cpuInterval <= 0 {
		cpuInterval = 15 * time.Second
	}
	return &Collector{
		mount:       mount,
		cpuInterval: cpuInterval,
		staleAfter:  2*cpuInterval + 5*time.Second,
	}
}

// Start 启动后台 CPU 采样循环，阻塞式采样由独立 goroutine 承担，随 ctx 退出。
func (c *Collector) Start(ctx context.Context) {
	go func() {
		for {
			pct, err := cpu.Percent(c.cpuInterval, false)
			if err != nil {
				// 记录失败状态：Collect 将报 null+说明，绝不回填旧值。
				c.cpuSample.Store(&cpuSample{err: truncErr(err.Error())})
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				continue
			}
			if len(pct) > 0 {
				c.cpuSample.Store(&cpuSample{pct: pct[0], at: time.Now()})
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
}

// collectCPU 依据最近一次采样状态决定 cpu_pct 值或错误说明：
// 未就绪 / 最近采样失败 / 采样过期 三种情形一律 null+说明，不回填旧值。
func (c *Collector) collectCPU(snap *Snapshot) {
	s := c.cpuSample.Load()
	switch {
	case s == nil:
		snap.Errors["cpu_pct"] = "waiting for first sampling window"
	case s.err != "":
		snap.Errors["cpu_pct"] = s.err
	case time.Since(s.at) > c.staleAfter:
		snap.Errors["cpu_pct"] = fmt.Sprintf("cpu sample stale (%ds old, threshold %ds)",
			int(time.Since(s.at).Seconds()), int(c.staleAfter.Seconds()))
	default:
		snap.CPUPct = f64(s.pct)
	}
}

// loadNotAvailable 返回平台对应的 load 不可用说明；windows 无 load 概念，
// gopsutil v4.26.9 在该平台返回模拟值而非 error，必须按平台显式拦截报 null（SPEC §3）。
func loadNotAvailable(goos string) string {
	if goos == "windows" {
		return "load average not available on windows"
	}
	return ""
}

// Collect 采集一次快照。任一子项失败只影响该字段（nil + Errors 说明），
// 不影响其他字段与其他采集项。
func (c *Collector) Collect() *Snapshot {
	snap := &Snapshot{Errors: map[string]string{}}

	c.collectCPU(snap)

	if vm, err := mem.VirtualMemory(); err != nil {
		snap.Errors["mem"] = truncErr(err.Error())
	} else {
		snap.MemUsed = i64(int64(vm.Used))
		snap.MemTotal = i64(int64(vm.Total))
	}

	if du, err := disk.Usage(c.mount); err != nil {
		snap.Errors["disk"] = truncErr(err.Error())
	} else {
		snap.DiskUsed = i64(int64(du.Used))
		snap.DiskTotal = i64(int64(du.Total))
	}

	// 累计 rx/tx 字节：全接口汇总。
	if io, err := gnet.IOCounters(false); err != nil {
		snap.Errors["net"] = truncErr(err.Error())
	} else if len(io) == 0 {
		snap.Errors["net"] = "no network interfaces reported"
	} else {
		snap.NetRx = i64(int64(io[0].BytesRecv))
		snap.NetTx = i64(int64(io[0].BytesSent))
	}

	if up, err := host.Uptime(); err != nil {
		snap.Errors["uptime_s"] = truncErr(err.Error())
	} else {
		snap.UptimeS = i64(int64(up))
	}

	// load 仅 linux/macOS 采集；Windows 按平台显式报 null+说明（不信任 gopsutil 的模拟值）。
	if msg := loadNotAvailable(runtime.GOOS); msg != "" {
		snap.Errors["load1"] = msg
	} else if avg, err := load.Avg(); err != nil {
		snap.Errors["load1"] = truncErr(err.Error())
	} else {
		snap.Load1 = f64(avg.Load1)
	}

	if len(snap.Errors) == 0 {
		snap.Errors = nil
	}
	return snap
}
