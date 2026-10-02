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
ASSETS="$TOOLDIR/../../assets/palworld-ue4ss"  # 面板仓库内置资产（面板接口用的也是这一份）
BIN="$INST/Pal/Binaries/Linux"

[ "$(id -u)" = 0 ] || { echo "需要 root（或用 sudo）"; exit 1; }
[ -x "$BIN/PalServer-Linux-Shipping" ] || { echo "不是 Palworld 实例目录: $INST"; exit 1; }
[ -f "$SO" ] || { echo "找不到 libUE4SS.so: $SO（先跑 build-ue4ss.sh）"; exit 1; }
[ -f "$ASSETS/mod/scripts/main.lua" ] || { echo "找不到内置资产: $ASSETS（请在 gspanel 仓库根目录执行）"; exit 1; }

# 原子替换，不能 cp 截断正在运行的 mmap .so，也不能修改硬链接测试副本的源文件。
copy_asset() {
  local tmp
  tmp=$(mktemp "$(dirname "$2")/.gspanel-asset.XXXXXX")
  install -m "${3:-644}" -o games -g games "$1" "$tmp"
  mv -f "$tmp" "$2"
}

echo "== 1/4 备份 start.sh（只备份一次）"
[ -f "$INST/start.sh.pre-ue4ss" ] || cp -a "$INST/start.sh" "$INST/start.sh.pre-ue4ss"

echo "== 2/4 安装 libUE4SS.so / EH 垫片 / 布局表 / 设置 / mod"
# Lua 语法必须先验：加载失败会使扩展功能不可用；旧 EH 运行时还可能崩溃
if command -v luac5.4 >/dev/null; then
  luac5.4 -p "$ASSETS/mod/scripts/main.lua" || { echo "mod 有 Lua 语法错误，已中止"; exit 1; }
fi
copy_asset "$SO" "$BIN/libUE4SS.so" 755
# __gxx_personality_v0 拦截垫片：libsteam_api.so 导出坏的 personality 抢占进程级
# 符号解析，任何 C++ 异常 unwind 都会 abort 游戏进程；垫片把它转发回系统 libstdc++。
# 须排在 LD_PRELOAD 第一位（先于 libUE4SS.so）。见 tools/palworld-ue4ss/shim-eh/
if [ -f "$ASSETS/libgxxfix.so" ]; then
  copy_asset "$ASSETS/libgxxfix.so" "$BIN/libgxxfix.so" 755
else
  echo "警告: 缺少 $ASSETS/libgxxfix.so（可用 tools/palworld-ue4ss/shim-eh/build.sh 构建）"
fi
copy_asset "$ASSETS/layouts/MemberVariableLayout.ini" "$BIN/MemberVariableLayout.ini"
copy_asset "$ASSETS/layouts/VTableLayout.ini" "$BIN/VTableLayout.ini"
copy_asset "$ASSETS/UE4SS-settings.ini" "$BIN/UE4SS-settings.ini"
# 实例根也放一份（UE4SS 的工作目录兼容）
copy_asset "$ASSETS/layouts/MemberVariableLayout.ini" "$INST/MemberVariableLayout.ini"
copy_asset "$ASSETS/layouts/VTableLayout.ini" "$INST/VTableLayout.ini"
mkdir -p "$BIN/Mods/gspanel/scripts" "$BIN/gspanel-mod"
copy_asset "$ASSETS/mod/scripts/main.lua" "$BIN/Mods/gspanel/scripts/main.lua"
copy_asset "$ASSETS/mods.txt" "$BIN/Mods/mods.txt"

echo "== 3/4 生成 start.sh（LD_PRELOAD 只对游戏二进制生效，绝不能进 shell）"
START_TMP=$(mktemp "$INST/.start.XXXXXX")
cat > "$START_TMP" <<'EOF'
#!/bin/bash
# 由 gspanel / tools/palworld-ue4ss 安装的 UE4SS 启动脚本
# LD_PRELOAD 只对游戏二进制生效；如果让它进入 bash/PalServer.sh，会因 UE4SS 构造器段错误
cd "$(dirname "$0")"
if [ ! -f Pal/Binaries/Linux/steamclient.so ]; then cp linux64/steamclient.so Pal/Binaries/Linux/steamclient.so 2>/dev/null || true; fi
chmod +x Pal/Binaries/Linux/PalServer-Linux-Shipping 2>/dev/null || true
if [ -f Pal/Binaries/Linux/steamclient.so ]; then :; else cp linux64/steamclient.so Pal/Binaries/Linux/steamclient.so 2>/dev/null || true; fi
chmod +x Pal/Binaries/Linux/PalServer-Linux-Shipping 2>/dev/null || true
PRE="$PWD/Pal/Binaries/Linux/libUE4SS.so"
# EH 垫片必须排在最前（详见 tools/palworld-ue4ss/shim-eh/gxxfix.c）
[ -f "$PWD/Pal/Binaries/Linux/libgxxfix.so" ] && PRE="$PWD/Pal/Binaries/Linux/libgxxfix.so:$PRE"
exec env LD_PRELOAD="$PRE" Pal/Binaries/Linux/PalServer-Linux-Shipping Pal -useperfthreads -NoAsyncLoadingThread -UseMultithreadForDS
EOF
chmod 755 "$START_TMP"
chown games:games "$START_TMP"
mv -f "$START_TMP" "$INST/start.sh"

echo "== 4/4 属主改回 games"
chown -R games:games "$BIN/libUE4SS.so" "$BIN/MemberVariableLayout.ini" "$BIN/VTableLayout.ini" \
  "$BIN/UE4SS-settings.ini" "$BIN/Mods" "$BIN/gspanel-mod" "$INST/MemberVariableLayout.ini" \
  "$INST/VTableLayout.ini" "$INST/start.sh" 2>/dev/null || true

echo "完成。重启实例后检查日志里是否出现："
echo "  [UE4SS] FName::FName 解析器命中 / Linux: full mode post-init done"
echo "  [Lua] [gspanel] mod loaded"
echo "面板：实例控制台页会多出「扩展: 在线玩家 / 给物品 / 给经验」按钮。"
