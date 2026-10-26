package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServer(t *testing.T, cfg config) (*server, *httptest.Server) {
	t.Helper()
	if cfg.Data == "" {
		cfg.Data = t.TempDir()
	}
	if cfg.Sync == "" {
		cfg.Sync = "each"
	}
	if cfg.Pool == 0 {
		cfg.Pool = 4
	}
	s, err := newServer(cfg, time.Second, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() {
		ts.Close()
		s.log.Close()
	})
	return s, ts
}

func scrape(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	return b.String()
}

// metricValue 从 /metrics 原文里取一行的值；找不到返回 -1。
func metricValue(text, series string) float64 {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
	}
	return -1
}

func TestCourse(t *testing.T) {
	_, ts := testServer(t, config{Work: 1000, DBMs: 1})
	resp, err := http.Get(ts.URL + "/api/courses/42")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var c course
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil || resp.StatusCode != 200 || c.ID != "42" {
		t.Fatalf("status %d, course %+v, err %v", resp.StatusCode, c, err)
	}
	m := scrape(t, ts)
	if v := metricValue(m, `http_requests_total{route="/api/courses/{id}",code="200"}`); v != 1 {
		t.Errorf("请求计数 = %v，期望 1\n%s", v, m)
	}
	if v := metricValue(m, `db_query_duration_seconds_count`); v != 1 {
		t.Errorf("查询计数 = %v", v)
	}
}

// 池大小为 1、每次查询 60 ms 时，同时来的 3 个请求只能一个一个过：最后一个至少等 120 ms。
func TestPoolSerializes(t *testing.T) {
	_, ts := testServer(t, config{Pool: 1, DBMs: 60})
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/api/courses/1")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d < 180*time.Millisecond {
		t.Fatalf("3 个请求用了 %v，池没有起到限制作用", d)
	}
	m := scrape(t, ts)
	if v := metricValue(m, `db_pool_wait_seconds_bucket{le="0.05"}`); v != 1 {
		t.Errorf("应当只有 1 个请求几乎不用等连接，得到 %v\n%s", v, m)
	}
	if v := metricValue(m, "db_pool_in_use"); v != 0 {
		t.Errorf("结束后占用数应为 0，得到 %v", v)
	}
}

func TestPoolTimeoutGives503(t *testing.T) {
	s, ts := testServer(t, config{Pool: 1, DBMs: 300})
	s.pool.timeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() {
		resp, err := http.Get(ts.URL + "/api/courses/1")
		if err == nil {
			resp.Body.Close()
		}
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	resp, err := http.Get(ts.URL + "/api/courses/2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("池被占满且等待超时，应返回 503，得到 %d", resp.StatusCode)
	}
	<-done
	if v := metricValue(scrape(t, ts), `http_requests_total{route="/api/courses/{id}",code="503"}`); v != 1 {
		t.Errorf("503 计数 = %v", v)
	}
}

func enroll(t *testing.T, url string, i int) {
	t.Helper()
	body := fmt.Sprintf(`{"student":"s%d","course":"c1"}`, i)
	resp, err := http.Post(url+"/api/enroll", "application/json", strings.NewReader(body))
	if err != nil {
		t.Error(err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("enroll 返回 %d", resp.StatusCode)
	}
}

func TestEnrollModes(t *testing.T) {
	for _, mode := range []string{"each", "group", "interval"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			s, ts := testServer(t, config{Sync: mode, Data: dir})
			const n = 40
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); enroll(t, ts.URL, i) }()
			}
			wg.Wait()
			if mode == "interval" {
				time.Sleep(120 * time.Millisecond) // 等后台 fsync（测试里间隔为 50 ms）
			}
			m := scrape(t, ts)
			commits := metricValue(m, "enroll_commits_total")
			fsyncs := metricValue(m, "enroll_fsync_duration_seconds_count")
			records := metricValue(m, "enroll_commit_batch_size_sum")
			if commits != n || records != n {
				t.Fatalf("commits=%v，batch 合计=%v，期望都是 %d", commits, records, n)
			}
			switch mode {
			case "each":
				if fsyncs != n {
					t.Errorf("each 应当每笔一次 fsync：%v 次", fsyncs)
				}
			default:
				if fsyncs < 1 || fsyncs > n {
					t.Errorf("%s 的 fsync 次数 %v 不合理", mode, fsyncs)
				}
			}
			t.Logf("%s：%d 笔提交，%v 次 fsync", mode, n, fsyncs)
			s.log.Close()
			b, err := os.ReadFile(filepath.Join(dir, "enroll.log"))
			if err != nil {
				t.Fatal(err)
			}
			if lines := bytes.Count(b, []byte("\n")); lines != n {
				t.Errorf("日志有 %d 行，期望 %d", lines, n)
			}
		})
	}
}

