#!/bin/bash
# 在「硬链接测试副本」上试验 libUE4SS.so，不影响生产实例。
# 用法: sudo ./test-ue4ss.sh <生产实例目录> <libUE4SS.so> [测试端口基数]
#   例: sudo ./test-ue4ss.sh /home/games/instances/palworld-1 /tmp/ue4ss/libUE4SS.so 8300
# 说明:
#   1) 用 cp -al 硬链接复制实例（秒级、几乎不占空间），随后断开存档/日志/配置的硬链接，
#      测试服写盘不会影响生产存档；
#   2) 游戏/RCON/REST 端口改为 <基数+11>/<基数+76>/<基数+12>（默认 8311/2576/8312 会冲突时自行调整）;
#   3) 启动 60 秒后自动杀掉测试进程（可用 RUN_SECS 覆盖）。
# 注意：测试副本会占用一份游戏进程内存（约 1~2GB），本机只有 3.6G 内存时
#       建议挑玩家离线时执行，并在 free 掉到 300MB 以下时手动中止。
set -euo pipefail
SRC=${1:?生产实例目录}
UE4SS_SO=${2:?libUE4SS.so 路径}
BASE=${3:-8300}
RUN_SECS=${RUN_SECS:-60}
DST=/home/games/test-ue4ss

[ "$(id -u)" = 0 ] || { echo "需要 root"; exit 1; }
[ -d "$SRC" ] || { echo "实例目录不存在: $SRC"; exit 1; }

echo "== 1/4 硬链接复制到 $DST"
rm -rf "$DST"; mkdir -p "$DST"
cp -al "$SRC/." "$DST/"
# 断开可变数据
rm -rf "$DST/Pal/Saved/SaveGames" "$DST/logs"
rm -f "$DST/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"
cp "$SRC/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini" "$DST/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"
mkdir -p "$DST/logs"
sed -i "s/PublicPort=[0-9]*/PublicPort=$((BASE+11))/; s/RCONPort=[0-9]*/RCONPort=$((BASE+76))/; s/RESTAPIPort=[0-9]*/RESTAPIPort=$((BASE+12))/; s/ServerName=\"[^\"]*\"/ServerName=\"UE4SS-TEST\"/" \
  "$DST/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"

echo "== 2/4 安装 UE4SS + 垫片 + 新 libstdc++ + mod"
BIN="$DST/Pal/Binaries/Linux"; RT="$BIN/gspanel-runtime"
mkdir -p "$BIN/Mods/gspanel/scripts" "$RT"
cp "$UE4SS_SO" "$BIN/libUE4SS.so"
cp "$(dirname "$0")/shim/libglibc238shim.so" "$RT/" 2>/dev/null || { bash "$(dirname "$0")/shim/build.sh"; cp "$(dirname "$0")/shim/libglibc238shim.so" "$RT/"; }
cp "$(dirname "$0")/mod/scripts/main.lua" "$BIN/Mods/gspanel/scripts/main.lua"
cp "$(dirname "$0")/mods.txt" "$BIN/Mods/mods.txt"
cp "$(dirname "$0")/UE4SS-settings.ini" "$BIN/UE4SS-settings.ini"
# 新 libstdc++：从 PPA deb 解出来放这里（见 README），没有就先只用系统版本
if [ -f "${STDCPP_SO:-}" ]; then cp "$STDCPP_SO" "$RT/libstdc++.so.6"; fi
chown -R games:games "$DST"

echo "== 3/4 启动测试服（$RUN_SECS 秒后自动停止）"
LOG=/tmp/ue4ss-test.log; : > "$LOG"
if [ ! -f "$RT/libstdc++.so.6" ]; then
  echo "!! 警告：$RT 下没有新 libstdc++（GLIBCXX_3.4.31/32），libUE4SS 很可能加载失败；"
  echo "   下载 jammy 版 PPA deb 并用 STDCPP_SO=... 指定（见 README）"
fi
# LD_PRELOAD 只随最终 exec 生效；不要让 bash/PalServer.sh 吃到（它们不是 UE 进程，会段错误）
setsid sudo -u games bash -c "cd '$DST' && exec env HOME=/home/games LD_LIBRARY_PATH='$RT' LD_PRELOAD='$RT/libglibc238shim.so:$BIN/libUE4SS.so' ./Pal/Binaries/Linux/PalServer-Linux-Shipping Pal -useperfthreads -NoAsyncLoadingThread -UseMultithreadForDS" >>"$LOG" 2>&1 &

for i in $(seq 1 "$RUN_SECS"); do
  sleep 1
  if grep -qaE '\[gspanel\]|\[UE4SS\]' "$LOG"; then break; fi
  avail=$(free -m | awk '/Mem:/{print $7}')
  [ "$avail" -lt 250 ] && { echo "可用内存 <250MB，提前停止"; break; }
done

echo "== 4/4 停止测试进程 & 输出摘要"
for p in $(pgrep -f 'PalServer-Linux-Shipping' || true); do
  [ "$(readlink /proc/$p/cwd 2>/dev/null || true)" = "$DST" ] && kill -9 "$p" && echo "killed $p"
done
echo "--- UE4SS/gspanel 日志:"; grep -aE '\[UE4SS\]|\[gspanel\]' "$LOG" | head -40 || true
echo "--- 完整日志: $LOG"
echo "--- 清理测试副本: rm -rf $DST"
