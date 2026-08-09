#!/usr/bin/env python3
"""Extract byte-offset windows around a literal/regex match in huge minified JS.

Usage: jsgrep.py <file> <pattern> [before] [after] [max_hits]
Reads the file in chunks so it never loads 22 MB into the agent context.
"""
import re
import sys

path = sys.argv[1]
pat = sys.argv[2]
before = int(sys.argv[3]) if len(sys.argv) > 3 else 300
after = int(sys.argv[4]) if len(sys.argv) > 4 else 900
maxhits = int(sys.argv[5]) if len(sys.argv) > 5 else 20

with open(path, 'r', encoding='utf-8', errors='replace') as fh:
    data = fh.read()

for i, m in enumerate(re.finditer(pat, data)):
    if i >= maxhits:
        print(f"... more hits truncated at {maxhits}")
        break
    s = max(0, m.start() - before)
    e = min(len(data), m.end() + after)
    print(f"===== hit {i} @ offset {m.start()} =====")
    print(data[s:e].replace('\n', '\\n'))
    print()
