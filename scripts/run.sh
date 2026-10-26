#!/usr/bin/env bash
# 跑一次实验：施加负载的同时采集资源层的原始数据，结束后按阶段查询 Prometheus。结果全部放进 runs/<名字>/。
#
# 用法（在仓库根目录，enrolld、node_exporter、Prometheus 已经启动）：
#   scripts/run.sh <名字> <loadgen 的参数…>
# 例：
#   scripts/run.sh r1-pool4 -url http://127.0.0.1:8080/api/courses/1 -rates 80,160,240,320,360,400,440
#
# runs/<名字>/ 里有：
#   meta.txt           主机、内核、磁盘、enrolld 的参数（/config）、绑核、压测命令、仓库提交
#   steps.csv          loadgen：客户端看到的每个阶段；最后一行 all 是所有有负载阶段的合计
#   red.csv            服务层，每个阶段一行（deploy/queries-red.txt）
#   use.csv            资源层，每个阶段一行（deploy/queries-use.txt）
#   vmstat.log mpstat.log iostat.log pidstat.log   资源层的原始输出，每秒一行
#   metrics-start.txt metrics-end.txt              开始和结束时 /metrics 的原文
#   loadgen.log        loadgen 的进度、结果表和警告
# 改了查询文件以后，用 scripts/requery.sh runs/<名字> 重新生成 red.csv、use.csv。
#
# 环境变量：
#   GEN_CPUS  loadgen 绑定的核，默认 0（见指导书的分核表）
#   SVC       enrolld 的地址，默认 http://127.0.0.1:8080
#   PROM      Prometheus 的地址，默认 http://127.0.0.1:9090
#   SLO       算 SLI 用的延迟阈值（秒），默认 0.05；要和直方图的某个桶上界一致
#   DATA      enrolld 的数据目录，默认 data
#   DISK      数据目录所在的磁盘（如 vda），默认自动判断
set -euo pipefail

