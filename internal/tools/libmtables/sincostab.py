#!/usr/bin/env python3
"""Generate pkg/sqlfn/glibc_sincostab_gen.go from glibc 2.31's sysdeps/ieee754/dbl-64/sincostab.c.

Usage: sincostab.py <sincostab.c> > pkg/sqlfn/glibc_sincostab_gen.go

The table is read from its big-endian half, where each double is its high word then its low word.
"""
import re, sys

src = open(sys.argv[1]).read()
big = src[src.index('#ifdef BIG_ENDI'):src.index('#else')]
words = re.findall(r'0x[0-9A-Fa-f]{8}', big[big.index('{ .i = {'):])
assert len(words) == 880, len(words)
vals = ['0x%08x%08x' % (int(words[i], 16), int(words[i + 1], 16)) for i in range(0, 880, 2)]
out = ['// Code generated from glibc 2.31 sysdeps/ieee754/dbl-64/sincostab.c. DO NOT EDIT.', '',
       'package sqlfn', '',
       '// glibcSinCosTab is __sincostab: for each table point, sin, its correction, cos, its correction.',
       'var glibcSinCosTab = [440]uint64{']
for i in range(0, 440, 4):
    out.append('\t' + ', '.join(vals[i:i + 4]) + ',')
out.append('}')
print('\n'.join(out))
