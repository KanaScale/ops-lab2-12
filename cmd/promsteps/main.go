// promsteps 按 loadgen 的阶段去 Prometheus 查询：对每个阶段，在阶段结束时刻、以阶段时长（去掉开头 -trim）
// 为窗口，执行查询文件里的每一条 PromQL，输出一张“阶段 × 查询”的 CSV 表。
//
// 这样客户端（loadgen 的 steps.csv）和服务端、资源层（本工具的输出）的数据按同样的阶段对齐，可以逐行比较。
//
// 查询文件每行一条：名字，空白，PromQL。# 开头的行是注释。PromQL 里可以用：
//
//	${window}   换成本阶段的窗口长度，例如 25s
//	${名字}      换成 -var 名字=值 给的值，例如 ${route}
//
// 每条查询必须只返回一条序列；返回多条时，用 sum(...) 或 avg(...) 聚合。PromQL 写成 TODO 的行会输出空列，提醒你们补上。
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type query struct{ name, expr string }

type step struct {
	name, target string
	start, end   int64
}

type vars map[string]string

func (v vars) String() string { return fmt.Sprint(map[string]string(v)) }

func (v vars) Set(s string) error {
	k, val, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return errors.New("-var 的格式是 名字=值")
	}
	v[k] = val
	return nil
}

func main() {
	vs := vars{}
	stepsPath := flag.String("steps", "", "loadgen 输出的 steps.csv")
	queriesPath := flag.String("queries", "deploy/queries-red.txt", "查询文件")
	prom := flag.String("prom", "http://127.0.0.1:9090", "Prometheus 地址")
	trim := flag.Duration("trim", 5*time.Second, "每个阶段开头不计入窗口的时长（让上一阶段的余波过去）")
	flag.Var(vs, "var", "查询里的变量，名字=值，可重复")
	flag.Parse()
	if *stepsPath == "" {
		fmt.Fprintln(os.Stderr, "用法：promsteps -steps runs/<名字>/steps.csv [-queries 文件] [-var route=/api/enroll]")
		os.Exit(2)
	}
	steps, err := readSteps(*stepsPath)
	if err != nil {
		fail(err)
	}
	qs, err := readQueries(*queriesPath)
	if err != nil {
		fail(err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if err := table(os.Stdout, os.Stderr, client, *prom, steps, qs, vs, *trim); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "promsteps：", err)
	os.Exit(1)
}

func readSteps(path string) ([]step, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) < 2 {
		return nil, fmt.Errorf("%s 里没有阶段", path)
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	for _, need := range []string{"step", "target_rps", "start_unix", "end_unix"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("%s 缺少列 %s，它是 loadgen 的输出吗？", path, need)
		}
	}
	var steps []step
	for _, r := range recs[1:] {
		if r[col["step"]] == "all" {
			continue
		}
		s, err1 := strconv.ParseInt(r[col["start_unix"]], 10, 64)
		e, err2 := strconv.ParseInt(r[col["end_unix"]], 10, 64)
		if err := errors.Join(err1, err2); err != nil {
			return nil, err
		}
		steps = append(steps, step{name: r[col["step"]], target: r[col["target_rps"]], start: s, end: e})
	}
	return steps, nil
}

func readQueries(path string) ([]query, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var qs []query
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, expr, ok := strings.Cut(line, " ")
		if !ok {
			name, expr, ok = strings.Cut(line, "\t")
		}
		if !ok || strings.TrimSpace(expr) == "" {
			return nil, fmt.Errorf("%s 第 %d 行：格式应为“名字 PromQL”", path, n)
		}
		qs = append(qs, query{name: name, expr: strings.TrimSpace(expr)})
	}
	return qs, sc.Err()
}

func expand(expr string, window time.Duration, vs vars) (string, error) {
	out := strings.ReplaceAll(expr, "${window}", fmt.Sprintf("%ds", int(window.Seconds())))
	for k, v := range vs {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	if i := strings.Index(out, "${"); i >= 0 {
		end := strings.IndexByte(out[i:], '}')
		if end < 0 {
			end = len(out) - i - 1
		}
		return "", fmt.Errorf("变量 %s 没有给值（用 -var 名字=值）", out[i:i+end+1])
	}
	return out, nil
}

func table(w, log io.Writer, client *http.Client, prom string, steps []step, qs []query, vs vars, trim time.Duration) error {
	cw := csv.NewWriter(w)
	head := []string{"step", "target_rps"}
	for _, q := range qs {
		head = append(head, q.name)
	}
	cw.Write(head)
	warned := map[string]bool{}
	for _, s := range steps {
		window := time.Duration(s.end-s.start)*time.Second - trim
		if window < 15*time.Second && !warned["window"] {
			fmt.Fprintf(log, "提醒：窗口只有 %v。抓取间隔为 5 s 时，窗口太短 rate() 可能拿不到两个样本；阶段至少 30 s\n", window)
			warned["window"] = true
		}
		line := []string{s.name, s.target}
		for _, q := range qs {
			if q.expr == "TODO" {
				if !warned[q.name] {
					fmt.Fprintf(log, "提醒：%s 还是 TODO，这一列为空\n", q.name)
					warned[q.name] = true
				}
				line = append(line, "")
				continue
			}
			expr, err := expand(q.expr, window, vs)
			if err != nil {
				return fmt.Errorf("%s：%w", q.name, err)
			}
			v, err := instant(client, prom, expr, s.end)
			if err != nil {
				return fmt.Errorf("阶段 %s，查询 %s：%w", s.name, q.name, err)
			}
			line = append(line, v)
		}
		cw.Write(line)
	}
	cw.Flush()
	return cw.Error()
}

type apiResp struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// instant 在时刻 t 执行一次即时查询，返回唯一一条序列的值；没有结果时返回空串。
func instant(client *http.Client, prom, expr string, t int64) (string, error) {
	u := strings.TrimRight(prom, "/") + "/api/v1/query?" + url.Values{
		"query": {expr}, "time": {strconv.FormatInt(t, 10)},
	}.Encode()
	resp, err := client.Get(u)
	if err != nil {
		return "", fmt.Errorf("连不上 Prometheus（%w）", err)
	}
	defer resp.Body.Close()
	var r apiResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("HTTP %d，返回的不是 Prometheus API 的 JSON", resp.StatusCode)
	}
	if r.Status != "success" {
		return "", fmt.Errorf("%s：%s\n  查询：%s", r.ErrorType, r.Error, expr)
	}
	switch r.Data.ResultType {
	case "scalar":
		var v [2]any
		if err := json.Unmarshal(r.Data.Result, &v); err != nil {
			return "", err
		}
		return round(v[1]), nil
	case "vector":
		var vec []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &vec); err != nil {
			return "", err
		}
		switch len(vec) {
		case 0:
			return "", nil
		case 1:
			return round(vec[0].Value[1]), nil
		}
		return "", fmt.Errorf("返回了 %d 条序列（第一条的标签：%v），请用 sum(...) 等聚合成一条\n  查询：%s",
			len(vec), vec[0].Metric, expr)
	}
	return "", fmt.Errorf("不支持的结果类型 %s", r.Data.ResultType)
}

// round 把 Prometheus 返回的数值保留 6 位有效数字，便于阅读；NaN、Inf 原样保留。
func round(v any) string {
	s, _ := v.(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'g', 6, 64)
}
