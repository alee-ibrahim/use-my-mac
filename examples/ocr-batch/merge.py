#!/usr/bin/env python3
"""Reduce step: merge.py OUT IN [IN ...] — concatenates inputs in order."""
import sys

with open(sys.argv[1], "wb") as out:
    for p in sys.argv[2:]:
        with open(p, "rb") as f:
            out.write(f.read())
print("merged", len(sys.argv) - 2, "parts")
