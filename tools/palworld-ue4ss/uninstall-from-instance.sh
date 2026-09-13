#!/bin/bash
# 从 Palworld 实例卸载 UE4SS（恢复原始 start.sh，保留 mod 文件与布局表以便重装）。
set -euo pipefail
INST=${1:?用法: uninstall-from-instance.sh <实例目录>}
BIN="$INST/Pal/Binaries/Linux"
[ "$(id -u)" = 0 ] || { echo "需要 root（或用 sudo）"; exit 1; }
if [ -f "$INST/start.sh.pre-ue4ss" ]; then
  cp -a "$INST/start.sh.pre-ue4ss" "$INST/start.sh"
  chown games:games "$INST/start.sh"
  echo "已恢复 $INST/start.sh（重启实例生效）"
else
  echo "没有找到 $INST/start.sh.pre-ue4ss，请手动恢复 start.sh"
fi
