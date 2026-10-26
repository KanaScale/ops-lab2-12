#!/usr/bin/env bash
# 改了 deploy/queries-*.txt 以后，对已经跑过的实验重新按阶段查询 Prometheus，覆盖 red.csv 和 use.csv。
# 查询用的变量（接口、SLO 阈值、核、磁盘）取自 meta.txt，和第一次查询时一致。
# 前提是 Prometheus 里还有那次实验的数据：它的数据在 /dev/shm 里，Prometheus 或主机重启后就没了。
#
# 用法（在仓库根目录）：scripts/requery.sh runs/<名字> [runs/<名字> …]
# 环境变量：PROM（Prometheus 的地址，默认 http://127.0.0.1:9090）
set -euo pipefail
PROM=${PROM:-http://127.0.0.1:9090}
[ $# -gt 0 ] || { echo "用法：scripts/requery.sh runs/<名字> …" >&2; exit 1; }
for dir in "$@"; do
  line=$(sed -n 's/^promsteps 变量: //p' "$dir/meta.txt")
  [ -n "$line" ] || { echo "$dir/meta.txt 里没有以 promsteps 变量 开头的一行" >&2; exit 1; }
  args=()
  for kv in $line; do args+=(-var "$kv"); done
  for f in red use; do
    bin/promsteps -steps "$dir/steps.csv" -prom "$PROM" "${args[@]}" -queries "deploy/queries-$f.txt" >"$dir/$f.csv.new"
    mv "$dir/$f.csv.new" "$dir/$f.csv"
  done
  echo "已重新查询：$dir"
done
