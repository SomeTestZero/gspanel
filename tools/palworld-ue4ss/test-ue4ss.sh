#!/bin/bash
# 隔离试跑：只硬链接只读游戏大文件；整个 Pal/Saved、日志、mod/so/布局均独立。
# 用法: sudo NATIVE_CHECK=1 RUN_SECS=180 ./test-ue4ss.sh <实例目录> <libUE4SS.so> [端口基数]
# 不启停生产实例。通过 transient systemd unit 限制测试服内存/CPU，退出必停止。
set -euo pipefail
SRC=$(realpath "${1:?生产实例目录}")
UE4SS_SO=$(realpath "${2:?libUE4SS.so 路径}")
BASE=${3:-18300}
RUN_SECS=${RUN_SECS:-180}
ASSETS="$(cd "$(dirname "$0")/../.." && pwd)/assets/palworld-ue4ss"
[ "$(id -u)" = 0 ] || { echo "需要 root"; exit 1; }
[[ "$BASE" =~ ^[0-9]+$ && "$RUN_SECS" =~ ^[0-9]+$ ]] || exit 1
[ "$BASE" -ge 1024 ] && [ "$BASE" -le 65400 ] || exit 1
[ -f "$SRC/Pal/Binaries/Linux/PalServer-Linux-Shipping" ] && [ -f "$UE4SS_SO" ] || exit 1
DST=$(mktemp -d /home/games/gsp-ue4ss-test.XXXXXX)
UNIT="gsp-ue4ss-test-$(basename "$DST" | cut -d. -f2)"
cleanup() { systemctl stop "$UNIT.service" 2>/dev/null || true; }
trap cleanup EXIT INT TERM
printf 'TEST_DIR=%s\nTEST_UNIT=%s\n' "$DST" "$UNIT"

# 旧脚本 cp -al 后直接 cp 覆盖 so/mod，会写坏生产硬链接！先 unlink 全部可变文件。
cp -al "$SRC/." "$DST/"
rm -rf "$DST/Pal/Saved" "$DST/logs" "$DST/steamapps" "$DST/Engine/Saved"
rm -f "$DST/start.sh" "$DST/start.sh.pre-ue4ss" "$DST/MemberVariableLayout.ini" "$DST/VTableLayout.ini"
BIN="$DST/Pal/Binaries/Linux"
rm -rf "$BIN/Mods" "$BIN/gspanel-mod" "$BIN/gspanel-runtime"
rm -f "$BIN/libUE4SS.so" "$BIN/libgxxfix.so" "$BIN/UE4SS-settings.ini" \
  "$BIN/MemberVariableLayout.ini" "$BIN/VTableLayout.ini" "$BIN/UE4SS.log" "$BIN/steam_appid.txt"
mkdir -p "$BIN/Mods/gspanel/scripts" "$BIN/gspanel-mod" "$DST/Pal/Saved/Config/LinuxServer" "$DST/logs"
# 干净测试世界，不复制生产存档/密码，也不向公网发布。
printf '[/Script/Pal.PalGameWorldSettings]\nOptionSettings=(ServerName="GSPanel-UE4SS-TEST",ServerPassword="isolated-local-test",AdminPassword="isolated-test-admin",PublicPort=%d,RCONEnabled=False,RESTAPIEnabled=True,RESTAPIPort=%d,bIsUseBackupSaveData=False)\n' \
  "$((BASE+11))" "$((BASE+12))" > "$DST/Pal/Saved/Config/LinuxServer/PalWorldSettings.ini"
install -m 755 "$UE4SS_SO" "$BIN/libUE4SS.so"
install -m 755 "$ASSETS/libgxxfix.so" "$BIN/libgxxfix.so"
install -m 644 "$ASSETS/mod/scripts/main.lua" "$BIN/Mods/gspanel/scripts/main.lua"
install -m 644 "$ASSETS/mods.txt" "$BIN/Mods/mods.txt"
install -m 644 "$ASSETS/UE4SS-settings.ini" "$BIN/UE4SS-settings.ini"
for f in MemberVariableLayout.ini VTableLayout.ini; do
  install -m 644 "$ASSETS/layouts/$f" "$BIN/$f"
  install -m 644 "$ASSETS/layouts/$f" "$DST/$f"
done
printf '2394010\n' > "$BIN/steam_appid.txt"
# 只 chown 新建文件，别对仍链接生产文件的整棵树递归 chown/chmod。
find "$DST" -type d -exec chown games:games {} +
find "$DST" -type f -links 1 -exec chown games:games {} +
luac5.4 -p "$BIN/Mods/gspanel/scripts/main.lua"

systemd-run --unit="$UNIT" --collect --uid=games --working-directory="$DST" \
  --property="RuntimeMaxSec=${RUN_SECS}s" --property=TimeoutStopSec=20 \
  --property=MemoryMax=1600M --property=MemorySwapMax=2G --property=CPUQuota=100% \
  --property="StandardOutput=append:$DST/logs/console.log" \
  --property="StandardError=append:$DST/logs/console.log" \
  --property=IPAddressDeny=any --property=IPAddressAllow=localhost \
  /usr/bin/env HOME=/home/games "LD_PRELOAD=$BIN/libgxxfix.so:$BIN/libUE4SS.so" \
  "$BIN/PalServer-Linux-Shipping" Pal -port="$((BASE+11))" -useperfthreads -NoAsyncLoadingThread -UseMultithreadForDS
if [ "${NATIVE_CHECK:-0}" = 1 ]; then
  # games 不一定能遍历面板用户的 home，复制测试驱动到它可读的隔离目录。
  install -m 644 -o games -g games "$(dirname "$0")/test-native.py" "$DST/native-check.py"
  sudo -u games python3 "$DST/native-check.py" "$DST"
  echo "PASS: 隔离原生回归通过"
  exit 0
fi
# 本脚本由 bg_run 管理；等待有上限，Ctrl-C/超时都会清理该 unit。
for ((i=0; i<RUN_SECS; i++)); do
  sleep 1
  systemctl is-active --quiet "$UNIT.service" || break
done
cleanup
printf '测试结束；独立日志/目录保留在 %s（确认无需排查后可删除）。\n' "$DST"
grep -aE '\[gspanel\]|Linux: UObject::ProcessEvent|SIG|segfault|Fatal' "$DST/logs/console.log" | tail -40 || true
