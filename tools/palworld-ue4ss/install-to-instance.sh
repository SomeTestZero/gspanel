#!/bin/bash
# 把编译好的 UE4SS + gspanel 扩展命令 mod 安装到 Palworld 实例，并改写 start.sh。
#
# 用法: sudo ./install-to-instance.sh <实例目录> [libUE4SS.so 路径]
#   .so 默认 /home/games/ue4ss-build/libUE4SS.so（可用 build-ue4ss.sh 生成）
# 卸载: sudo ./uninstall-from-instance.sh <实例目录>
#
# 说明：Palworld 启动后会把进程 cwd 切到 Pal/Binaries/Linux，
#       所以 mod 的文件队列目录是 <实例>/Pal/Binaries/Linux/gspanel-mod。
set -euo pipefail
INST=${1:?用法: install-to-instance.sh <实例目录> [libUE4SS.so]}
SO=${2:-/home/games/ue4ss-build/libUE4SS.so}
TOOLDIR="$(cd "$(dirname "$0")" && pwd)"
BIN="$INST/Pal/Binaries/Linux"

[ "$(id -u)" = 0 ] || { echo "需要 root（或用 sudo）"; exit 1; }
[ -x "$BIN/PalServer-Linux-Shipping" ] || { echo "不是 Palworld 实例目录: $INST"; exit 1; }
[ -f "$SO" ] || { echo "找不到 libUE4SS.so: $SO（先跑 build-ue4ss.sh）"; exit 1; }

echo "== 1/4 备份 start.sh（只备份一次）"
[ -f "$INST/start.sh.pre-ue4ss" ] || cp -a "$INST/start.sh" "$INST/start.sh.pre-ue4ss"

echo "== 2/4 安装 libUE4SS.so / 布局表 / 设置 / mod"
# Lua 语法必须先验：UE4SS 的语法错误会抛 C++ 异常直接 abort 整个游戏进程
if command -v luac5.4 >/dev/null; then
  luac5.4 -p "$TOOLDIR/mod/scripts/main.lua" || { echo "mod 有 Lua 语法错误，已中止"; exit 1; }
fi
cp "$SO" "$BIN/libUE4SS.so"
cp "$TOOLDIR/layouts/MemberVariableLayout.ini" "$BIN/MemberVariableLayout.ini"
cp "$TOOLDIR/layouts/VTableLayout.ini" "$BIN/VTableLayout.ini"
cp "$TOOLDIR/UE4SS-settings.ini" "$BIN/UE4SS-settings.ini"
# 实例根也放一份（UE4SS 的工作目录兼容）
cp "$TOOLDIR/layouts/MemberVariableLayout.ini" "$INST/MemberVariableLayout.ini"
cp "$TOOLDIR/layouts/VTableLayout.ini" "$INST/VTableLayout.ini"
mkdir -p "$BIN/Mods/gspanel/scripts" "$BIN/gspanel-mod"
cp "$TOOLDIR/mod/scripts/main.lua" "$BIN/Mods/gspanel/scripts/main.lua"
printf 'gspanel : 1\n' > "$BIN/Mods/mods.txt"

echo "== 3/4 生成 start.sh（LD_PRELOAD 只对游戏二进制生效，绝不能进 shell）"
cat > "$INST/start.sh" <<'EOF'
#!/bin/bash
# 由 gspanel / tools/palworld-ue4ss 安装的 UE4SS 启动脚本
# LD_PRELOAD 只对游戏二进制生效；如果让它进入 bash/PalServer.sh，会因 UE4SS 构造器段错误
cd "$(dirname "$0")"
if [ ! -f Pal/Binaries/Linux/steamclient.so ]; then cp linux64/steamclient.so Pal/Binaries/Linux/steamclient.so 2>/dev/null || true; fi
chmod +x Pal/Binaries/Linux/PalServer-Linux-Shipping 2>/dev/null || true
exec env LD_PRELOAD="$PWD/Pal/Binaries/Linux/libUE4SS.so" Pal/Binaries/Linux/PalServer-Linux-Shipping Pal -useperfthreads -NoAsyncLoadingThread -UseMultithreadForDS
EOF
chmod 755 "$INST/start.sh"

echo "== 4/4 属主改回 games"
chown -R games:games "$BIN/libUE4SS.so" "$BIN/MemberVariableLayout.ini" "$BIN/VTableLayout.ini" \
  "$BIN/UE4SS-settings.ini" "$BIN/Mods" "$BIN/gspanel-mod" "$INST/MemberVariableLayout.ini" \
  "$INST/VTableLayout.ini" "$INST/start.sh" 2>/dev/null || true

echo "完成。重启实例后检查日志里是否出现："
echo "  [UE4SS] FName::FName 解析器命中 / Linux: full mode post-init done"
echo "  [Lua] [gspanel] mod loaded"
echo "面板：实例控制台页会多出「扩展: 在线玩家 / 给物品 / 给经验」按钮。"
