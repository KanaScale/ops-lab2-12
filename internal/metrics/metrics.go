// Package metrics 只用标准库输出 Prometheus 文本格式（0.0.4）的指标。
//
// 实际项目一般用官方的 client_golang；这里手写，一是示例不引入第三方依赖，
// 二是让你们能读到 /metrics 背后到底是什么：counter 是只增的计数，gauge 是当前值，
// histogram 是一组按上界累计的桶计数加上 _sum 和 _count（L-08 第 10、25 页）。
package metrics

import (
	"bufio"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// LatencyBuckets 是请求延迟用的桶上界（秒）。包含 20 ms、50 ms、200 ms、1 s 这几个常用的 SLO 阈值，
// 这样“延迟不超过阈值的请求占比”可以直接从对应的桶读出来，不用插值。
var LatencyBuckets = []float64{0.001, 0.002, 0.005, 0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5}

// Registry 保存所有指标，按注册顺序输出。
type Registry struct {
	mu   sync.Mutex
	fams []Family
}

// Family 是一组同名的指标（含 # HELP、# TYPE 两行）。
type Family interface {
	write(w *bufio.Writer)
}

func NewRegistry() *Registry { return &Registry{} }

// Add 注册一组由本包构造的指标，例如 NewHistogramFunc 的返回值。
func (r *Registry) Add(f Family) { r.add(f) }

func (r *Registry) add(f Family) {
	r.mu.Lock()
	r.fams = append(r.fams, f)
	r.mu.Unlock()
}

// Write 按文本格式输出全部指标。
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	fams := slices.Clone(r.fams)
	r.mu.Unlock()
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		f.write(bw)
	}
	return bw.Flush()
}

// Handler 是 GET /metrics 的处理函数。Content-Type 必须写对：Prometheus 3 遇到无法识别的类型会拒绝这次抓取。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.Write(w)
	})
}

// ---- counter ----

// Counter 只增不减。进程重启后从 0 开始，Prometheus 的 rate() 会把这种归零当作重置处理。
type Counter struct{ n atomic.Uint64 }

func (c *Counter) Inc()          { c.n.Add(1) }
func (c *Counter) Add(d uint64)  { c.n.Add(d) }
func (c *Counter) Value() uint64 { return c.n.Load() }

type CounterVec struct{ v *vec[Counter] }

func (r *Registry) NewCounterVec(name, help string, labels ...string) *CounterVec {
	v := newVec(name, help, "counter", labels, func() *Counter { return new(Counter) },
		func(bw *bufio.Writer, name, lbl string, c *Counter) {
			writeSample(bw, name, lbl, float64(c.Value()))
		})
	r.add(v)
	return &CounterVec{v}
}

func (r *Registry) NewCounter(name, help string) *Counter { return r.NewCounterVec(name, help).With() }

func (c *CounterVec) With(values ...string) *Counter { return c.v.with(values) }

// ---- gauge ----

// Gauge 是可增可减的当前值，例如在途请求数、连接池占用数。
type Gauge struct{ bits atomic.Uint64 }

func (g *Gauge) Set(v float64)  { g.bits.Store(math.Float64bits(v)) }
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }
func (g *Gauge) Inc()           { g.Add(1) }
func (g *Gauge) Dec()           { g.Add(-1) }

func (g *Gauge) Add(d float64) {
	for {
		old := g.bits.Load()
		if g.bits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+d)) {
			return
		}
	}
}

type GaugeVec struct{ v *vec[Gauge] }

func (r *Registry) NewGaugeVec(name, help string, labels ...string) *GaugeVec {
	v := newVec(name, help, "gauge", labels, func() *Gauge { return new(Gauge) },
		func(bw *bufio.Writer, name, lbl string, g *Gauge) {
			writeSample(bw, name, lbl, g.Value())
		})
	r.add(v)
	return &GaugeVec{v}
}

func (r *Registry) NewGauge(name, help string) *Gauge { return r.NewGaugeVec(name, help).With() }

func (g *GaugeVec) With(values ...string) *Gauge { return g.v.with(values) }

// ---- 抓取时才计算的值 ----

type funcFamily struct {
	name, help, typ string
	f               func() float64
}

func (f *funcFamily) write(bw *bufio.Writer) {
	writeHeader(bw, f.name, f.help, f.typ)
	writeSample(bw, f.name, "", f.f())
}

// NewCounterFunc 注册一个在每次抓取时调用 f 得到当前累计值的 counter，例如进程累计用掉的 CPU 秒数。
func (r *Registry) NewCounterFunc(name, help string, f func() float64) {
	r.add(&funcFamily{name, help, "counter", f})
}

// NewGaugeFunc 注册一个在每次抓取时调用 f 得到当前值的 gauge，例如 RSS、goroutine 数。
func (r *Registry) NewGaugeFunc(name, help string, f func() float64) {
	r.add(&funcFamily{name, help, "gauge", f})
}

// ---- histogram ----

// Histogram 把观测值分桶计数。桶的含义是“≤ 上界”（le = less or equal），输出时按上界累计；
// 另有 _sum（观测值之和）和 _count（观测次数），二者相除就是平均值。
type Histogram struct {
	upper  []float64
	mu     sync.Mutex
	counts []uint64 // 不累计；最后一格是超过所有上界的（+Inf）
	sum    float64
	count  uint64
}

