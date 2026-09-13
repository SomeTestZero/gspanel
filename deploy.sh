#!/usr/bin/env bash
# GSPanel 一键部署：裸机自动走完整安装，已安装则只做构建+重启面板。
# 幂等，可重复执行；只会动 gspanel.service，绝不重启游戏实例 unit（gspanel-*）。
#
# 面板以普通用户运行（默认取 sudo 调用者/项目目录属主，可用 PANEL_USER=xx 覆盖），
# 写 unit、启停实例、装 32 位依赖等特权动作经 /usr/local/sbin/gspanel-priv（sudoers 白名单）。
set -euo pipefail

cd "$(dirname "$0")"
PROJ_DIR=$(pwd)
UNIT=/etc/systemd/system/gspanel.service
HELPER=/usr/local/sbin/gspanel-priv
SUDOERS=/etc/sudoers.d/gspanel

[ "${EUID:-$(id -u)}" = 0 ] || { echo "需要 root 运行：sudo ./deploy.sh"; exit 1; }

# 面板运行用户：优先显式 PANEL_USER，其次 sudo 调用者，最后项目目录属主
PANEL_USER="${PANEL_USER:-${SUDO_USER:-$(stat -c %U "$PROJ_DIR")}}"
[ "$PANEL_USER" != root ] || { echo "无法确定普通用户：请以普通用户身份 clone 项目后 sudo ./deploy.sh，或 PANEL_USER=用户名 sudo ./deploy.sh"; exit 1; }
id "$PANEL_USER" >/dev/null 2>&1 || { echo "用户不存在: $PANEL_USER"; exit 1; }
echo ">> 面板运行用户: $PANEL_USER"

first_install=0
[ -f data/config.json ] || first_install=1

# 1. Go 1.22+
need_go=1
if command -v go >/dev/null; then
  minor=$(go version | sed 's/.*go1\.\([0-9]*\).*/\1/')
  [ "${minor:-0}" -ge 22 ] && need_go=0
fi
if [ "$need_go" = 1 ]; then
  echo ">> 安装 Go（需 1.22+）"
  apt-get update -qq && apt-get install -y golang-go
fi

# 2. games 用户（游戏运行身份，面板不会自动建）
if ! id games >/dev/null 2>&1; then
  echo ">> 创建 games 用户"
  useradd -m -s /bin/bash games
fi

# 3. 特权助手 + sudoers 白名单
echo ">> 安装特权助手 $HELPER"
install -o root -g root -m 0755 priv/gspanel-priv "$HELPER"
cat > "$SUDOERS" <<EOF
# GSPanel：面板运行用户的最小特权
$PANEL_USER ALL=(root) NOPASSWD: $HELPER
$PANEL_USER ALL=(games) NOPASSWD: ALL
EOF
chmod 0440 "$SUDOERS"
visudo -cf "$SUDOERS" >/dev/null

# 4. 构建（以面板运行用户身份，产物属主一致）
echo ">> go build"
sudo -u "$PANEL_USER" -H bash -c "cd '$PROJ_DIR' && $(command -v go) build -o gspanel ."

# 4.5 仓库自带的迁移存档（saves/<实例名>.tar.gz）放进备份目录，供面板「恢复」。
# 本机已有同名实例则跳过——那是存档来源机，别把种子回灌进它的备份列表。
for f in saves/*.tar.gz; do
  [ -e "$f" ] || continue
  name=$(basename "$f" .tar.gz)
  [ -d "/home/games/instances/$name" ] && continue
  mkdir -p "/home/games/backups/$name"
  cp "$f" "/home/games/backups/$name/"
  chown -R games:games "/home/games/backups/$name"
  echo ">> 存档已就位：$name（装好游戏后到实例「备份」页点恢复）"
done

# 5. 面板 unit：每次都写，保证 User/路径/能力与本次部署一致
echo ">> 写入 $UNIT 并重启面板（游戏实例不受影响）"
cat > "$UNIT" <<EOF
[Unit]
Description=GSPanel - lightweight game server management panel
After=network.target

[Service]
Type=simple
User=$PANEL_USER
Group=$PANEL_USER
WorkingDirectory=$PROJ_DIR
ExecStart=$PROJ_DIR/gspanel
Restart=on-failure
RestartSec=3
LimitNOFILE=65536
# 需要写/改 games 属主的游戏文件（写完 chown 回 games）；切 games 走 sudo，不用 SETUID。
# 注意：不要设 CapabilityBoundingSet——它会连带限制 setuid root 的 sudo（缺 CAP_SETUID 切不到 games）。
AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE

[Install]
WantedBy=multi-user.target
EOF

# 6. data/ 属主归面板运行用户
chown -R "$PANEL_USER:$PANEL_USER" "$PROJ_DIR/data" 2>/dev/null || true

systemctl daemon-reload
systemctl enable gspanel >/dev/null 2>&1 || true
systemctl restart gspanel

sleep 1
systemctl is-active --quiet gspanel && echo ">> gspanel 运行中（:8800，用户 $PANEL_USER）" || { echo "启动失败："; journalctl -u gspanel -n 20 --no-pager; exit 1; }
if [ "$first_install" = 1 ]; then
  echo ">> 首次启动密码：journalctl -u gspanel | grep 密码"
  echo ">> 新机别忘了：面板「设置/环境」安装 steamcmd 与 32 位依赖；防火墙放行 8800（建议仅内网）"
fi
