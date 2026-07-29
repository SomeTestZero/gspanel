#!/usr/bin/env bash
# 把各实例最新备份收进仓库 saves/（每实例只留最新一份，作迁移种子），
# 提交推送留给人工确认（脚本不做 git 变更）。
set -euo pipefail
cd "$(dirname "$0")"

mkdir -p saves
found=0
for dir in /home/games/backups/*/; do
  name=$(basename "$dir")
  latest=$(ls -t "$dir"*.tar.gz 2>/dev/null | head -1 || true)
  [ -n "$latest" ] || continue
  cp "$latest" "saves/$name.tar.gz"
  echo ">> $name <= $(basename "$latest") ($(du -h "saves/$name.tar.gz" | cut -f1))"
  found=1
done
[ "$found" = 1 ] || { echo "没有找到任何备份（先在面板做一次备份）"; exit 1; }

echo ">> 确认后手动提交："
echo "   git add saves/ && git commit -m 'update saves' && git push"
