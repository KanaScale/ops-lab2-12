package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"enroll-lab/internal/metrics"
)

// commitLog 是写路径的“提交日志”，相当于数据库的 redo log（L-07 第 14、15 页）。
// Commit 返回时，这条记录已经按所选的方式处理完：each 和 group 下已经 fsync 落盘，interval 下还在页缓存里。
type commitLog interface {
	Commit(rec []byte) error
	Close() error
}

// FsyncBuckets 是 fsync 耗时的桶（秒）：本地 NVMe 约 0.1 ms，云盘 1～3 ms，机械盘约 10 ms（L-07 第 7 页）。
var FsyncBuckets = []float64{0.0001, 0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5}

// BatchBuckets 是“一次 fsync 带走几条记录”的桶。
var BatchBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024}

type logMetrics struct {
	fsync *metrics.Histogram
	batch *metrics.Histogram
}

func newLogMetrics(reg *metrics.Registry) *logMetrics {
	return &logMetrics{
		fsync: reg.NewHistogram("enroll_fsync_duration_seconds", "每次 fsync 的耗时（应用自己量的）", FsyncBuckets),
		batch: reg.NewHistogram("enroll_commit_batch_size", "每次 fsync 落盘的记录条数", BatchBuckets),
	}
}

func (m *logMetrics) sync(f *os.File, records int) error {
	start := time.Now()
	err := f.Sync() // fsync(2)：等设备确认数据写到了持久介质
	m.fsync.Observe(time.Since(start).Seconds())
	m.batch.Observe(float64(records))
	return err
}

func openCommitLog(mode, dir string, interval time.Duration, m *logMetrics) (commitLog, error) {
	f, err := os.OpenFile(filepath.Join(dir, "enroll.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "each":
		return &eachLog{f: f, m: m}, nil
	case "group":
		g := &groupLog{f: f, m: m, ch: make(chan *pending, 4096), quit: make(chan struct{}), done: make(chan struct{})}
		go g.loop()
		return g, nil
	case "interval":
		l := &intervalLog{f: f, m: m, stop: make(chan struct{}), done: make(chan struct{})}
		go l.loop(interval)
		return l, nil
	}
	f.Close()
	return nil, fmt.Errorf("-sync 只能是 each、group 或 interval，不是 %q", mode)
}

// eachLog：每笔提交单独 write + fsync，并且一次只能有一笔在做。
// 锁相当于热点行上的锁：锁持有的时间里有一段在等磁盘（L-07 第 25 页）。
type eachLog struct {
	mu sync.Mutex
	f  *os.File
	m  *logMetrics
}

func (l *eachLog) Commit(rec []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(rec); err != nil {
		return err
	}
	return l.m.sync(l.f, 1)
}

func (l *eachLog) Close() error { return l.f.Close() }

// groupLog：组提交。一个后台协程负责落盘：取走此刻排队的全部记录，一次写入、一次 fsync，
// 再逐个通知。每条记录在返回前仍然落了盘，持久性不变；排队越多，一次 fsync 带走的越多。
type groupLog struct {
	f    *os.File
	m    *logMetrics
	ch   chan *pending
	quit chan struct{}
	done chan struct{}
	once sync.Once
	buf  []byte
}

type pending struct {
	rec  []byte
	errc chan error
}

const maxBatch = 1024

var errClosed = errors.New("日志已关闭")

func (g *groupLog) Commit(rec []byte) error {
	p := &pending{rec: rec, errc: make(chan error, 1)}
	select {
	case g.ch <- p:
	case <-g.quit:
		return errClosed
	}
	return <-p.errc
}

func (g *groupLog) loop() {
	defer close(g.done)
	for {
		select {
		case first := <-g.ch:
			g.commitBatch(first)
		case <-g.quit:
			for { // 退出前把已经排上队的做完
				select {
				case first := <-g.ch:
					g.commitBatch(first)
				default:
					return
				}
			}
		}
	}
}

func (g *groupLog) commitBatch(first *pending) {
	batch := []*pending{first}
	for len(batch) < maxBatch {
		select {
		case p := <-g.ch:
			batch = append(batch, p)
			continue
		default:
		}
		break
	}
	g.buf = g.buf[:0]
	for _, p := range batch {
		g.buf = append(g.buf, p.rec...)
	}
	_, err := g.f.Write(g.buf)
	if err == nil {
		err = g.m.sync(g.f, len(batch))
	}
	for _, p := range batch {
		p.errc <- err
	}
}

func (g *groupLog) Close() error {
	var err error
	g.once.Do(func() {
		close(g.quit)
		<-g.done
		err = g.f.Close()
	})
	return err
}

// intervalLog：只 write 到页缓存就返回，后台每隔 interval 统一 fsync 一次。
// 类似 MySQL 的 innodb_flush_log_at_trx_commit=2：快，但机器掉电时会丢掉最近一个间隔内已经“选上”的记录。
type intervalLog struct {
	mu      sync.Mutex
	f       *os.File
	m       *logMetrics
	pending int
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (l *intervalLog) Commit(rec []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(rec); err != nil {
		return err
	}
	l.pending++
	return nil
}

func (l *intervalLog) flush() {
	l.mu.Lock()
	n := l.pending
	l.pending = 0
	l.mu.Unlock()
	if n > 0 {
		if err := l.m.sync(l.f, n); err != nil {
			log.Printf("fsync 失败：%v", err)
		}
	}
}

func (l *intervalLog) loop(interval time.Duration) {
	defer close(l.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.flush()
		case <-l.stop:
			l.flush()
			return
		}
	}
}

func (l *intervalLog) Close() error {
	var err error
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		err = l.f.Close()
	})
	return err
}

