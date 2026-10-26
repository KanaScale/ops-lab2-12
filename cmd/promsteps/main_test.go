package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpand(t *testing.T) {
	got, err := expand(`rate(x{route="${route}"}[${window}])`, 25*time.Second, vars{"route": "/api/enroll"})
	if err != nil || got != `rate(x{route="/api/enroll"}[25s])` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := expand(`x{le="${slo}"}`, time.Second, vars{}); err == nil || !strings.Contains(err.Error(), "${slo}") {
		t.Fatalf("缺变量时应指出是哪个：%v", err)
	}
}

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadStepsSkipsAll(t *testing.T) {
	p := write(t, "steps.csv", "step,target_rps,start_unix,end_unix,ok\n1,100,1000,1030,5\n2,200,1030,1060,9\nall,,1000,1060,14\n")
	steps, err := readSteps(p)
	if err != nil || len(steps) != 2 || steps[1].end != 1060 || steps[1].target != "200" {
		t.Fatalf("%+v %v", steps, err)
	}
}

func TestReadQueries(t *testing.T) {
	p := write(t, "q.txt", "# 注释\n\nserver_rps   sum(rate(x[${window}]))\nfoo\tTODO\n")
	qs, err := readQueries(p)
	if err != nil || len(qs) != 2 || qs[0].expr != "sum(rate(x[${window}]))" || qs[1].expr != "TODO" {
		t.Fatalf("%+v %v", qs, err)
	}
	if _, err := readQueries(write(t, "bad.txt", "onlyname\n")); err == nil {
		t.Fatal("只有名字的行应当报错")
	}
}

// 假的 Prometheus：按查询内容返回结果，并记下收到的查询和时刻。
func fakeProm(t *testing.T, seen *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, ts := r.URL.Query().Get("query"), r.URL.Query().Get("time")
		*seen = append(*seen, q+"@"+ts)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "multi"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"a":"1"},"value":[1,"1"]},{"metric":{"a":"2"},"value":[1,"2"]}]}}`)
		case strings.Contains(q, "empty"):
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		case strings.Contains(q, "bad("):
			fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
		default:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"123.456789"]}]}}`)
		}
	}))
}

func TestTable(t *testing.T) {
	var seen []string
	ts := fakeProm(t, &seen)
	defer ts.Close()
	steps := []step{{"1", "100", 1000, 1030}, {"2", "200", 1030, 1060}}
	qs := []query{{"a", `sum(rate(x{r="${route}"}[${window}]))`}, {"b", "empty"}, {"c", "TODO"}}
	var out, log strings.Builder
	if err := table(&out, &log, ts.Client(), ts.URL, steps, qs, vars{"route": "/r"}, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	recs, _ := csv.NewReader(strings.NewReader(out.String())).ReadAll()
	if len(recs) != 3 || strings.Join(recs[0], ",") != "step,target_rps,a,b,c" || strings.Join(recs[2], ",") != "2,200,123.457,," {
		t.Fatalf("输出不对：\n%s", out.String())
	}
	if seen[0] != `sum(rate(x{r="/r"}[25s]))@1030` {
		t.Errorf("查询或时刻不对：%v", seen)
	}
	if !strings.Contains(log.String(), "c 还是 TODO") {
		t.Errorf("应提醒 TODO：%s", log.String())
	}
}

func TestTableErrors(t *testing.T) {
	var seen []string
	ts := fakeProm(t, &seen)
	defer ts.Close()
	steps := []step{{"1", "100", 1000, 1030}}
	for _, expr := range []string{"multi", "bad(x)"} {
		var out, log strings.Builder
		err := table(&out, &log, ts.Client(), ts.URL, steps, []query{{"q", expr}}, vars{}, 5*time.Second)
		if err == nil {
			t.Errorf("%s 应当报错", expr)
		}
	}
}