func newHistogram(upper []float64) *Histogram {
	if !slices.IsSorted(upper) {
		panic("metrics: 桶上界必须递增")
	}
	return &Histogram{upper: upper, counts: make([]uint64, len(upper)+1)}
}

func (h *Histogram) Observe(v float64) {
	i, _ := slices.BinarySearch(h.upper, v) // 第一个 ≥ v 的上界；等于上界的值落在这个桶里
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.count++
	h.mu.Unlock()
}

func (h *Histogram) snapshot() (counts []uint64, sum float64, count uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.counts), h.sum, h.count
}

// NewHistogramFunc 构造一个在每次抓取时调用 f 取得桶计数的直方图。f 返回不累计的各桶计数
// （长度为 len(buckets)+1，最后一格是超过所有上界的）、观测值之和与总次数。用 Registry.Add 注册。
func NewHistogramFunc(name, help string, buckets []float64, f func() (counts []uint64, sum float64, count uint64)) Family {
	return &histFunc{name: name, help: help, upper: slices.Clone(buckets), f: f}
}

type histFunc struct {
	name, help string
	upper      []float64
	f          func() ([]uint64, float64, uint64)
}

func (h *histFunc) write(bw *bufio.Writer) {
	counts, sum, count := h.f()
	writeHeader(bw, h.name, h.help, "histogram")
	writeBuckets(bw, h.name, "", h.upper, counts, sum, count)
}

func writeBuckets(bw *bufio.Writer, name, lbl string, upper []float64, counts []uint64, sum float64, count uint64) {
	var cum uint64
	for i, le := range upper {
		cum += counts[i]
		writeSample(bw, name+"_bucket", joinLabels(lbl, `le="`+formatFloat(le)+`"`), float64(cum))
	}
	writeSample(bw, name+"_bucket", joinLabels(lbl, `le="+Inf"`), float64(count))
	writeSample(bw, name+"_sum", lbl, sum)
	writeSample(bw, name+"_count", lbl, float64(count))
}

type HistogramVec struct{ v *vec[Histogram] }

func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...string) *HistogramVec {
	upper := slices.Clone(buckets)
	v := newVec(name, help, "histogram", labels, func() *Histogram { return newHistogram(upper) },
		func(bw *bufio.Writer, name, lbl string, h *Histogram) {
			counts, sum, count := h.snapshot()
			writeBuckets(bw, name, lbl, h.upper, counts, sum, count)
		})
	r.add(v)
	return &HistogramVec{v}
}

func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	return r.NewHistogramVec(name, help, buckets).With()
}

func (h *HistogramVec) With(values ...string) *Histogram { return h.v.with(values) }

// ---- 带标签的一组指标 ----

type vec[T any] struct {
	name, help, typ string
	labels          []string
	newChild        func() *T
	writeChild      func(bw *bufio.Writer, name, lbl string, c *T)

	mu       sync.Mutex
	children map[string]*child[T]
}

type child[T any] struct {
	lbl string // 已经格式化好的 k="v",k="v"
	c   *T
}

func newVec[T any](name, help, typ string, labels []string, newChild func() *T,
	writeChild func(*bufio.Writer, string, string, *T)) *vec[T] {
	return &vec[T]{name: name, help: help, typ: typ, labels: slices.Clone(labels),
		newChild: newChild, writeChild: writeChild, children: map[string]*child[T]{}}
}

// with 返回这组标签值对应的那个指标，第一次用到时创建。
func (v *vec[T]) with(values []string) *T {
	if len(values) != len(v.labels) {
		panic("metrics: " + v.name + " 需要 " + strconv.Itoa(len(v.labels)) + " 个标签值")
	}
	key := strings.Join(values, "\xff")
	v.mu.Lock()
	defer v.mu.Unlock()
	if ch, ok := v.children[key]; ok {
		return ch.c
	}
	parts := make([]string, len(values))
	for i, val := range values {
		parts[i] = v.labels[i] + `="` + escapeLabel(val) + `"`
	}
	ch := &child[T]{lbl: strings.Join(parts, ","), c: v.newChild()}
	v.children[key] = ch
	return ch.c
}

func (v *vec[T]) write(bw *bufio.Writer) {
	v.mu.Lock()
	children := make([]*child[T], 0, len(v.children))
	for _, ch := range v.children {
		children = append(children, ch)
	}
	v.mu.Unlock()
	if len(children) == 0 && len(v.labels) > 0 {
		return // 还没有任何一组标签值被用到
	}
	slices.SortFunc(children, func(a, b *child[T]) int { return strings.Compare(a.lbl, b.lbl) })
	writeHeader(bw, v.name, v.help, v.typ)
	for _, ch := range children {
		v.writeChild(bw, v.name, ch.lbl, ch.c)
	}
}

// ---- 文本格式 ----

func writeHeader(bw *bufio.Writer, name, help, typ string) {
	bw.WriteString("# HELP " + name + " " + escapeHelp(help) + "\n")
	bw.WriteString("# TYPE " + name + " " + typ + "\n")
}

func writeSample(bw *bufio.Writer, name, lbl string, v float64) {
	bw.WriteString(name)
	if lbl != "" {
		bw.WriteString("{" + lbl + "}")
	}
	bw.WriteString(" " + formatFloat(v) + "\n")
}

func joinLabels(a, b string) string {
	if a == "" {
		return b
	}
	return a + "," + b
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
