#!/bin/bash
# 构建 GLIBC_2.38 兼容垫片（见 shim.c 注释）
set -euo pipefail
cd "$(dirname "$0")"
gcc -shared -fPIC -O2 -std=gnu11 -o libglibc238shim.so shim.c -ldl -Wl,--version-script=shim.map
echo "built $(pwd)/libglibc238shim.so"
readelf --dyn-syms -W libglibc238shim.so | grep -E 'fmod|isoc23' || true
