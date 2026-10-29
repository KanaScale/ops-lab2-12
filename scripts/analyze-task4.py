#!/usr/bin/env python3
"""Recompute task 4 derivations from committed CSVs without changing raw data."""
import csv
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def rows(run, file):
    with (ROOT / 'runs' / run / file).open(encoding='utf-8', newline='') as stream:
        return {row['step']: row for row in csv.DictReader(stream)}


def main():
    r1 = rows('r1-pool4', 'red.csv')
    s1 = rows('r1-pool4', 'steps.csv')
    print('Little law: step, lambda*mean_seconds, sampled_inflight, relative_difference')
    for step in ('1', '6', '9'):
        row = r1[step]
        estimate = float(row['server_rps']) * float(row['server_mean_ms']) / 1000
        measured = float(row['inflight_avg'])
        print(f'{step}: {estimate:.6f}, {measured:g}, {(measured / estimate - 1):+.2%}')
    stage_p99 = [float(row['p99_ms']) for step, row in s1.items() if step != 'all']
    print(f'Unweighted mean of stage p99: {sum(stage_p99) / len(stage_p99):.6f} ms')
    print(f"Actual merged p99: {s1['all']['p99_ms']} ms")
    print(f'SLO capacity / measured peak: {float(s1["5"]["ok_rps"]) / float(s1["7"]["ok_rps"]):.4%}')
    print(f'Prediction error R1: {float(s1["7"]["ok_rps"]) / 384 - 1:+.4%}')
    print('Latency deltas: step, client_mean-server_mean, client_p99-server_p99 (ms)')
    for step in ('1', '6', '8'):
        print(step, *(f'{float(s1[step][client]) - float(r1[step][server]):.6f}'
                      for client, server in (('mean_ms', 'server_mean_ms'), ('p99_ms', 'server_p99_ms'))))
    first_bad = next(row for step, row in s1.items()
                     if step != 'all' and float(row['slo_good']) < 0.99)
    burn = (1 - float(first_bad['slo_good'])) / 0.01
    print(f'First bad step: {first_bad["step"]}; burn: {burn:.6f}; budget days: {30 / burn:.6f}')
    w1 = rows('w1-each', 'red.csv')['6']
    u1 = rows('w1-each', 'use.csv')['6']
    print(f'Writes/fsync: {float(u1["disk_w_per_s"]) / float(w1["fsync_per_s"]):.6f}')
    print(f'Flushes/fsync: {float(u1["disk_f_per_s"]) / float(w1["fsync_per_s"]):.6f}')
    w2 = rows('w2-group', 'steps.csv')
    w3 = rows('w3-interval', 'steps.csv')
    sw1 = rows('w1-each', 'steps.csv')['6']
    print(f'Group high-load throughput / each peak: {float(w2["8"]["ok_rps"]) / float(sw1["ok_rps"]):.6f}')
    print(f'Group p99 reduction at 791 RPS: {1 - float(w2["4"]["p99_ms"]) / float(sw1["p99_ms"]):.4%}')
    print(f'Interval p99 speedup at 3160 RPS: {float(w2["8"]["p99_ms"]) / float(w3["3"]["p99_ms"]):.6f}')


if __name__ == '__main__':
    main()
