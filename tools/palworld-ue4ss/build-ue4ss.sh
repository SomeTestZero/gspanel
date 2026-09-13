#!/bin/bash
# 从源码构建带 Palworld 1.0.4 修复的 libUE4SS.so（原生 Linux）。
#
# 环境（Ubuntu 22.04 / glibc 2.35 实测）：
#   sudo add-apt-repository -y ppa:ubuntu-toolchain-r/test
#   sudo apt-get install -y gcc-13 g++-13 cmake ninja-build
#   rustup（国内可用 https://mirrors.ustc.edu.cn/rust-static/rustup 安装）
#
# 用法: ./build-ue4ss.sh [源码目录，默认 /tmp/ue4ss-src]
# 产物: <源码>/build/Game__Dev__Linux64/lib/libUE4SS.so
set -euo pipefail
SRC=${1:-/tmp/ue4ss-src}
TOOLDIR="$(cd "$(dirname "$0")" && pwd)"
export PATH="$HOME/.cargo/bin:$PATH"

if [ ! -d "$SRC/.git" ]; then
  git clone --depth 1 -b linux-native https://github.com/XarminaEu/ue4ss-linux.git "$SRC"
fi
cd "$SRC"
echo "== 打补丁（Palworld 1.0.4 适配）"
git checkout -- . 2>/dev/null || true
git apply "$TOOLDIR/patches/ue4ss-linux-palworld-1.0.4.patch"

echo "== 配置 (需要 CMake >= 3.23；22.04 自带 3.22 需自行升级，或确认补丁已包含 FILE_SET 兼容)"
cmake -S . -B build -G Ninja \
  -DCMAKE_BUILD_TYPE=Game__Dev__Linux64 \
  -DUE4SS_GUI_ENABLED=OFF -DUE4SS_INPUT_ENABLED=OFF \
  -DCMAKE_C_COMPILER=gcc-13 -DCMAKE_CXX_COMPILER=g++-13
echo "== 编译（单线程，约 30-60 分钟）"
ninja -C build -j1 UE4SS
SO="$SRC/build/Game__Dev__Linux64/lib/libUE4SS.so"
echo "== 产物: $SO"
strip -o "${SO%.so}.stripped.so" "$SO"
ls -la "$SO" "${SO%.so}.stripped.so"
