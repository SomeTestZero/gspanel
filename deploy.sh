#!/usr/bin/env bash
# GSPanel 一键部署：裸机自动走完整安装，已安装则只做构建+重启面板。
# 幂等，可重复执行；只会动 gspanel.service，绝不重启游戏实例 unit（gspanel-*）。
set -euo pipefail

UNIT=/etc/systemd/system/gspanel.service
cd "$(dirname "$0")"

[ "${EUID:-$(id -u)}" = 0 ] || { echo "需要 root 运行"; exit 1; }

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

# 3. 构建
echo ">> go build"
go build -o gspanel .

# 3.5 仓库自带的迁移存档（saves/<实例名>.tar.gz）放进备份目录，供面板「恢复」。
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

# 4. systemd unit：已存在则不动，只重启面板
if [ -f "$UNIT" ]; then
  echo ">> 重启 gspanel.service（游戏实例不受影响）"
  systemctl restart gspanel
else
  echo ">> 首次安装：写入 $UNIT 并开机自启"
  cat > "$UNIT" <<EOF
[Unit]
Description=GSPanel - lightweight game server management panel
After=network.target

[Service]
Type=simple
ExecStart=$(pwd)/gspanel
Restart=on-failure
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now gspanel
  first_install=1
fi

sleep 1
systemctl is-active --quiet gspanel && echo ">> gspanel 运行中（:8800）" || { echo "启动失败："; journalctl -u gspanel -n 10 --no-pager; exit 1; }
if [ "${first_install:-0}" = 1 ]; then
  echo ">> 首次启动密码：journalctl -u gspanel | grep 密码"
  echo ">> 新机别忘了：面板「设置/环境」安装 steamcmd 与 32 位依赖；防火墙放行 8800（建议仅内网）"
fi
