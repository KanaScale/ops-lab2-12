#!/usr/bin/env bash
# 下载实验 2 用到的 Prometheus 和 node_exporter，核对 sha256 后解压到 tools/。
#
# 用法（在仓库根目录）：scripts/get-tools.sh
#
# 版本和 sha256 写死在下面，取自两个项目在 GitHub Release 上发布的 sha256sums.txt。
# GitHub 下载太慢时：用任何办法拿到同名的 .tar.gz（例如在自己电脑上下载后 scp 过来），放进 tools/ 再运行本脚本，
# 它会直接校验这个文件，不再下载。校验不通过的文件一律不用。
set -euo pipefail

PROM_VER=3.13.4    # Prometheus 3.13 是长期支持版（LTS），支持到 2027-07-31
NODE_VER=1.12.1

case $(uname -m) in
  x86_64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "不支持的 CPU 架构：$(uname -m)" >&2; exit 1 ;;
esac

declare -A SUM=(
  [prometheus-$PROM_VER.linux-amd64]=87f21a66f96c597a189cef8d640e8921b621fc17a9feff707c332c4f3b3ddd56
  [prometheus-$PROM_VER.linux-arm64]=ffa86c7d6e7f7e7dc8a79c81c5c2a6c9164464b79335f25613f4327c863b8489
  [node_exporter-$NODE_VER.linux-amd64]=b51d8a76aa2a9156a55d501aca6276fae09e262259a5e4e831d2c2222f084e63
  [node_exporter-$NODE_VER.linux-arm64]=ad35b605f9954b9f1ffddf5ba054bdc5a98d790b9eae5291e1eeb83f1ecbd0e7
)

mkdir -p tools
fetch() { # fetch 名字 下载地址
  local name=$1 url=$2 tgz=tools/$1.tar.gz
  if [ -x "tools/$name/${name%%-*}" ]; then
    echo "已有 tools/$name，跳过"
    return
  fi
  if [ ! -f "$tgz" ]; then
    echo "下载 $url"
    curl -fL --retry 3 --connect-timeout 15 -o "$tgz.part" "$url"
    mv "$tgz.part" "$tgz"
  fi
  if ! echo "${SUM[$name]}  $tgz" | sha256sum -c --quiet -; then
    echo "校验失败：$tgz 不是官方发布的文件，已删除。请重新下载。" >&2
    rm -f "$tgz"
    exit 1
  fi
  tar -xzf "$tgz" -C tools
  rm -f "$tgz"
  echo "已解压到 tools/$name"
}

fetch "prometheus-$PROM_VER.linux-$arch" \
  "https://github.com/prometheus/prometheus/releases/download/v$PROM_VER/prometheus-$PROM_VER.linux-$arch.tar.gz"
fetch "node_exporter-$NODE_VER.linux-$arch" \
  "https://github.com/prometheus/node_exporter/releases/download/v$NODE_VER/node_exporter-$NODE_VER.linux-$arch.tar.gz"

ln -sfn "prometheus-$PROM_VER.linux-$arch/prometheus" tools/prometheus
ln -sfn "node_exporter-$NODE_VER.linux-$arch/node_exporter" tools/node_exporter
tools/prometheus --version | head -n 1
tools/node_exporter --version 2>&1 | head -n 1
