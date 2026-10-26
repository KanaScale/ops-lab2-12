package main

import (
	"math"
	"os"
	"runtime"
	rtmetrics "runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"enroll-lab/internal/metrics"
)

// registerProcessMetrics 注册进程和 Go 运行时的指标（L-06：一个服务在机器上是一个进程，占 CPU、内存、fd、线程）。
// 名字与 client_golang 的同名指标含义一致，读 /proc 失败时（比如不在 Linux 上）返回 0。
func registerProcessMetrics(reg *metrics.Registry) {
	reg.NewCounterFunc("process_cpu_seconds_total", "进程累计用掉的 CPU 时间（user + system，秒）", func() float64 {
		var ru syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
			return 0
		}
		return tvSeconds(ru.Utime) + tvSeconds(ru.Stime)
	})
	reg.NewGaugeFunc("process_resident_memory_bytes", "进程真正占用的物理内存 RSS（字节）", func() float64 {
		f := procFields("/proc/self/statm")
		if len(f) < 2 {
			return 0
		}
		pages, _ := strconv.ParseFloat(f[1], 64)
		return pages * float64(os.Getpagesize())
	})
	reg.NewGaugeFunc("process_open_fds", "打开的文件描述符数（含连接）", func() float64 {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return 0
		}
		return float64(len(ents))
	})
	reg.NewGaugeFunc("process_threads", "操作系统线程数", func() float64 {
		// /proc/self/stat 第 2 列是带括号的进程名，可能含空格，所以从右括号之后数：线程数是第 20 列。
		b, err := os.ReadFile("/proc/self/stat")
		if err != nil {
			return 0
		}
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) < 18 {
			return 0
		}
		n, _ := strconv.ParseFloat(f[17], 64)
		return n
	})
	reg.NewCounterFunc("process_cpu_wait_seconds_total",
		"进程的线程在运行队列里等 CPU 的累计时间（秒），取自各线程的 /proc/self/task/*/schedstat；它的速率是平均有几个线程在等核，是 CPU 饱和的直接证据",
		func() float64 {
			var total float64
			tasks, _ := os.ReadDir("/proc/self/task")
			for _, t := range tasks {
				f := procFields("/proc/self/task/" + t.Name() + "/schedstat")
				if len(f) >= 2 {
					ns, _ := strconv.ParseFloat(f[1], 64)
					total += ns / 1e9
				}
			}
			return total
		})
	reg.NewGaugeFunc("go_goroutines", "goroutine 数", func() float64 { return float64(runtime.NumGoroutine()) })
	reg.NewGaugeFunc("go_gomaxprocs", "Go 同时用来执行代码的线程数上限（受 taskset 和 cgroup 的 CPU 限制影响）",
		func() float64 { return float64(runtime.GOMAXPROCS(0)) })

	samples := []rtmetrics.Sample{
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/memory/classes/heap/objects:bytes"},
	}
	read := func(i int) float64 {
		s := []rtmetrics.Sample{samples[i]}
		rtmetrics.Read(s)
		switch s[0].Value.Kind() {
		case rtmetrics.KindUint64:
			return float64(s[0].Value.Uint64())
		case rtmetrics.KindFloat64:
			return s[0].Value.Float64()
		}
		return 0
	}
	reg.NewCounterFunc("go_gc_cycles_total", "完成的 GC 次数", func() float64 { return read(0) })
	reg.Add(newSchedLatency())
	reg.NewCounterFunc("go_gc_cpu_seconds_total", "GC 用掉的 CPU 时间估计（秒）", func() float64 { return read(1) })
	reg.NewGaugeFunc("go_heap_objects_bytes", "堆上对象占用的字节数（含尚未回收的）", func() float64 { return read(2) })
}

// schedBuckets 是 goroutine 调度延迟的桶上界（秒）。
var schedBuckets = []float64{1e-5, 5e-5, 1e-4, 5e-4, 0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1}

// newSchedLatency 把运行时的 /sched/latencies:seconds（goroutine 可以运行、却在等一个执行线程的时间）
// 换成 Prometheus 的直方图 go_sched_latencies_seconds。
//
// Go 只用 GOMAXPROCS 个线程执行全部 goroutine。CPU 不够时，排队的是 goroutine，队列在 Go 运行时里，
// 内核的运行队列（vmstat 的 r、PSI）几乎看不到它；这和 Tomcat 的请求在线程池前排队是一回事（L-06 第 17 页）。
func newSchedLatency() metrics.Family {
	return metrics.NewHistogramFunc("go_sched_latencies_seconds",
		"goroutine 可以运行但在等执行线程（等 CPU）的时间；运行时的桶比这里细，换算时整桶归入不小于它上沿的那个桶，_sum 按每个桶的下沿估计",
		schedBuckets, func() ([]uint64, float64, uint64) {
			s := []rtmetrics.Sample{{Name: "/sched/latencies:seconds"}}
			rtmetrics.Read(s)
			if s[0].Value.Kind() != rtmetrics.KindFloat64Histogram {
				return make([]uint64, len(schedBuckets)+1), 0, 0
			}
			h := s[0].Value.Float64Histogram()
			counts := make([]uint64, len(schedBuckets)+1)
			var sum float64
			var total uint64
			for i, n := range h.Counts {
				if n == 0 {
					continue
				}
				lo, hi := h.Buckets[i], h.Buckets[i+1]
				j, _ := slices.BinarySearch(schedBuckets, hi)
				counts[j] += n
				if !math.IsInf(lo, -1) {
					sum += lo * float64(n)
				}
				total += n
			}
			return counts, sum, total
		})
}

func tvSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}

func procFields(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}