GEN_CPUS=${GEN_CPUS:-0}
SVC=${SVC:-http://127.0.0.1:8080}
PROM=${PROM:-http://127.0.0.1:9090}
SLO=${SLO:-0.05}
DATA=${DATA:-data}

die() { echo "run.sh：$*" >&2; exit 1; }

name=${1:-}
if [ -z "$name" ] || [ $# -lt 2 ]; then die "用法：scripts/run.sh <名字> <loadgen 参数…>"; fi
shift
[[ $name =~ ^[A-Za-z0-9._-]+$ ]] || die "名字只能用字母、数字和 . _ -"
if [ ! -x bin/loadgen ] || [ ! -x bin/promsteps ]; then die "请在仓库根目录运行，并先 make build"; fi
for c in vmstat mpstat iostat pidstat taskset curl findmnt lsblk; do
  command -v "$c" >/dev/null || die "缺少命令 $c（vmstat 在 procps 里，mpstat、iostat、pidstat 在 sysstat 里）"
done
dir=runs/$name
[ ! -e "$dir" ] || die "$dir 已经存在：换一个名字，或者确认不要以后再删掉它"

config=$(curl -fsS -m 5 "$SVC/config") || die "连不上 enrolld（$SVC），它启动了吗？"
pid=$(sed -n 's/.*"pid": *\([0-9][0-9]*\).*/\1/p' <<<"$config")
if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
  die "从 /config 拿不到 enrolld 的进程号；enrolld 要和本脚本在同一台主机上"
fi

# 压测的接口，用来在查询里只看这个接口
url=""
prev=""
for a in "$@"; do
  [ "$prev" = "-url" ] && url=$a
  case $a in -url=*) url=${a#-url=} ;; esac
  prev=$a
done
[ -n "$url" ] || die "loadgen 参数里缺少 -url"
path=${url#*://}
path=/${path#*/}
path=${path%%\?*}
case $path in
  /api/courses/*) route='/api/courses/{id}' ;;
  *) route=$path ;;
esac

# enrolld 绑定的核，换成正则（1-3 → 1|2|3）
svc_cpus=$(taskset -pc "$pid" | sed 's/.*: *//')
cpu_regex() {
  local parts part i out=()
  IFS=, read -ra parts <<<"$1"
  for part in "${parts[@]}"; do
    if [[ $part == *-* ]]; then
      for ((i = ${part%-*}; i <= ${part#*-}; i++)); do out+=("$i"); done
    else
      out+=("$part")
    fi
  done
  local IFS='|'
  echo "${out[*]}"
}
cpus=$(cpu_regex "$svc_cpus")
for c in $(cpu_regex "$GEN_CPUS" | tr '|' ' '); do
  [[ "|$cpus|" == *"|$c|"* ]] && echo "警告：loadgen（GEN_CPUS=$GEN_CPUS）和 enrolld（$svc_cpus）共用了核 $c，结果会互相干扰" >&2
done

# 数据目录所在的磁盘：分区（vda1）要换成整块盘（vda），node_exporter 默认不统计分区
src=$(findmnt -no SOURCE -T "$DATA" 2>/dev/null || true)
fstype=$(findmnt -no FSTYPE -T "$DATA" 2>/dev/null || true)
disk=${DISK:-}
if [ -z "$disk" ] && [ -b "$src" ]; then
  if [ "$(lsblk -no TYPE "$src" | head -n 1)" = part ]; then
    disk=$(lsblk -no PKNAME "$src" | head -n 1)
  else
    disk=$(lsblk -no KNAME "$src" | head -n 1)
  fi
fi
[ -n "$disk" ] || { disk=unknown; echo "警告：判断不出 $DATA 在哪块盘上（$src，$fstype），磁盘那几列会是空的；可以用 DISK=设备名 指定" >&2; }
[ "$fstype" != tmpfs ] || echo "警告：$DATA 在 tmpfs 上，fsync 不写盘，写路径的数据没有意义" >&2

mkdir -p "$dir"
{
  echo "名字: $name"
  echo "开始: $(date -Iseconds)"
  echo "主机: $(uname -srm)"
  echo "CPU: $(nproc) 个逻辑核；$(lscpu 2>/dev/null | sed -n 's/^Model name: *//p' | head -n 1)"
  echo "内存: $(free -m | awk '/^Mem:/{print $2 " MiB，available " $7 " MiB"}')"
  echo "数据目录: $DATA → $src（$fstype），统计的磁盘: $disk"
  echo "enrolld: pid $pid，绑定的核 $svc_cpus"
  echo "loadgen: 绑定的核 $GEN_CPUS"
  echo "接口: $route；SLO 阈值 ${SLO}s"
  echo "promsteps 变量: route=$route slo=$SLO cpus=$cpus disk=$disk"
  echo "仓库提交: $(git rev-parse --short HEAD 2>/dev/null || echo 无)，未提交的改动 $(git status --porcelain 2>/dev/null | wc -l) 处"
  echo "压测命令: bin/loadgen ${*@Q}"
  echo "enrolld /config:"
  echo "$config"
} >"$dir/meta.txt"

curl -fsS -m 5 "$SVC/metrics" >"$dir/metrics-start.txt"

# 资源层的原始数据，每秒一行；S_TIME_FORMAT=ISO 让 sysstat 输出 24 小时制和 ISO 日期
export LC_ALL=C S_TIME_FORMAT=ISO
collectors=()
vmstat -t -w 1 >"$dir/vmstat.log" 2>&1 & collectors+=($!)
mpstat -P ALL 1 >"$dir/mpstat.log" 2>&1 & collectors+=($!)
if [ "$disk" != unknown ]; then
  iostat -d -x -t "$disk" 1 >"$dir/iostat.log" 2>&1 & collectors+=($!)
else
  iostat -d -x -t 1 >"$dir/iostat.log" 2>&1 & collectors+=($!)
fi
pidstat -h -u -r -w -p "$pid" 1 >"$dir/pidstat.log" 2>&1 & collectors+=($!)
stop_collectors() { kill "${collectors[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap stop_collectors EXIT

echo "== loadgen（结果写进 $dir/）"
status=0
taskset -c "$GEN_CPUS" bin/loadgen "$@" -slo "${SLO}s" -out "$dir/steps.csv" 2>&1 | tee "$dir/loadgen.log" || status=$?
sleep 1
stop_collectors
trap - EXIT
curl -fsS -m 5 "$SVC/metrics" >"$dir/metrics-end.txt" || echo "警告：结束时取不到 /metrics" >&2
[ "$status" -eq 0 ] || die "loadgen 退出码 $status，见 $dir/loadgen.log"

ps_args=(-steps "$dir/steps.csv" -prom "$PROM" -var "route=$route" -var "slo=$SLO" -var "cpus=$cpus" -var "disk=$disk")
echo "== 按阶段查询 Prometheus"
if bin/promsteps "${ps_args[@]}" -queries deploy/queries-red.txt >"$dir/red.csv" &&
  bin/promsteps "${ps_args[@]}" -queries deploy/queries-use.txt >"$dir/use.csv"; then
  for f in red use; do
    echo "-- $f.csv"
    # 空值换成 -，否则 column 会把相邻的逗号并成一个，列就错位了
    awk -F, -v OFS=, '{for (i = 1; i <= NF; i++) if ($i == "") $i = "-"; print}' "$dir/$f.csv" |
      if command -v column >/dev/null; then column -s, -t; else cat; fi
  done
else
  echo "查询 Prometheus 失败。修好以后，只要 Prometheus 里的数据还在，就可以用 scripts/requery.sh $dir 重新查询" >&2
fi
echo "完成：$dir"
