#!/usr/bin/env bash
# Two-host O2 experiment. Run from either host; requires sysstat and existing binaries.
# Server: bash scripts/o2.sh server o2-net
# Client: bash scripts/o2.sh client o2-net
set -euo pipefail
cd "$(dirname "$0")/.."
export LC_ALL=C TZ=UTC S_TIME_FORMAT=ISO
role=${1:?usage: o2.sh server|client name}
name=${2:?usage: o2.sh server|client name}
[[ $role == server || $role == client ]] || { echo 'role must be server or client' >&2; exit 1; }
[[ $name =~ ^[A-Za-z0-9._-]+$ ]] || { echo 'invalid run name' >&2; exit 1; }
server_ip=192.168.0.171
client_ip=192.168.0.16
cpus=${CPUS:-0-1}
rates=${RATES:-50,100,200,400,600,800,950,1100,1400}
step=${STEP:-30s}
peer=$server_ip
binary=bin/loadgen
if [[ $role == server ]]; then peer=$client_ip; binary=bin/enrolld; fi
for tool in curl sar mpstat ss ip taskset date sha256sum; do command -v "$tool" >/dev/null || { echo "missing: $tool" >&2; exit 1; }; done
[[ -x $binary ]] || { echo "missing executable: $binary" >&2; exit 1; }
taskset -c "$cpus" true
ss_filter="( dst $peer and ( sport = :8080 or dport = :8080 ) )"
ss -tin "$ss_filter" >/dev/null
route=$(ip -4 route get "$peer")
iface=$(awk '{for(i=1;i<=NF;i++) if($i=="dev") {print $(i+1); exit}}' <<<"$route")
local_ip=$(awk '{for(i=1;i<=NF;i++) if($i=="src") {print $(i+1); exit}}' <<<"$route")
expected=$client_ip
[[ $role == server ]] && expected=$server_ip
[[ $local_ip == "$expected" ]] || { echo "expected private IP $expected, got $local_ip; check topology" >&2; exit 1; }
if [[ $role == server && -n $(ss -H -ltn '( sport = :8080 )') ]]; then
  echo 'Port 8080 is occupied. Stop the previous enrolld before starting O2.' >&2
  exit 1
fi
dir="runs/$name-$role"
[[ ! -e $dir ]] || { echo "$dir exists; use a different run name" >&2; exit 1; }
mkdir -p "$dir" data
pids=()
service_pid=''
cleanup() {
  trap - EXIT INT TERM
  if [[ -n $service_pid ]] && kill -0 "$service_pid" 2>/dev/null; then
    curl --noproxy '*' -fsS -m 3 http://127.0.0.1:8080/metrics > "$dir/metrics-end.txt" || true
  fi
  cat /proc/net/snmp > "$dir/snmp-end.txt"
  cat /proc/net/dev > "$dir/netdev-end.txt"
  date -u '+finished_utc: %FT%TZ' >> "$dir/meta.txt"
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
{
  date -u '+started_utc: %FT%TZ'
  echo "role: $role; peer: $peer; private_ip: $local_ip; interface: $iface"
  echo "cpus: $cpus; rates: $rates; step: $step; response_bytes: 262144"
  echo 'GOGC: 100; GOMAXPROCS: 2; timezone: UTC'
  hostname; uname -a; nproc; free -m; ip -br -4 addr
  echo "$route"
  timedatectl show -p NTPSynchronized 2>/dev/null || true
  sha256sum "$binary"
  git rev-parse HEAD 2>/dev/null || true
} > "$dir/meta.txt"
if [[ $role == server ]]; then
  GOGC=100 GOMAXPROCS=2 taskset -c "$cpus" "$binary" -addr 0.0.0.0:8080 > "$dir/enrolld.log" 2>&1 &
  service_pid=$!
  pids+=("$service_pid")
  ready=0
  for ((i=0;i<50;i++)); do
    kill -0 "$service_pid" 2>/dev/null || break
    if curl --noproxy '*' -fsS -m 1 http://127.0.0.1:8080/config > "$dir/config.json" 2>/dev/null &&
       grep -Eq "\"pid\": *$service_pid([,[:space:]]|$)" "$dir/config.json"; then ready=1; break; fi
    sleep 0.2
  done
  [[ $ready == 1 ]] || { cat "$dir/enrolld.log" >&2; exit 1; }
  curl --noproxy '*' -fsS -m 3 http://127.0.0.1:8080/metrics > "$dir/metrics-start.txt"
else
  curl --noproxy '*' -fsS -m 5 "http://$server_ip:8080/config" > "$dir/server-config.json"
  bytes=$(curl --noproxy '*' -fsS -m 5 -o /dev/null -w '%{size_download}' "http://$server_ip:8080/api/export?kb=256")
  [[ $bytes == 262144 ]] || { echo "unexpected response size: $bytes" >&2; exit 1; }
  echo "verified_response_bytes: $bytes" >> "$dir/meta.txt"
fi
cat /proc/net/snmp > "$dir/snmp-start.txt"
cat /proc/net/dev > "$dir/netdev-start.txt"
sar -n DEV,EDEV,TCP,ETCP 1 > "$dir/sar.log" 2>&1 & pids+=("$!")
mpstat -P ALL 1 > "$dir/mpstat.log" 2>&1 & pids+=("$!")
(
  while true; do
    date -u '+epoch=%s utc=%FT%TZ'
    ss -tin "$ss_filter"
    sleep 1
  done
) > "$dir/ss.log" 2>&1 & pids+=("$!")
(
  echo 'epoch,tx_bytes,rx_bytes,tx_packets,rx_packets,tx_dropped,rx_dropped'
  while true; do
    printf '%s' "$(date +%s)"
    for field in tx_bytes rx_bytes tx_packets rx_packets tx_dropped rx_dropped; do
      read -r value < "/sys/class/net/$iface/statistics/$field"
      printf ',%s' "$value"
    done
    printf '\n'
    sleep 1
  done
) > "$dir/net.csv" 2>&1 & pids+=("$!")
if [[ $role == server ]]; then
  echo "Server ready on $server_ip:8080; recording in $dir. Run the client on B."
  echo 'After the client finishes, press Ctrl+C here to stop this service and its collectors.'
  wait "$service_pid"
else
  GOGC=100 GOMAXPROCS=2 taskset -c "$cpus" "$binary" \
    -url "http://$server_ip:8080/api/export?kb=256" -rates "$rates" -step "$step" \
    -out "$dir/steps.csv" 2>&1 | tee "$dir/loadgen.log"
  echo "Finished: $dir. Stop the server collector with Ctrl+C on A."
fi
