# O2 实验操作

主机 A：旧服务器 `60.204.193.54`，内网 `192.168.0.171`，运行 enrolld。
主机 B：新服务器 `124.70.147.36`，内网 `192.168.0.16`，运行 loadgen。
两台的 VPC ID 已由用户核对相同。下面的压测地址只使用内网。

## 1. 临时安全组规则

在华为云控制台进入 A 的安全组入方向规则，添加允许 IPv4、自定义 TCP、端口 8080、源地址 `192.168.0.16/32`，描述 `lab2-o2-temp`。不要给公网 `0.0.0.0/0` 开放 8080；核对已有规则没有对公网放行该端口。B 不需要开放入方向 8080。完成实验后删除这条临时规则。

## 2. 准备两台机器

新服务器 B 的终端执行（root）：

```bash
mkdir -p /root/ops-lab2-12/bin /root/ops-lab2-12/scripts
apt-get update
apt-get install -y sysstat curl
```

本地 Windows PowerShell 执行，把 A 已编译的 Linux 客户端复制给 B，无需在 B 安装 Go：

```powershell
Set-Location -LiteralPath 'D:\笔记\大三上\互联网产品开发运维\labs\ops-lab2-12'
New-Item -ItemType Directory -Force -Path .\bin | Out-Null
scp root@60.204.193.54:/root/ops-lab2-12/bin/loadgen .\bin\loadgen-o2-linux
scp .\bin\loadgen-o2-linux root@124.70.147.36:/root/ops-lab2-12/bin/loadgen
scp .\scripts\o2.sh root@60.204.193.54:/root/ops-lab2-12/scripts/o2.sh
scp .\scripts\o2.sh root@124.70.147.36:/root/ops-lab2-12/scripts/o2.sh
```

## 3. 启动 A

先停止 A 上原有 enrolld（回到它的终端按 Ctrl+C），避免占用 8080；其他组员此时不要运行实验。A 终端执行：

```bash
cd /root/ops-lab2-12
bash scripts/o2.sh server o2-net
```

看到 `Server ready` 后再启动 B。脚本使用 A 的 CPU 0–1，在后台启动自己的服务和采集器，终端保持运行。它不会结束其他人的进程，也不会启动 Prometheus 或 node_exporter；本次不依赖它们。

## 4. 在 B 发起压测

B 终端执行：

```bash
cd /root/ops-lab2-12
chmod +x bin/loadgen
bash scripts/o2.sh client o2-net
```

脚本先验证内网路由和 262144 字节的响应，再以 `50,100,200,400,600,800,950,1100,1400` 请求/秒运行，每阶段 30 秒，总计约 4.5 分钟；严重过载时 loadgen 会提前停止后续阶段。两台记录 `sar.log`、`ss.log`、`mpstat.log`、网卡累计计数 `net.csv` 和主机信息；B 额外保存 `steps.csv` 与 `loadgen.log`。`ss.log` 内若出现过滤表达式错误，应修复后重跑。

如果 B 报连接失败，先核对 A 的 `Server ready`、安全组来源 `/32` 和两台的内网路由，不要改用公网地址压测。脚本拒绝覆盖已有结果；失败重试时两端统一换成新名字，如 `o2-net-2`，保留失败日志。

## 5. 停止与回收数据

B 显示 `Finished` 后，回 A 的脚本终端按 Ctrl+C。脚本会结束本次服务与采集器，保存结束时计数；Ctrl+C 退出码 130 是正常停止。然后删除第 1 步的 `lab2-o2-temp` 安全组规则。

本地 PowerShell（仓库目录）执行：

```powershell
scp -r root@60.204.193.54:/root/ops-lab2-12/runs/o2-net-server .\runs\
scp -r root@124.70.147.36:/root/ops-lab2-12/runs/o2-net-client .\runs\
```

拉取后再分析；如果没有出现网络平台或 CPU 先满，按数据决定下一次负载，不直接把最高目标 RPS 当作带宽上限。更名重试时以上拉取目录也要相应改名。
