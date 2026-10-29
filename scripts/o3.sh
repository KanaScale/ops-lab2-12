#!/usr/bin/env bash
# 在旧服务器仓库根目录运行：scripts/o3.sh；服务和 Prometheus 须先启动并载入 O3 规则。
set -euo pipefail

dir=runs/o3-read-alert
svc=http://127.0.0.1:8080
prom=http://127.0.0.1:9090

[[ ! -e $dir ]] || { echo "$dir 已存在，请先检查旧结果" >&2; exit 1; }
[[ -x bin/loadgen ]] || { echo "缺少 bin/loadgen，请先 make build" >&2; exit 1; }
curl -fsS -m 5 "$svc/config" >/dev/null || { echo "enrolld 未监听 8080" >&2; exit 1; }
curl -fsS -m 5 "$prom/api/v1/rules?type=alert" | python3 -c '
import json, sys
groups = json.load(sys.stdin)["data"]["groups"]
assert any(r.get("name") == "ReadSLOBudgetBurn" for g in groups for r in g["rules"]), "Prometheus 尚未载入 ReadSLOBudgetBurn 规则"
' || exit 1

mkdir -p "$dir"
date -u +%FT%TZ > "$dir/started-utc.txt"
curl -fsS "$svc/config" > "$dir/server-config.json"
curl -fsS "$prom/api/v1/rules?type=alert" > "$dir/rules-before.json"

# 在压测期间每 5 秒抓一次；/api/v1/alerts 只返回当前活动告警，事后查会丢失 firing。
(
  for ((i=0; i<30; i++)); do
    if curl -fsS -m 4 "$prom/api/v1/alerts" > "$dir/alerts-current.json"; then
      if python3 - "$dir/alerts-current.json" "$dir/alerts-history.jsonl" <<'PY'
import datetime, json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    response = json.load(f)
alerts = [a for a in response.get("data", {}).get("alerts", [])
          if a.get("labels", {}).get("alertname") == "ReadSLOBudgetBurn"]
with open(sys.argv[2], "a", encoding="utf-8") as f:
    f.write(json.dumps({"time_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                        "alerts": alerts}, ensure_ascii=False) + "\n")
sys.exit(0 if any(a.get("state") == "firing" for a in alerts) else 1)
PY
      then
        cp "$dir/alerts-current.json" "$dir/alert-firing.json"
        break
      fi
    fi
    sleep 5
  done
) &
poll=$!

set +e
taskset -c 0 bin/loadgen -url "$svc/api/courses/1" -rates 461 -step 120s \
  -out "$dir/steps.csv" > "$dir/loadgen.log" 2>&1
load_status=$?
set -e
wait "$poll" || true
date -u +%FT%TZ > "$dir/finished-utc.txt"
if [[ ! -f $dir/alert-firing.json ]]; then
  echo "尚未捕获 firing；查看 $dir/alerts-history.jsonl 和 Prometheus 状态" >&2
  exit 1
fi
echo "已捕获 firing：$dir/alert-firing.json"
exit "$load_status"
