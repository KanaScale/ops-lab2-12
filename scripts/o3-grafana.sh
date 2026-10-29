#!/usr/bin/env bash
# Grafana 安装完成后，在旧服务器仓库根目录以 root 运行。
# 配置仅监听本机 3000；通过 SSH -L 3000:127.0.0.1:3000 访问。
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "请以 root 运行" >&2; exit 1; }
[[ -x /usr/share/grafana/bin/grafana ]] || {
  echo "Grafana 尚未通过 Debian/Ubuntu 软件包安装" >&2; exit 1;
}
curl -fsS -m 5 http://127.0.0.1:9090/-/ready >/dev/null || {
  echo "本机 Prometheus 9090 尚未就绪" >&2; exit 1;
}

install -d -m 755 /etc/grafana/provisioning/datasources
install -d -m 755 /etc/grafana/provisioning/dashboards
install -d -m 755 /var/lib/grafana/dashboards/lab2
install -d -m 755 /etc/systemd/system/grafana-server.service.d

cat >/etc/grafana/provisioning/datasources/lab2-prometheus.yaml <<'YAML'
apiVersion: 1
datasources:
  - name: Lab2 Prometheus
    uid: lab2-prometheus
    type: prometheus
    access: proxy
    url: http://127.0.0.1:9090
    isDefault: false
    editable: false
YAML

cat >/etc/grafana/provisioning/dashboards/lab2.yaml <<'YAML'
apiVersion: 1
providers:
  - name: Lab2
    orgId: 1
    folder: Lab2
    type: file
    disableDeletion: false
    updateIntervalSeconds: 30
    options:
      path: /var/lib/grafana/dashboards/lab2
YAML

python3 - <<'PY'
import json
from pathlib import Path
source = Path("deploy/o3-dashboard.json")
target = Path("/var/lib/grafana/dashboards/lab2/o3-dashboard.json")
dashboard = json.loads(source.read_text(encoding="utf-8"))
dashboard.pop("__inputs", None)
dashboard.pop("__requires", None)
for panel in dashboard["panels"]:
    panel["datasource"]["uid"] = "lab2-prometheus"
target.write_text(json.dumps(dashboard, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
PY
chown -R grafana:grafana /var/lib/grafana/dashboards/lab2

# 不开放公网 3000；浏览器从本机经 SSH 隧道进入。
cat >/etc/systemd/system/grafana-server.service.d/lab2-local.conf <<'CONF'
[Service]
Environment=GF_SERVER_HTTP_ADDR=127.0.0.1
CONF
systemctl daemon-reload
systemctl restart grafana-server

for ((i=0; i<20; i++)); do
  if curl -fsS -m 2 http://127.0.0.1:3000/api/health; then
    echo
    echo "看板地址（需本地 SSH 隧道）：http://127.0.0.1:3000/d/lab2-enrolld-o3"
    exit 0
  fi
  sleep 2
done
echo "Grafana 未在 40 秒内就绪，请查看 journalctl -u grafana-server -n 80" >&2
exit 1
