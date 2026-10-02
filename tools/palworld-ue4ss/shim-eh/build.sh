#!/bin/bash
# 构建 __gxx_personality_v0 拦截垫片 libgxxfix.so（见 gxxfix.c 注释）
set -euo pipefail
cd "$(dirname "$0")"
gcc -shared -fPIC -O2 -std=gnu11 -o libgxxfix.so gxxfix.c -ldl -Wl,--version-script=gxxfix.map
echo "built $(pwd)/libgxxfix.so"
readelf --dyn-syms -W libgxxfix.so | grep -E 'personality|UND' || true
