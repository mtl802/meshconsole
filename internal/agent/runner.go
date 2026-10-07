package agent

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/mtl802/meshconsole/internal/agent/collect"
	"github.com/mtl802/meshconsole/internal/config"
)

const (
	backoffBase = time.Second
	backoffMax  = 60 * time.Second
)

// Runner 前台心跳循环：每 CollectIntervalS 采集上报一次；
// 上报失败按指数退避重试（1s→2s→…→60s 封顶，带抖动），成功后回到正常间隔。
type Runner struct {
	Cfg       *config.Agent
	State     *State
	Version   string
	Log       *slog.Logger
	Collector *collect.Collector
}

// Run 阻塞运行直至 ctx 取消。
func (r *Runner) Run(ctx context.Context) error {
	// CPU 采样异步进行，不阻塞心跳循环（SPEC §2）。
	r.Collector.Start(ctx)

	client := &http.Client{Timeout: httpTimeout}
	interval := time.Duration(r.Cfg.CollectIntervalS) * time.Second
	backoff := time.Duration(0)
	beat := 0

	for {
		err := reportOnce(ctx, client, r.State.ConsoleURL, r.State.NodeToken, r.State.Name, r.Version, r.Collector)
		var next time.Duration
		if err == nil {
			beat++
			if backoff != 0 {
				r.Log.Info("report recovered, resuming normal interval", "interval", interval.String())
			}
			backoff = 0
			next = interval
		} else {
			if backoff == 0 {
				backoff = backoffBase
			} else {
				backoff *= 2
				if backoff > backoffMax {
					backoff = backoffMax
				}
			}
			next = jitter(backoff)
			if next > backoffMax {
				next = backoffMax // 抖动封顶：重试间隔永不超 60s（审查 R1-#10）
			}
			r.Log.Error("report failed, backing off", "err", err, "retry_in", next.String())
		}

		select {
		case <-ctx.Done():
			r.Log.Info("agent stopping", "heartbeats_sent", beat)
			return nil
		case <-time.After(next):
		}
	}
}

// jitter 返回 [0.5d, d) 的随机抖动时长（上限即 d 本身，封顶后不超 backoffMax）。
func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Float64()*float64(d/2))
}
