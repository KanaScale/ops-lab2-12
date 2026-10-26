// enrolld 是实验 2 的被测服务：一个缩小了的选课后端，有一条读路径和一条写路径，
// 各带一个可以被打满的资源，并通过 /metrics 暴露 Prometheus 格式的指标。
//
//	GET  /api/courses/{id}   读路径：一段纯计算（-work），再到“数据库连接池”（-pool 个连接）
//	                          里占一个连接 -db-ms 毫秒。对应 L-06 课程实践的“CPU”和“连接池”两个上限。
//	POST /api/enroll         写路径：把一条选课记录追加到 data/enroll.log，按 -sync 的方式落盘。
//	                          对应 L-07 资源账里“每笔提交等 fsync”的那一行。
//	GET  /metrics            Prometheus 指标（RED、连接池、fsync、进程）
//	GET  /config             当前参数（JSON），scripts/run.sh 会把它记进每次实验的 meta.txt
//	GET  /healthz            存活检查
//	GET  /debug/pprof/       Go 的性能剖析接口
//	GET  /api/report、/api/export   选做 O1（内存与 GC）、O2（网络）用
//
// “数据库”是模拟的：占着一个连接睡 -db-ms 毫秒。它不消耗本机 CPU，只限制同时能有多少个查询，
// 和真实的连接池一样，池满以后请求只能排队等。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"enroll-lab/internal/metrics"
)

type config struct {
	Addr        string  `json:"addr"`
	Work        int     `json:"work"`
	Pool        int     `json:"pool"`
	DBMs        float64 `json:"db_ms"`
	PoolTimeout string  `json:"pool_timeout"`
	Sync        string  `json:"sync"`
	Interval    string  `json:"sync_interval"`
	Data        string  `json:"data"`
	GoVersion   string  `json:"go_version"`
	GOMAXPROCS  int     `json:"gomaxprocs"`
	GOGC        string  `json:"gogc"`
	PID         int     `json:"pid"`
	Started     string  `json:"started"`
}

type server struct {
	cfg     config
	reg     *metrics.Registry
	pool    *pool
	log     commitLog
	seq     atomic.Uint64
	exports []byte

	requests *metrics.CounterVec
	duration *metrics.HistogramVec
	inflight *metrics.GaugeVec
	commits  *metrics.Counter
}

func main() {
	var cfg config
	var poolTimeout, interval time.Duration
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:8080", "监听地址；默认只监听本机，选做 O2 两台主机时改成 0.0.0.0:8080")
	flag.IntVar(&cfg.Work, "work", 200000, "每个读请求的计算量（循环次数，固定不变）；耗时因 CPU 而异，任务 2 自己测")
	flag.IntVar(&cfg.Pool, "pool", 4, "数据库连接池大小")
	flag.Float64Var(&cfg.DBMs, "db-ms", 10, "每次查询占用一个连接的时间（毫秒）")
	flag.DurationVar(&poolTimeout, "pool-timeout", time.Second, "等连接的最长时间，超过返回 503")
	flag.StringVar(&cfg.Sync, "sync", "each", "写路径落盘方式：each 每笔 fsync；group 组提交；interval 定期 fsync")
	flag.DurationVar(&interval, "sync-interval", time.Second, "-sync interval 时多久 fsync 一次")
	flag.StringVar(&cfg.Data, "data", "data", "数据目录，必须在真实的磁盘上（不能是 tmpfs）")
	benchN := flag.Int("bench-fsync", 0, "不启动服务，只在数据目录里做 N 次“追加 + fsync”并打印耗时分布")
	flag.Parse()
	cfg.PoolTimeout = poolTimeout.String()
	cfg.Interval = interval.String()

	if err := os.MkdirAll(cfg.Data, 0o755); err != nil {
		log.Fatal(err)
	}
	warnIfTmpfs(cfg.Data)
	if *benchN > 0 {
		if err := benchFsync(cfg.Data, *benchN, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if cfg.Pool < 1 || cfg.Work < 0 || cfg.DBMs < 0 {
		log.Fatal("-pool 至少为 1，-work、-db-ms 不能为负")
	}

	cfg.GoVersion = runtime.Version()
	cfg.GOMAXPROCS = runtime.GOMAXPROCS(0)
	cfg.GOGC = os.Getenv("GOGC")
	cfg.PID = os.Getpid()
	cfg.Started = time.Now().Format(time.RFC3339)

	s, err := newServer(cfg, poolTimeout, interval)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: cfg.Addr, Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		// 优雅停止（L-05）：不再接新请求，等在途请求做完，最后把日志落盘再退出。
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("等在途请求超时：%v", err)
		}
		close(stopped)
	}()

	log.Printf("enrolld 监听 %s；GOMAXPROCS=%d；读：work=%d、pool=%d、db-ms=%g；写：sync=%s，数据目录 %s",
		cfg.Addr, cfg.GOMAXPROCS, cfg.Work, cfg.Pool, cfg.DBMs, cfg.Sync, cfg.Data)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-stopped
	if err := s.log.Close(); err != nil {
		log.Printf("关闭日志：%v", err)
	}
	log.Print("已停止")
}