// 组提交时，后台正在 fsync 的那段时间里到达的提交会合并进下一次 fsync。
func TestGroupCommitBatches(t *testing.T) {
	_, ts := testServer(t, config{Sync: "group"})
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); enroll(t, ts.URL, i) }()
	}
	wg.Wait()
	m := scrape(t, ts)
	fsyncs := metricValue(m, "enroll_fsync_duration_seconds_count")
	if fsyncs >= n {
		t.Errorf("200 个并发提交用了 %v 次 fsync，没有合并", fsyncs)
	}
	t.Logf("200 个并发提交，%v 次 fsync", fsyncs)
}

func TestEnrollBadRequest(t *testing.T) {
	_, ts := testServer(t, config{})
	resp, err := http.Post(ts.URL+"/api/enroll", "application/json", strings.NewReader(`{"student":""}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("缺字段应返回 400，得到 %d", resp.StatusCode)
	}
}

func TestBadSyncMode(t *testing.T) {
	if _, err := openCommitLog("never", t.TempDir(), time.Second, nil); err == nil {
		t.Fatal("未知的 -sync 应当报错")
	}
}

func TestExportAndReport(t *testing.T) {
	_, ts := testServer(t, config{})
	resp, err := http.Get(ts.URL + "/api/export?kb=1500")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	resp.Body.Close()
	if buf.Len() != 1500<<10 {
		t.Errorf("export 返回 %d 字节，期望 %d", buf.Len(), 1500<<10)
	}
	resp, err = http.Get(ts.URL + "/api/report?mb=2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("report 返回 %d", resp.StatusCode)
	}
}

func TestMetricsAndConfig(t *testing.T) {
	_, ts := testServer(t, config{Work: 5, Pool: 7, DBMs: 1})
	http.Get(ts.URL + "/api/courses/1")
	m := scrape(t, ts)
	for _, want := range []string{
		`enrolld_info{work="5",pool="7",db_ms="1",sync="each",go_version="`,
		`http_request_duration_seconds_bucket{route="/api/courses/{id}",code="200",le="0.05"}`,
		`http_inflight_requests{route="/api/courses/{id}"} 0`,
		"db_pool_size 7",
		"# TYPE process_cpu_seconds_total counter",
		"process_resident_memory_bytes ",
		"process_threads ",
		"go_gc_cycles_total ",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics 缺少 %q", want)
		}
	}
	if v := metricValue(m, "process_threads"); v < 1 {
		t.Errorf("process_threads = %v", v)
	}
	resp, err := http.Get(ts.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	var c config
	json.NewDecoder(resp.Body).Decode(&c)
	resp.Body.Close()
	if c.Pool != 7 || c.Work != 5 {
		t.Errorf("/config = %+v", c)
	}
}

func TestBenchFsync(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	if err := benchFsync(dir, 20, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "p99") {
		t.Errorf("输出缺少分位数：%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "fsync-bench.tmp")); !os.IsNotExist(err) {
		t.Error("临时文件没有删除")
	}
}

func TestNearestRank(t *testing.T) {
	for _, c := range []struct {
		p    float64
		n    int
		want int
	}{{50, 10, 4}, {99, 100, 98}, {99, 1000, 989}, {100, 7, 6}, {1, 7, 0}, {99, 1, 0}} {
		if got := nearestRank(c.p, c.n); got != c.want {
			t.Errorf("nearestRank(%v, %d) = %d，期望 %d", c.p, c.n, got, c.want)
		}
	}
}
