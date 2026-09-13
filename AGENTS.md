# AGENTS.md · 项目记忆库（GSPanel）

> **维护规则（最高优先级）：每次改动本项目后，必须同步更新本文件中被改动到的描述，
> 保持它与代码实际行为一致，然后再算完成。** 本文件是给后续会话快速恢复上下文用的，
> 写给 AI 看：求准、求密、不求全（细节去翻 README.md 和源码）。

## 一句话概述

轻量游戏服务器管理面板：Go 单二进制（`//go:embed static templates` 内嵌前端与模板），
游戏进程由 systemd 托管（每个实例一个 unit `gspanel-<实例名>`，面板重启/崩溃不影响游戏），
新游戏通过 `templates/*.json` 模板扩展，零 Go 代码改动。

## 运行形态

- 面板服务：`gspanel.service`（开机自启），**以普通用户运行**（systemd `User=`，本机 `ubuntu`）：
  - unit 里 `AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE`：写 `/home/games/**`（games 属主文件）后能 chown 回 games；
    **别设 `CapabilityBoundingSet`**（见已知坑）
  - 需要 root 的动作经助手 `/usr/local/sbin/gspanel-priv`（sudoers `/etc/sudoers.d/gspanel` 白名单）：
    写/删 `gspanel-*.service`+daemon-reload、start/stop/enable/disable、apt 装 32 位依赖
  - 切 `games` 身份走 `sudo -u games env HOME=/home/games ...`（sudo 会清能力位；仅 root 旧模式用 Credential）
  - 二进制 `/home/ubuntu/gspanel/gspanel`，源码同目录（属主 `ubuntu`）
- 监听：默认 `:8800`（`State.BindAddr()`，存 `data/config.json` 的 `port`）
- 路径（main.go）：`BaseDir` 运行时取二进制所在目录（clone 位置随意，`data/`、`templates/`
  用户模板都跟随它）；游戏侧固定 `InstancesDir=/home/games/instances`、
  `BackupsDir=/home/games/backups`，游戏进程以 `games` 用户运行
- 状态：实例/账号/计划任务在 `data/config.json`（密码哈希、实例、计划任务、会话），
  事件日志（时间线）在 `data/events.jsonl`
- 控制台日志：`/home/games/instances/<名>/logs/console.log`；面板日志 `journalctl -u gspanel -f`
- 异地备份：设置页「备份异地同步」配 `sync_targets`（SSH 目标列表，存 config.json，空=关闭；
  旧单目标字段 `sync_target` 加载时自动迁移）后，每次备份（手动/定时）成功自动 rsync 到
  `目标:~/gspanel-saves/<实例>-latest.tar.gz`（远端只留最新一份，固定文件名覆盖）；
  任一目标同步失败则备份任务判失败、进事件日志（单个失败不影响其他目标）。
  依赖面板机 root 到目标主机的免密 SSH（当前配的是 yecao2 + yecao 两台）

## 构建 / 部署 / 验证

