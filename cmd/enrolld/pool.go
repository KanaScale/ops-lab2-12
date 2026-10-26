package main

import (
	"context"
	"errors"
	"time"

	"enroll-lab/internal/metrics"
)

var errPoolTimeout = errors.New("等数据库连接超时")

// pool 模拟数据库连接池：最多 size 个查询同时进行，每个查询占用一个连接 hold 这么久。
// 池满时请求在这里排队；等待超过 timeout 就放弃（L-06 第 17、19 页：有界等待 + 超时）。
type pool struct {
	slots   chan struct{}
	hold    time.Duration
	timeout time.Duration

	inUse *metrics.Gauge
	wait  *metrics.Histogram
	held  *metrics.Histogram
}

func newPool(reg *metrics.Registry, size int, hold, timeout time.Duration) *pool {
	reg.NewGauge("db_pool_size", "连接池大小").Set(float64(size))
	return &pool{
		slots:   make(chan struct{}, size),
		hold:    hold,
		timeout: timeout,
		inUse:   reg.NewGauge("db_pool_in_use", "正在被查询占用的连接数"),
		wait:    reg.NewHistogram("db_pool_wait_seconds", "等到一个空闲连接花的时间（含等待超时的请求）", metrics.LatencyBuckets),
		held:    reg.NewHistogram("db_query_duration_seconds", "拿到连接以后查询本身占用连接的时间", metrics.LatencyBuckets),
	}
}

func (p *pool) query(ctx context.Context) error {
	start := time.Now()
	t := time.NewTimer(p.timeout)
	defer t.Stop()
	select {
	case p.slots <- struct{}{}:
	case <-t.C:
		p.wait.Observe(time.Since(start).Seconds())
		return errPoolTimeout
	case <-ctx.Done():
		p.wait.Observe(time.Since(start).Seconds())
		return ctx.Err()
	}
	got := time.Now()
	p.wait.Observe(got.Sub(start).Seconds())
	p.inUse.Inc()
	time.Sleep(p.hold) // “数据库在执行”：占着连接，但不占本机 CPU
	p.inUse.Dec()
	<-p.slots
	p.held.Observe(time.Since(got).Seconds())
	return nil
}
