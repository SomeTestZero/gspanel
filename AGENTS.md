# AGENTS.md · 项目记忆库（GSPanel）

> **维护规则（最高优先级）：每次改动本项目后，必须同步更新本文件中被改动到的描述，
> 保持它与代码实际行为一致，然后再算完成。** 本文件是给后续会话快速恢复上下文用的，
> 写给 AI 看：求准、求密、不求全（细节去翻 README.md 和源码）。

## 一句话概述

轻量游戏服务器管理面板：Go 单二进制（`//go:embed static templates` 内嵌前端与模板），
游戏进程由 systemd 托管（每个实例一个 unit `gspanel-<实例名>`，面板重启/崩溃不影响游戏），
新游戏通过 `templates/*.json` 模板扩展，零 Go 代码改动。

## 运行形态

- 面板服务：`gspanel.service`（开机自启），二进制 `/root/gspanel/gspanel`，源码同目录
- 监听：默认 `:8800`（`State.BindAddr()`，存 `data/config.json` 的 `port`）
- 路径（main.go）：`BaseDir` 运行时取二进制所在目录（clone 位置随意，`data/`、`templates/`
  用户模板都跟随它）；游戏侧固定 `InstancesDir=/home/games/instances`、
  `BackupsDir=/home/games/backups`，游戏进程以 `games` 用户运行
- 状态：全部在 `data/config.json`（密码哈希、实例、计划任务、会话）
- 控制台日志：`/home/games/instances/<名>/logs/console.log`；面板日志 `journalctl -u gspanel -f`

## 构建 / 部署 / 验证

```bash
./deploy.sh                                        # 幂等一键部署：裸机全装（Go/games 用户/unit/自启），已装则只构建+重启面板；
                                                   # saves/<实例>.tar.gz 会拷进备份目录供恢复（本机已有同名实例则跳过）
./push-saves.sh                                    # 收各实例最新备份到 saves/ 作迁移种子；git 提交推送留人工
journalctl -u gspanel -n 10 --no-pager               # 启动日志里有 "loaded N game templates"
go vet ./...                                         # 无测试框架；临时写过 *_test.go 验证后删除
```

模板热加载：`templates/*.json` 同时是 embed 源和运行时用户模板目录
（`LoadTemplates(embedded, BaseDir+"/templates")`，同 ID 用户覆盖内置），
所以只加模板时重启服务即生效，但重新 build 才会嵌进二进制。

## 文件职责速查

| 文件 | 职责 |
|---|---|
| main.go | 启动、常量、embed、Server 结构体 |
| api.go | 全部 HTTP 路由/handler（认证、实例 CRUD、启停、控制台、配置读写、备份、计划任务） |
| auth.go | 会话 + 登录限流（10 分钟错 5 次锁 10 分钟） |
| config.go | State 结构、config.json 读写、BindAddr |
| templates.go | 模板结构体、`LoadTemplates`、`validateTemplate`、URL 导入（导入即落盘 templates/） |
| instance.go | 实例生命周期：创建（拷 DefaultArgs）、安装、种子配置、删除 |
| systemctl.go | 生成 start.sh（`cd 实例目录 && exec 启动命令`）与 systemd unit |
| steamcmd.go | steamcmd 安装/更新游戏、32 位依赖检测与安装（apt） |
| rcon.go / rest.go | Source RCON 协议；游戏 REST 管理 API（Palworld RCON 丢响应的替代通道） |
| configfile.go | 游戏配置读写，三种 format：`option-settings`（Palworld 专用就地替换）/ `kv` / `raw` |
| tasks.go / stream.go | 后台任务（安装/更新/备份）+ SSE 日志订阅 |
| scheduler.go | 计划任务（每日/间隔：重启、备份、更新）；`gracefulStop`：RCON 广播→存档→停 |
| backup.go / monitor.go / netinfo.go / util.go | 备份打包/恢复（恢复后按面板记录重写 ini 端口/密码/服务器名）/上传（跨服迁移存档：新机建同名模板实例→上传备份包或 deploy 放好 saves/→恢复）；/proc 资源监控；公网 IP 探测；chown 等杂项 |

## 模板系统（改动重灾区，坑都在这）

- 必填：`id`（小写/数字/中划线 ≤40）、`name`、`steam_app_id`、`executable`；
  `ports` 每项 key 唯一、proto 仅 udp/tcp；`stop_mode` 仅 `rcon`/`sigterm`
- `executable` 相对实例目录，start.sh 会先 cd 进去再 exec；`default_args` 创建实例时拷贝，
  **无变量替换**，要写实例路径就用相对路径（如 dst 模板的 `-persistent_storage_root .`）
- 配置 format：
  - `option-settings`：Palworld 单行 `OptionSettings=(...)`，就地替换保留引号风格
  - `kv`：扁平 `key=value` 行；**section 头会被忽略**；**写入时文件里不存在的键会追加到
    文件末尾**——带 section 的 ini（如 DST）schema 里只能放游戏默认生成文件中已有的键
  - `raw`：整文替换；raw 写入会创建文件但**不会创建父目录**
- **`type:"bool"` 下拉固定提交大写 `"True"/"False"`**（static/app.js:505，为 Palworld 设计）。
  需要小写 `true/false` 的游戏（DST 等）必须改用 `select` + `options:["true","false"]`
- `seed_from`：从 steamcmd 下载的游戏文件里复制种子配置（游戏不自带默认配置的游戏用不了）
- `backup_paths` 相对实例目录；`world_paths` 是「创建新世界」时删除的路径
- `rest_api.commands`：把控制台命令路由到游戏 REST API；`console_buttons` 缺省用内置默认按钮
- 无 RCON 的游戏控制台只能看日志，`has_rcon=false`

## 现有模板（14 个）

palworld / valheim / cs2 / 7dtd / rust / satisfactory / zomboid / enshrouded / gmod / tf2 /
ark-se / terraria / corekeeper / dst（饥荒联机版，343050，2026-07 新增：配置收进实例目录
`dst/`，布尔用 select，需 Klei cluster_token 才能上线，详见 templates/dst.json notes）

## 已知坑（README「已知问题」有完整版）

- Palworld：经验/掉落倍率等创世界时固化进存档；RCON 丢响应是游戏 bug，部分命令已走 REST
- steamcmd 报 "Missing configuration"：删 `~/steamcmd/appcache` 与 `~/Steam/appcache`
- `systemctl show --value` 多属性顺序不保证，解析要用 key=value
- 改 `/home/games/instances/**` 任何文件后属主必须保持 `games:games`（用 `chownToGames`）
- 写文件前先想：面板进程是 root，游戏进程是 games，写错属主游戏写不动
- `data/config.json` 曾被提交进 git（含面板密码哈希/实例 RCON 密码）：已用 filter-repo 重写
  全部历史并 force push（`081edfd` 起历史中无此文件），`.gitignore` 已排除 `data/` 和二进制；
  仓库必须 private（`saves/` 迁移存档的 ini 里含游戏管理员/RCON 密码）

## 会话恢复 checklist

1. 读本文件 → 2. 看 `git log --oneline -5` 和 `git status` → 3. 改完更新本文件。
