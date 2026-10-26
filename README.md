# enroll-lab：实验 2 的统一示例

> 实验要求、任务步骤、检查点和评分见 Canvas 上发布的《实验 2：测出上限，看见饱和》指导书。本仓库只提供示例代码和工具，不含指导书。

一个简化的选课后端 `enrolld`，以及用于压测和整理数据的工具。读路径和写路径各含一个可以达到饱和的资源，用于练习 L-07 的资源三问（上限由什么决定、如何判断饱和、如何换算容量）和 L-08 的 USE、RED。**服务本身不是重点**，它的作用是提供一个结果可预测、也可测量的对象。

## 接口

| 方法与路径 | 作用 |
|---|---|
| `GET /api/courses/{id}` | 读路径：先执行一段纯计算（`-work`），再从“数据库连接池”（共 `-pool` 个连接）中占用一个连接 `-db-ms` 毫秒 |
| `POST /api/enroll` | 写路径：请求体 `{"student":"…","course":"…"}`；记录追加到 `data/enroll.log`，按 `-sync` 指定的方式持久化后才返回 |
| `GET /metrics` | Prometheus 格式的指标：RED、连接池、fsync、进程和 Go 运行时 |
| `GET /config` | 当前参数（JSON） |
| `GET /healthz` | 存活检查 |
| `GET /debug/pprof/` | Go 的性能剖析 |
| `GET /api/report?mb=4`、`GET /api/export?kb=256` | 供选做 O1（内存与 GC）、O2（网络）使用 |

“数据库”是模拟的：占用一个连接并休眠 `-db-ms` 毫秒，不消耗本机 CPU。`-sync` 有三种取值：`each` 每次提交执行一次 fsync；`group` 组提交；`interval` 只写入页缓存，每秒执行一次 fsync。`bin/enrolld -h` 列出全部参数。

## 构建

需要 Go 1.26 或更高版本、GNU make。没有第三方依赖，构建时无需联网。

```bash
make build test        # 生成 bin/enrolld、bin/loadgen、bin/promsteps，并运行测试
scripts/get-tools.sh   # 下载并校验 Prometheus 3.13.4、node_exporter 1.12.1，放到 tools/
```

## 文件

| 文件 | 说明 |
|---|---|
| `cmd/enrolld/` | 被测服务 |
| `cmd/loadgen/` | 开环压测：按到达率分阶段加压，输出每个阶段的吞吐、错误率、延迟分布 |
| `cmd/promsteps/` | 按 loadgen 的阶段查询 Prometheus，输出“阶段 × 查询”的表 |
| `internal/metrics/` | 仅用标准库输出 Prometheus 文本格式的指标，建议阅读 |
| `deploy/prometheus.yml` | Prometheus 配置 |
| `deploy/queries-red.txt`、`deploy/queries-use.txt` | 服务层和资源层的查询；前者有四行 TODO 需自行编写 |
| `scripts/run.sh` | 运行一次实验：加压并同时采集资源数据，结束后查询 Prometheus，结果存入 `runs/<名字>/` |
| `scripts/requery.sh` | 修改查询文件后，对已完成的实验重新查询 |
| `scripts/rates.sh`、`scripts/table.sh` | 生成各阶段的到达率；将一次实验的数据整理为 Markdown 表格 |
| `scripts/get-tools.sh` | 下载 Prometheus 和 node_exporter |
| `LAB2.md` | 小组的数据与分析记录，需要填写并提交 |