// benchFsync 在 dir 里重复 n 次“追加一条约 100 字节的记录 + fsync”，和写路径做的事一样，打印耗时分布。
func benchFsync(dir string, n int, out io.Writer) error {
	path := filepath.Join(dir, "fsync-bench.tmp")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(path)
	defer f.Close()
	rec := []byte(`{"seq":0,"t":1760000000000000000,"student":"s0000001","course":"c1","pad":"................"}` + "\n")
	ds := make([]float64, 0, n)
	begin := time.Now()
	for i := 0; i < n; i++ {
		if _, err := f.Write(rec); err != nil {
			return err
		}
		t := time.Now()
		if err := f.Sync(); err != nil {
			return err
		}
		ds = append(ds, float64(time.Since(t))/float64(time.Millisecond))
	}
	total := time.Since(begin)
	slices.Sort(ds)
	var sum float64
	for _, d := range ds {
		sum += d
	}
	mean := sum / float64(n)
	q := func(p float64) float64 { return ds[nearestRank(p, n)] }
	fmt.Fprintf(out, "fsync 次数 %d，总耗时 %.2f s，数据目录 %s\n", n, total.Seconds(), dir)
	fmt.Fprintf(out, "fsync 耗时（毫秒）：mean %.3f  p50 %.3f  p90 %.3f  p99 %.3f  max %.3f\n",
		mean, q(50), q(90), q(99), ds[n-1])
	fmt.Fprintf(out, "按平均值推算，一个写者串行 fsync 每秒最多约 %.0f 次（= 1000 / %.3f）\n", 1000/mean, mean)
	return nil
}

// nearestRank 返回第 p 百分位在已排序的 n 个数里的下标（最近秩法：第 ceil(p/100 × n) 个）。
func nearestRank(p float64, n int) int {
	k := int(p / 100 * float64(n))
	if float64(k) < p/100*float64(n) {
		k++
	}
	return max(0, min(k-1, n-1))
}

// warnIfTmpfs：tmpfs 在内存里，fsync 立即返回，写路径的实验就没有意义了。
func warnIfTmpfs(dir string) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil && st.Type == 0x01021994 {
		log.Printf("警告：%s 在 tmpfs（内存）上，fsync 不会写盘。请用 -data 指到真实磁盘上的目录", dir)
	}
}
