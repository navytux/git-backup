#!/usr/bin/env python
# make corruption to c6c31ba413a4588cac7f77919bfcbe4adbf1d3b4 loose object
import os, zlib

def readfile(path):
    with open(path, 'r') as f:
        return f.read()

def writefile(path, data):
    try:
        os.unlink(path)
    except OSError:
        pass
    with open(path, 'w') as f:
        f.write(data)


z = readfile("c6c31ba413a4588cac7f77919bfcbe4adbf1d3b4.orig")
print `z`

d = zlib.decompress(z)
print `d`

D = d.replace('good', 'BAAD')
print `D`

Z = zlib.compress(D)
print `Z`

writefile("c6/c31ba413a4588cac7f77919bfcbe4adbf1d3b4", Z)