```bash
sudo ./deploy.sh                                   # 幂等一键部署（需 root）：裸机全装（Go/games 用户/unit/特权助手/自启），已装则只构建+重启面板；
                                                   # 面板运行用户默认取 sudo 调用者，PANEL_USER=用户名 可覆盖；unit 每次重写
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
| `api.go` | 全部 HTTP 路由/handler（认证、实例 CRUD、启停、控制台、配置读写、备份、计划任务、事件日志 `GET /api/events`）；各写操作成功后顺手记事件。`POST /api/instances/{name}/mod-command` 是 Palworld 扩展命令（走 UE4SS mod 文件队列，返回 `{ok,message}`） |
| auth.go | 会话 + 登录限流（10 分钟错 5 次锁 10 分钟） |
| config.go | State 结构、config.json 读写、BindAddr |
| events.go | 事件日志（时间线）：`EventLog` 持久化到 `data/events.jsonl`（每行一条 JSON，内存留 500 条，超 256KB 重写截断）；任务开始/结束经 `TaskManager.OnStart/OnFinish` 回调自动入日志；看门狗 `watchInstances`（scheduler tick 驱动，靠 `ActiveEnterTimestampMonotonic` 识别进程重启）检测**非面板发起**的退出：崩溃被 systemd 拉起→`crash`、自行退出→`exit`、failed→`failed`（面板操作以 `HasRunningFor`+2 分钟内事件排除误报）；前端「事件日志」页（侧栏，10 秒轮询，可按实例过滤） |
| templates.go | 模板结构体、`LoadTemplates`、`validateTemplate`、URL 导入（导入即落盘 templates/） |
| instance.go | 实例生命周期：创建（拷 DefaultArgs）、安装、种子配置、删除 |
| systemctl.go | 生成 start.sh（`cd 实例目录 && exec 启动命令`）与 systemd unit；写 unit/启停实例 privileged 动作：root 直接执行，普通用户经 `systemctlPriv` → `sudo /usr/local/sbin/gspanel-priv`，只读 `show` 不走特权 |
| priv/gspanel-priv | 特权助手（deploy.sh 安装到 `/usr/local/sbin`）：unit-write/unit-remove/ctl/install-deps，unit 名与动作严格校验，sudoers 只授权面板用户执行它 |
| steamcmd.go | steamcmd 安装/更新游戏、32 位依赖检测与安装（apt） |
| rcon.go / rest.go | Source RCON 协议；游戏 REST 管理 API（Palworld RCON 丢响应的替代通道） |
| configfile.go | 游戏配置读写，三种 format：`option-settings`（Palworld 专用就地替换）/ `kv` / `raw` |
| tasks.go / stream.go | 后台任务（安装/更新/备份）+ SSE 日志订阅；`OnStart/OnFinish` 回调与 `Task.Err` 供事件日志记录任务始末 |
| scheduler.go | 计划任务（每日/间隔：重启、备份、更新）；`updateInstance`（手动/定时/自动更新共用入口）先比对 Steam buildid 预检，已最新则直接返回不停服（预检失败照旧更新）；`gracefulStop`：RCON 广播→存档→停；tick 里挂版本轮询入口与看门狗 `watchInstances` |
| updatecheck.go | 版本轮询自动更新：实例开 `auto_update`（设置页开关，存 config.json）后，每 30 分钟用 api.steamcmd.net 查 public 分支 buildid 对比本地 `steamapps/appmanifest_<appid>.acf`，落后且实例无任务在跑（`HasRunningFor`）时更新。玩家门槛 `autoUpdateReady`：服务没开或模板无 `format=players` REST 命令→直接更；有玩家→广播通知（REST Broadcast 优先，每小时最多一次）并等待；无玩家持续 10 分钟（内存态 `autoStates`，面板重启重计）→才起 `auto-update` 任务走 `updateInstance` 流程（停→更→回写配置→拉起）。广播通知与首次无玩家两个等待节点会写事件日志 |
| backup.go / monitor.go / netinfo.go / util.go | 备份打包/恢复（恢复后按面板记录重写 ini 端口/密码/服务器名）/上传（跨服迁移存档：新机建同名模板实例→上传备份包或 deploy 放好 saves/→恢复）；`backupAndSync`（手动/定时备份共用入口）备份成功后按 `sync_targets` 列表逐目标 rsync 异地同步（`syncBackup`，远端只留最新一份）；/proc 资源监控；公网 IP 探测；chown 等杂项 |
| tools/palworld-ue4ss/ | Palworld 给物品的框架侧物料：ue4ss-linux 源码补丁（8 个修复）、build/install/uninstall/硬链接副本测试脚本；运行时资产（mod/布局表/设置）在 `assets/palworld-ue4ss/`（embed 进面板二进制） |
| mods.go | Palworld 扩展命令（UE4SS）面板侧管理：二进制上传/导入/下载（`data/ue4ss/`）、实例安装/卸载/状态（`assets/palworld-ue4ss/` 下发 + start.sh 注入 + `Instance.UE4SS`） |

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
- 写文件前先想：面板进程是普通用户 `ubuntu`（靠 `CAP_DAC_OVERRIDE` 才能写 games 属主文件，靠 `CAP_CHOWN` 才能 chown 回 games），游戏进程是 games，写错属主游戏写不动
- **面板 unit 不能设 `CapabilityBoundingSet`**：setuid root 的 `sudo` 也受它约束，缺 CAP_SETUID 会导致 `sudo -u games` 失败（表现为「环境」页依赖检测全 false）；限权只用 `AmbientCapabilities`
- **Palworld 给物品（原生 Linux）已打通**：官方没有 give 命令（v1.0.4 实测 RCON 一律 `Unknown command`；
  REST 只有 info/players/metrics/announce/kick/ban/unban/save/stop/shutdown/settings/game-data），
  我们用**自行修复并重编译的 ue4ss-linux**（LD_PRELOAD + Lua mod）实现；上游预编译版在 1.0.4 上必修的
  8 个 bug（GUObjectArray 校验/上限、GMalloc vtable 锚点、FMalloc vtable 基类槽数、FName 解析器与 ABI、
  `bit_cast_mfp` 零初始化、chunk 遍历越界）全部在 `tools/palworld-ue4ss/patches/` + README 里。
  面板：**面板原生管理**——「设置/环境 → Palworld 扩展命令」上传/按路径导入/URL 下载 libUE4SS.so
  （存 `data/ue4ss/libUE4SS.so`，不入 git），实例「设置 → 扩展命令」一键安装/卸载/更新
  （`mods.go` + `Instance.UE4SS` 标志）；
  `writeStartScript` 在 `UE4SS=true` 时生成 `exec env LD_PRELOAD=... 游戏二进制`（LD_PRELOAD 绝不能进 shell），
  所以面板重写 start.sh 不再丢注入。
  控制台页有「扩展: 在线玩家/给物品/给经验」按钮（`api.go` `mod-command` + 文件队列
  `<实例>/Pal/Binaries/Linux/gspanel-mod/cmd.txt|res.txt`，因为 ProcessConsoleExec hook 在本游戏装不上）。
  内置资产（mod/布局表/设置）在 `assets/palworld-ue4ss/`（embed 进二进制，安装时下发）；
  框架补丁/构建脚本/硬链接副本测试在 `tools/palworld-ue4ss/`（游戏更新后重新编译 .so 并在面板重新「安装」即可）；
  **mod 的 Lua 语法/加载期错误会抛 C++ 异常直接 abort 游戏进程**（libsteam_api 的
  `__gxx_personality_v0` 冲突），改完 mod 必须先 `luac5.4 -p` 校验（已踩坑：Lua 5.4 无全局 unpack、
  嵌套函数不能用 `...`）。截至 2026-09-13 已实测：UE4SS 完整初始化（FindAllOf/StaticFindObject/
  ForEachUObject 154k 对象）、mod 加载、文件队列 who/give 错误路径；**对在线玩家的实际给物品
  尚未实测**（当时无人上线），玩家上线后可用面板「给物品…」验证
- `data/config.json` 曾被提交进 git（含面板密码哈希/实例 RCON 密码）：已用 filter-repo 重写
  全部历史并 force push（`081edfd` 起历史中无此文件），`.gitignore` 已排除 `data/` 和二进制；
  仓库必须 private（`saves/` 迁移存档的 ini 里含游戏管理员/RCON 密码）

## 会话恢复 checklist

1. 读本文件 → 2. 看 `git log --oneline -5` 和 `git status` → 3. 改完更新本文件。
