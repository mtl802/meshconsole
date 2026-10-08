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

// CommandLoop 为命令通道的后台巡检（SPEC-M1d §2/§5），两个节奏：
//   - 快节奏（sweep 间隔 = 离线巡检同级，默认 15s）：SweepUnknown——lease×2 或
//     running 后 timeout×1.5 到期无回执的 claimed/running 命令置 unknown +
//     degraded（非 failed，迟到回执可修正）；
//   - 慢节奏（每小时，随 metrics retention 模式）：离线节点 pending 24h 过期
//     未领 → archived；commands 表 90 天保留清理。
func CommandLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	log.Info("command loop started", "unknown_sweep", "15s", "archive_retention", "1h")
	runHourly := func() {
		now := time.Now().Unix()
		if n, err := st.ArchiveStalePending(ctx, now, now-24*3600); err != nil {
			if ctx.Err() == nil {
				log.Error("archive stale pending commands", "err", err)
			}
		} else if n > 0 {
			log.Info("stale pending commands archived", "count", n)
		}
		if n, err := st.CleanupCommands(ctx, now-90*24*3600); err != nil {
			if ctx.Err() == nil {
				log.Error("cleanup commands", "err", err)
			}
		} else if n > 0 {
			log.Info("commands retention cleanup", "deleted", n, "retention_days", 90)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := st.SweepUnknown(ctx, time.Now().Unix(), store.CommandLeaseS)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("sweep unknown commands", "err", err)
				continue
			}
			if n > 0 {
				log.Warn("commands marked unknown (no receipt)", "count", n)
			}
		case <-hourly.C:
			runHourly()
		}
	}
}
