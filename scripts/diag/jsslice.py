#!/usr/bin/env python3
"""Print a byte-offset slice of a huge minified JS file.

Usage: jsslice.py <file> <start> <length>
"""
import sys

path, start, length = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
with open(path, 'r', encoding='utf-8', errors='replace') as fh:
    fh.seek(0)
    data = fh.read()
print(data[start:start + length].replace('\n', '\\n'))
