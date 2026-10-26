#!/usr/bin/env bash
# 把一次实验的 steps.csv（客户端）、red.csv（服务层）、use.csv（资源层）按阶段拼起来，
# 挑出指定的列，输出 Markdown 表格，可以直接贴进 LAB2.md。
#
# 用法：scripts/table.sh runs/<名字> 列名 [列名…]
#   列名取自三张表的表头；step、target_rps 总是输出。三张表有同名列时取先出现的那张。
# 例：
#   scripts/table.sh runs/r1-pool4 ok_rps p99_ms server_p99_ms pool_in_use_avg svc_cpu_busy
set -euo pipefail
dir=${1:?用法：scripts/table.sh runs/<名字> 列名…}
shift
[ $# -gt 0 ] || { echo "至少给一个列名" >&2; exit 1; }
files=("$dir/steps.csv")
for f in red use; do [ -f "$dir/$f.csv" ] && files+=("$dir/$f.csv"); done
awk -F, -v want="$*" '
FNR == 1 {
  file++
  for (i = 1; i <= NF; i++) if (!(($i) in where)) { where[$i] = file; idx[$i] = i }
  next
}
{
  key = $1
  if (file == 1) order[++n] = key
  for (i = 1; i <= NF; i++) val[file, key, i] = $i
}
END {
  k = split("step target_rps " want, cols, " ")
  for (c = 1; c <= k; c++) if (!(cols[c] in where)) { printf "没有这一列：%s\n", cols[c] > "/dev/stderr"; bad = 1 }
  if (bad) exit 1
  line = "|"; sep = "|"
  for (c = 1; c <= k; c++) { line = line " " cols[c] " |"; sep = sep "---|" }
  print line; print sep
  for (r = 1; r <= n; r++) {
    line = "|"
    for (c = 1; c <= k; c++) {
      v = val[where[cols[c]], order[r], idx[cols[c]]]
      line = line " " (v == "" ? "-" : v) " |"
    }
    print line
  }
}' "${files[@]}"
