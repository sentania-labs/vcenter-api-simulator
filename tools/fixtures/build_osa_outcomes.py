#!/usr/bin/env python3
"""Extract OSA wildcard outcome classes and label order from a sanitized walk.

Usage: build_osa_outcomes.py SANITIZED_VSAN_DIRECTORY OUTPUT_JSON
The generated fixture excludes source identities, timestamps and sample values.
"""
import glob
import json
import os
import sys

source, destination = sys.argv[1:]
cases = []
for path in sorted(glob.glob(os.path.join(source, 'VsanPerfQueryPerf-*.json'))):
    name = os.path.basename(path)[len('VsanPerfQueryPerf-'):-len('.json')]
    with open(path) as f:
        response = json.load(f)
    case = {'family': name}
    if isinstance(response, dict) and '_fault' in response:
        fault = response['_fault']
        case['outcome'] = 'fault'
        case['fault'] = fault['fault'].rsplit('.', 1)[-1]
        if 'invalidProperty' in fault:
            case['invalidProperty'] = fault['invalidProperty']
    elif response and response[0].get('value'):
        case['outcome'] = 'samples'
        case['entities'] = len(response)
        case['labels'] = [series['metricId']['label'] for series in response[0]['value']]
    else:
        case['outcome'] = 'empty'
    cases.append(case)
with open(destination, 'w') as f:
    json.dump(cases, f, separators=(',', ':'))
    f.write('\n')
