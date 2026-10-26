#!/usr/bin/env bash
# 按一个基准值（通常是你们预测的上限）生成各阶段的到达率，输出 loadgen -rates 要的逗号分隔列表。
#
# 用法：scripts/rates.sh <基准> [倍数…]
#   不给倍数时用 0.2 0.4 0.6 0.7 0.8 0.9 1.0 1.1 1.2
# 例：
#   scripts/rates.sh 377                      → 75,151,226,264,302,339,377,415,452
#   scripts/rates.sh 1300 0.3 0.6 0.8 1 1.25 1.5
set -euo pipefail
base=${1:?用法：scripts/rates.sh <基准> [倍数…]}
shift
[ $# -gt 0 ] || set -- 0.2 0.4 0.6 0.7 0.8 0.9 1.0 1.1 1.2
awk -v base="$base" 'BEGIN {
  if (base + 0 <= 0) { print "基准必须是正数" > "/dev/stderr"; exit 1 }
  for (i = 1; i < ARGC; i++) printf "%s%d", (i > 1 ? "," : ""), base * ARGV[i] + 0.5
  print ""
}' "$@"
