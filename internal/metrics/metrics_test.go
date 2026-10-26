package metrics

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestCounterWithLabels(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("http_requests_total", "请求数", "route", "code")
	c.With("/api/x", "200").Add(3)
	c.With("/api/x", "503").Inc()
	got := render(t, r)
	want := `# HELP http_requests_total 请求数
# TYPE http_requests_total counter
http_requests_total{route="/api/x",code="200"} 3
http_requests_total{route="/api/x",code="503"} 1
`
	if got != want {
		t.Fatalf("输出不符：\n%s\n期望：\n%s", got, want)
	}
}

func TestVecWithoutChildrenWritesNothing(t *testing.T) {
	r := NewRegistry()
	r.NewCounterVec("unused_total", "没用到", "route")
	if got := render(t, r); got != "" {
		t.Fatalf("没有任何标签值时不应输出，得到：\n%s", got)
	}
}

func TestHistogramIsCumulativeAndInclusive(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("lat_seconds", "延迟", []float64{0.01, 0.05, 0.1})
	for _, v := range []float64{0.005, 0.01, 0.02, 0.05, 0.2} {
		h.Observe(v)
	}
	got := render(t, r)
	// 0.01 和 0.05 恰好等于上界，应当落在 le="0.01"、le="0.05" 里（le 是“小于等于”）。
	for _, line := range []string{
		`lat_seconds_bucket{le="0.01"} 2`,
		`lat_seconds_bucket{le="0.05"} 4`,
		`lat_seconds_bucket{le="0.1"} 4`,
		`lat_seconds_bucket{le="+Inf"} 5`,
		`lat_seconds_sum 0.28500000000000003`, // 0.005+0.01+0.02+0.05+0.2 的浮点和
		`lat_seconds_count 5`,
		`# TYPE lat_seconds histogram`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("缺少 %q，实际输出：\n%s", line, got)
		}
	}
}

func TestHistogramLabelsComeBeforeLe(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogramVec("d_seconds", "x", []float64{1}, "route")
	h.With("/a").Observe(0.5)
	got := render(t, r)
	if !strings.Contains(got, `d_seconds_bucket{route="/a",le="1"} 1`+"\n") {
		t.Fatalf("标签顺序或格式不对：\n%s", got)
	}
	if !strings.Contains(got, `d_seconds_sum{route="/a"} 0.5`+"\n") {
		t.Fatalf("_sum 缺少标签：\n%s", got)
	}
}

func TestHistogramFunc(t *testing.T) {
	r := NewRegistry()
	r.Add(NewHistogramFunc("sched_seconds", "调度", []float64{0.001, 0.01}, func() ([]uint64, float64, uint64) {
		return []uint64{3, 1, 2}, 0.5, 6
	}))
	got := render(t, r)
	for _, line := range []string{`sched_seconds_bucket{le="0.001"} 3`, `sched_seconds_bucket{le="0.01"} 4`,
		`sched_seconds_bucket{le="+Inf"} 6`, "sched_seconds_sum 0.5", "sched_seconds_count 6"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("缺少 %q：\n%s", line, got)
		}
	}
}

func TestGaugeAndFuncs(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("inflight", "在途")
	g.Inc()
	g.Inc()
	g.Dec()
	r.NewGaugeFunc("answer", "常数", func() float64 { return 42 })
	r.NewCounterFunc("cpu_seconds_total", "CPU", func() float64 { return 1.5 })
	got := render(t, r)
	for _, line := range []string{"inflight 1", "answer 42", "cpu_seconds_total 1.5", "# TYPE cpu_seconds_total counter"} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("缺少 %q：\n%s", line, got)
		}
	}
}

func TestEscaping(t *testing.T) {
	r := NewRegistry()
	r.NewCounterVec("x_total", "第一行\n第二行", "v").With(`a"b\c`).Inc()
	got := render(t, r)
	if !strings.Contains(got, `# HELP x_total 第一行\n第二行`+"\n") {
		t.Errorf("HELP 没有转义换行：\n%s", got)
	}
	if !strings.Contains(got, `x_total{v="a\"b\\c"} 1`+"\n") {
		t.Errorf("标签值没有转义：\n%s", got)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("n_total", "x", "k")
	g := r.NewGauge("g", "x")
	h := r.NewHistogram("h", "x", LatencyBuckets)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.With("a").Inc()
				g.Add(1)
				h.Observe(0.003)
				g.Add(-1)
			}
		}()
	}
	wg.Wait()
	got := render(t, r)
	for _, line := range []string{`n_total{k="a"} 8000`, "g 0", "h_count 8000", `h_bucket{le="0.005"} 8000`, `h_bucket{le="0.002"} 0`} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("缺少 %q：\n%s", line, got)
		}
	}
}

func TestHandlerContentType(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("c_total", "x").Inc()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "c_total 1\n") {
		t.Fatalf("正文不对：%s", rec.Body.String())
	}
}
