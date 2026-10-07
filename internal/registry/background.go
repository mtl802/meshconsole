package registry

import (
	"context"
	"log/slog"
	"time"

	"github.com/mtl802/meshconsole/internal/store"
)

// SweepLoop 周期巡检：last_seen 超过 offlineAfter 的在线节点标记 offline
// （DESIGN §4.1：60s 无心跳判定离线）。巡检间隔取 offlineAfter 的 1/4，下限 2s。
func SweepLoop(ctx context.Context, st *store.Store, log *slog.Logger, offlineAfter time.Duration) {
	interval := offlineAfter / 4
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	log.Info("offline sweeper started", "interval", interval.String(), "offline_after", offlineAfter.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := st.SweepOffline(ctx, time.Now().Add(-offlineAfter).Unix())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("sweep offline", "err", err)
			continue
		}
		if n > 0 {
			log.Warn("nodes marked offline", "count", n)
		}
	}
}

// CleanupLoop 每小时清理超过保留期（默认 7 天）的 metrics 行；启动时先执行一次。
func CleanupLoop(ctx context.Context, st *store.Store, log *slog.Logger, retentionDays int) {
	retention := time.Duration(retentionDays) * 24 * time.Hour
	run := func() {
		n, err := st.CleanupMetrics(ctx, time.Now().Add(-retention).Unix())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("cleanup metrics", "err", err)
			return
		}
		if n > 0 {
			log.Info("metrics retention cleanup", "deleted", n, "retention_days", retentionDays)
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	log.Info("metrics retention loop started", "interval", "1h", "retention_days", retentionDays)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}
