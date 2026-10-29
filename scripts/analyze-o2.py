#!/usr/bin/env python3
"""Summarize O2 client stages against both hosts' per-second network and CPU samples."""
import csv
import datetime as dt
import statistics
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
CLIENT = ROOT / 'runs/o2-net-client'
SERVER = ROOT / 'runs/o2-net-server'
DATE = dt.date.fromisoformat(next(line.split(': ', 1)[1][:10]
    for line in (CLIENT / 'meta.txt').read_text(encoding='utf-8').splitlines()
    if line.startswith('started_utc: ')))


def rows(path):
    with path.open(encoding='utf-8', newline='') as stream:
        return list(csv.DictReader(stream))


def net_rows(path):
    return {int(r['epoch']): r for r in rows(path)}


def cpu_rows(path):
    out = {}
    with path.open(encoding='utf-8') as stream:
        for line in stream:
            parts = line.split()
            if len(parts) != 12 or parts[1] != 'all' or parts[0].count(':') != 2:
                continue
            clock = dt.time.fromisoformat(parts[0])
            epoch = int(dt.datetime.combine(DATE, clock, dt.timezone.utc).timestamp())
            out[epoch] = {'busy': 100 - float(parts[11]), 'soft': float(parts[7]),
                          'idle': float(parts[11])}
    return out


def retrans_rows(path):
    out = {}
    metric = ''
    with path.open(encoding='utf-8') as stream:
        for line in stream:
            parts = line.split()
            if 'retrans/s' in parts:
                metric = 'retrans'
            elif parts and (parts[0] == 'Linux' or 'IFACE' in parts or 'active/s' in parts):
                metric = ''
            elif metric == 'retrans' and len(parts) == 6 and parts[0].count(':') == 2:
                clock = dt.time.fromisoformat(parts[0])
                epoch = int(dt.datetime.combine(DATE, clock, dt.timezone.utc).timestamp())
                out[epoch] = float(parts[3])
    return out


def sar_tx_rows(path):
    out = {}
    metric = ''
    with path.open(encoding='utf-8') as stream:
        for line in stream:
            parts = line.split()
            if 'rxpck/s' in parts:
                metric = 'dev'
            elif 'IFACE' in parts or 'active/s' in parts or 'retrans/s' in parts:
                metric = ''
            elif metric == 'dev' and len(parts) == 10 and parts[1] == 'eth0' and parts[0].count(':') == 2:
                clock = dt.time.fromisoformat(parts[0])
                epoch = int(dt.datetime.combine(DATE, clock, dt.timezone.utc).timestamp())
                out[epoch] = float(parts[5])
    return out


def mean_in(data, lo, hi, key=None):
    values = [row[key] if key else row for t, row in data.items() if lo <= t <= hi]
    return statistics.mean(values) if values else float('nan')


def bandwidth(data, lo, hi, column):
    points = [(t, int(r[column])) for t, r in data.items() if lo <= t <= hi]
    points.sort()
    return (points[-1][1] - points[0][1]) * 8 / (points[-1][0] - points[0][0]) / 1e6


def main():
    steps = [r for r in rows(CLIENT / 'steps.csv') if r['step'] != 'all']
    snet, cnet = net_rows(SERVER / 'net.csv'), net_rows(CLIENT / 'net.csv')
    scpu, ccpu = cpu_rows(SERVER / 'mpstat.log'), cpu_rows(CLIENT / 'mpstat.log')
    sretrans, cretrans = retrans_rows(SERVER / 'sar.log'), retrans_rows(CLIENT / 'sar.log')
    stx = sar_tx_rows(SERVER / 'sar.log')
    print('step target ok_rps p99_ms timeout dropped server_tx_Mbps client_rx_Mbps sar_tx_kBs server_cpu_busy client_cpu_busy client_softirq server_retrans_s client_retrans_s')
    for r in steps:
        lo, hi = int(r['start_unix']) + 5, int(r['end_unix'])
        values = [r['step'], r['target_rps'], r['ok_rps'], r['p99_ms'], r['timeout'], r['dropped'],
                  f"{bandwidth(snet, lo, hi, 'tx_bytes'):.1f}", f"{bandwidth(cnet, lo, hi, 'rx_bytes'):.1f}",
                  f"{mean_in(stx, lo, hi):.0f}",
                  f"{mean_in(scpu, lo, hi, 'busy'):.1f}", f"{mean_in(ccpu, lo, hi, 'busy'):.1f}",
                  f"{mean_in(ccpu, lo, hi, 'soft'):.1f}", f"{mean_in(sretrans, lo, hi):.2f}",
                  f"{mean_in(cretrans, lo, hi):.2f}"]
        print(' '.join(values))


if __name__ == '__main__':
    main()