func newServer(cfg config, poolTimeout, interval time.Duration) (*server, error) {
	reg := metrics.NewRegistry()
	s := &server{cfg: cfg, reg: reg}

	reg.NewGaugeVec("enrolld_info", "服务参数，值恒为 1；参数在标签里", "work", "pool", "db_ms", "sync", "go_version").
		With(strconv.Itoa(cfg.Work), strconv.Itoa(cfg.Pool), strconv.FormatFloat(cfg.DBMs, 'g', -1, 64), cfg.Sync, cfg.GoVersion).
		Set(1)
	s.requests = reg.NewCounterVec("http_requests_total", "处理完的请求数，按接口和状态码分", "route", "code")
	s.duration = reg.NewHistogramVec("http_request_duration_seconds",
		"服务端耗时：从进入处理函数到处理函数返回（不含内核里的排队和网络）", metrics.LatencyBuckets, "route", "code")
	s.inflight = reg.NewGaugeVec("http_inflight_requests", "正在处理的请求数（在途）", "route")
	s.commits = reg.NewCounter("enroll_commits_total", "已确认落盘方式完成、返回给客户端的选课记录数")

	s.pool = newPool(reg, cfg.Pool, time.Duration(cfg.DBMs*float64(time.Millisecond)), poolTimeout)
	lm := newLogMetrics(reg)
	var err error
	s.log, err = openCommitLog(cfg.Sync, cfg.Data, interval, lm)
	if err != nil {
		return nil, err
	}
	registerProcessMetrics(reg)

	s.exports = make([]byte, 1<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range s.exports {
		s.exports[i] = byte(rng.Uint32())
	}
	return s, nil
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/courses/{id}", s.instrument("/api/courses/{id}", s.handleCourse))
	mux.HandleFunc("POST /api/enroll", s.instrument("/api/enroll", s.handleEnroll))
	mux.HandleFunc("GET /api/report", s.instrument("/api/report", s.handleReport))
	mux.HandleFunc("GET /api/export", s.instrument("/api/export", s.handleExport))
	mux.Handle("GET /metrics", s.reg.Handler())
	mux.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(s.cfg)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// instrument 给一个接口加上 RED：请求数（按状态码）、服务端耗时直方图、在途请求数。
func (s *server) instrument(route string, h http.HandlerFunc) http.HandlerFunc {
	inflight := s.inflight.With(route)
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		inflight.Inc()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		h(rec, r)
		inflight.Dec()
		code := strconv.Itoa(rec.code)
		s.duration.With(route, code).Observe(time.Since(start).Seconds())
		s.requests.With(route, code).Inc()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// 客户端已经断开时记的状态码（nginx 的习惯）；这时响应已经发不出去了。
const statusClientClosed = 499

type course struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Teacher   string   `json:"teacher"`
	SeatsLeft int      `json:"seats_left"`
	Schedule  []string `json:"schedule"`
	Checksum  uint64   `json:"checksum"`
}

// handleCourse 是读路径：先做一段计算（校验、组装，占本机 CPU），再查一次“数据库”（占一个连接）。
func (s *server) handleCourse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sum := spin(s.cfg.Work)
	if err := s.pool.query(r.Context()); err != nil {
		if errors.Is(err, errPoolTimeout) {
			http.Error(w, "等数据库连接超时", http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(statusClientClosed)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(course{
		ID: id, Name: "互联网产品开发与运维", Teacher: "任锐", SeatsLeft: int(sum % 120),
		Schedule: []string{"周二 3-4 节 单双周", "周五 3-4 节 单双周"}, Checksum: sum,
	})
}

type enrollReq struct {
	Student string `json:"student"`
	Course  string `json:"course"`
}

type record struct {
	Seq     uint64 `json:"seq"`
	T       int64  `json:"t"`
	Student string `json:"student"`
	Course  string `json:"course"`
}

// handleEnroll 是写路径：记录按 -sync 指定的方式落盘之后，才告诉客户端“选上了”。
func (s *server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Student == "" || req.Course == "" {
		http.Error(w, `请求体应为 {"student":"…","course":"…"}`, http.StatusBadRequest)
		return
	}
	seq := s.seq.Add(1)
	line, _ := json.Marshal(record{Seq: seq, T: time.Now().UnixNano(), Student: req.Student, Course: req.Course})
	if err := s.log.Commit(append(line, '\n')); err != nil {
		http.Error(w, "写入失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	s.commits.Inc()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"seq":%d}`+"\n", seq)
}

// handleReport 供选做 O1：每个请求分配 mb MB 的临时内存（默认 4），制造 GC 压力。
func (s *server) handleReport(w http.ResponseWriter, r *http.Request) {
	mb := intParam(r, "mb", 4, 1, 256)
	buf := make([]byte, mb<<20)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = byte(i) // 每页写一下，让内存真正被用到
	}
	var sum uint64
	for i := 0; i < len(buf); i += 4096 {
		sum += uint64(buf[i])
	}
	fmt.Fprintf(w, `{"bytes":%d,"sum":%d}`+"\n", len(buf), sum)
}

// handleExport 供选做 O2：返回 kb KB 的数据（默认 256），用大响应把带宽打满。
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	kb := intParam(r, "kb", 256, 1, 1<<20)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(kb<<10))
	for left := kb << 10; left > 0; {
		n := min(left, len(s.exports))
		if _, err := w.Write(s.exports[:n]); err != nil {
			return
		}
		left -= n
	}
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return max(lo, min(v, hi))
}

// spin 是读路径上的“业务计算”：固定次数的整数运算，只占 CPU，不分配内存、不做 I/O。
// 次数固定，所以每个请求的计算量不变；耗时多少取决于 CPU，由你们自己测。
func spin(n int) uint64 {
	x := uint64(88172645463325252)
	for i := 0; i < n; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	return x
}
