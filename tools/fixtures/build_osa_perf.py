#!/usr/bin/env python3
"""Derive OSA performance labels and value envelopes from a sanitized walk.

Usage: build_osa_perf.py SANITIZED_VSAN_DIRECTORY OUTPUT_JSON
The output intentionally contains no captured entity identifiers or timestamps.
"""
import glob
import json
import math
import os
import sys

source, dest = sys.argv[1:]
families = {}
for path in sorted(glob.glob(os.path.join(source, 'VsanPerfQueryPerf-*.json'))):
    name = os.path.basename(path)[len('VsanPerfQueryPerf-'):-len('.json')]
    with open(path) as f:
        result = json.load(f)
    if not isinstance(result, list) or not result or not result[0].get('value'):
        continue
    labels = [series['metricId']['label'] for series in result[0]['value']]
    stats = {}
    none = []
    for label in labels:
        values = []
        for entity in result:
            for series in entity.get('value', []):
                if series['metricId']['label'] == label:
                    for v in series['values'].split(','):
                        if v == 'None':
                            continue
                        values.append(float(v))
        if not values:
            none.append(label)
            continue
        values.sort()
        def percentile(p):
            return values[math.floor((len(values)-1)*p)]
        stats[label] = {'min': values[0], 'p25': percentile(.25), 'p50': percentile(.5), 'p75': percentile(.75), 'max': values[-1], 'n': len(values)}
    families[name] = {'labels': labels, 'stats': stats, 'none': none}
with open(dest, 'w') as f:
    json.dump(families, f, sort_keys=True, separators=(',', ':'))
    f.write('\n')
