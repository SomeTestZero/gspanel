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
  - unit 里 `AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER`：写 `/home/games/**`（games 属主文件）后能 chown 回 games，
    并对 games 属主文件 chmod（安装扩展命令复制 libUE4SS.so/start.sh 需要）；
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
  依赖**面板运行用户**的免密 SSH（`syncBackup` 直接 ssh，不经 sudo；目标名要能解析且有 Host 配置）

## 构建 / 部署 / 验证

```bash
sudo ./deploy.sh                                   # 幂等一键部署（需 root）：裸机全装（Go/games 用户/unit/特权助手/自启），已装则只构建+重启面板；
                                                   # 面板运行用户默认取 sudo 调用者，PANEL_USER=用户名 可覆盖；unit 每次重写
                                                   # saves/<实例>.tar.gz 会拷进备份目录供恢复（本机已有同名实例则跳过）
./push-saves.sh                                    # 收各实例最新备份到 saves/ 作迁移种子；git 提交推送留人工
journalctl -u gspanel -n 10 --no-pager               # 启动日志里有 "loaded N game templates"
PATH=/usr/local/go/bin:$PATH go test ./...            # mod 命令参数/响应关联/版本一致性回归
PATH=/usr/local/go/bin:$PATH go vet ./...
lua5.4 tools/palworld-ue4ss/test-mod.lua               # 纯 Lua 模拟回归（玩家身份、单次发放、到账检查、队列）
```

模板热加载：`templates/*.json` 同时是 embed 源和运行时用户模板目录
（`LoadTemplates(embedded, BaseDir+"/templates")`，同 ID 用户覆盖内置），
所以只加模板时重启服务即生效，但重新 build 才会嵌进二进制。

## 文件职责速查

| 文件 | 职责 |
|---|---|
| main.go | 启动、常量、embed、Server 结构体 |
| `api.go` | 全部 HTTP 路由/handler（认证、实例 CRUD、启停、控制台、配置读写、备份、计划任务、事件日志 `GET /api/events`）；各写操作成功后顺手记事件。`POST /api/instances/{name}/mod-command` 是 Palworld 扩展命令（`modqueue.go` 的请求 ID 文件队列，参数/数量/物品库校验，返回 `{ok,message}`；写操作记事件）；`POST /api/instances/{name}/give` 是「给物品」统一入口（参数/数量/品质/物品库校验，帕鲁走 mod 队列、七日杀走 telnet `give`，成败都记事件） |
| auth.go | 会话 + 登录限流（10 分钟错 5 次锁 10 分钟） |
| config.go | State 结构、config.json 读写、BindAddr |
| events.go | 事件日志（时间线）：`EventLog` 持久化到 `data/events.jsonl`（每行一条 JSON，内存留 500 条，超 256KB 重写截断）；任务开始/结束经 `TaskManager.OnStart/OnFinish` 回调自动入日志；看门狗 `watchInstances`（scheduler tick 驱动，靠 `ActiveEnterTimestampMonotonic` 识别进程重启）检测**非面板发起**的退出：崩溃被 systemd 拉起→`crash`、自行退出→`exit`、failed→`failed`（面板操作以 `HasRunningFor`+2 分钟内事件排除误报）；前端「事件日志」页（侧栏，10 秒轮询，可按实例过滤） |
| templates.go | 模板结构体、`LoadTemplates`、`validateTemplate`、URL 导入（导入即落盘 templates/） |
| instance.go | 实例生命周期：创建（拷 DefaultArgs）、安装、种子配置、删除 |
| systemctl.go | 生成 start.sh（`cd 实例目录 && exec 启动命令`）与 systemd unit（含 `Environment=HOME=/home/games`，见已知坑）；写 unit/启停实例 privileged 动作：root 直接执行，普通用户经 `systemctlPriv` → `sudo /usr/local/sbin/gspanel-priv`，只读 `show` 不走特权 |
| priv/gspanel-priv | 特权助手（deploy.sh 安装到 `/usr/local/sbin`）：unit-write/unit-remove/ctl/install-deps，unit 名与动作严格校验，sudoers 只授权面板用户执行它 |
| pathspec.go | 模板路径项（`backup_paths`/`world_paths`）解析：相对实例目录 / `~/`（games 家目录）/ `{config:配置键}` 占位符（world_paths 定位当前世界存档用）/ `!模式`（仅 backup_paths，tar --exclude）；`validatePathSpec` 含占位符展开值校验，防配置值把路径带飞 |
| steamcmd.go | steamcmd 安装/更新游戏、32 位依赖检测与安装（apt）；`ensureSteamSDK64`（postInstall 调）自动补 `~/.steam/sdk64/steamclient.so` 软链 |
| rcon.go / rest.go | Source RCON 协议；游戏 REST 管理 API（Palworld RCON 丢响应的替代通道） |
| telnet.go | telnet 控制台（七日杀等）：`telnetExec` 静默间隔收响应（600ms 无数据=结束）、`telnetExecConfig` 从配置文件读 TelnetEnabled/TelnetPort/TelnetPassword；收尾 `telnetExit` 先发 `exit` 再关 socket（认证前不发，见已知坑），避免服务端往死连接写数据报错刷屏；`ensureTelnetEnabled` 安装收尾自动开启 |
| modmgr.go | 文件夹式 Mod 管理（`mod_manager.dir`，如 Mods）：ModInfo.xml 解析、zip/tar.gz/**7z/rar** 解压安装（zip/tar 走 Go 原生防 zip-slip/解压炸弹；7z/rar 走外部 `7z`/`7za`（需 p7zip-full），`check7zEntries` 预检 `7z l -slt` 目录拒绝对路径/`..`/符号链接/加密条目，解后还有符号链接复查）；整合包里嵌套的子压缩包再解一层（`expandNestedArchives`，只解一层）+「未识别内容」报告（不再静默丢弃，响应 `skipped`/事件日志/任务日志）；启用/禁用（Mods ↔ Mods.disabled）、上传/URL 直链安装、删除 |
| nexuscache.go | Nexus 本地 Mod 目录缓存（data/nexus-mods.json）：`syncNexusCatalog` 全量同步任务（80/批，正反两遍翻页去重，9028 个约 2.5 分钟），读取时补译文/版本提示 |
| nexus.go | NexusMods：浏览/搜索走 GraphQL v2（`mods(filter/sort/offset/count)`），端侧与版本提示启发式（modSideHint/gameVersionHint）；文件列表/下载直链/账号校验走 v1 REST（download_link 需 Premium；免费账号用 nxm 里的 key/expires）；⚠️ v1 没有 search 接口 |
| modlang.go | Mod 中文翻译：翻译链 缓存 data/modlang.json → 内置词典（游戏自带 TFP 组件等）→ OpenAI 兼容接口（设置页可配）→ MyMemory 免费接口（Google 被墙不可用）；译文按源文本缓存复用 |
| configfile.go | 游戏配置读写，四种 format：`option-settings`（Palworld 专用就地替换）/ `kv` / `xmlkv`（XML 属性式，七日杀 serverconfig.xml，就地替换保留注释）/ `raw` |
| tasks.go / stream.go | 后台任务（安装/更新/备份）+ SSE 日志订阅；`OnStart/OnFinish` 回调与 `Task.Err` 供事件日志记录任务始末 |
| scheduler.go | 计划任务（每日/间隔：重启、备份、更新）；`updateInstance`（手动/定时/自动更新共用入口）先比对 Steam buildid 预检，已最新则直接返回不停服（预检失败照旧更新）；`gracefulStop`：RCON 广播→存档→停；`newWorld`（创建新世界：停→备份→删 world_paths 解析出的存档→启动，候选路径不存在则跳过并在日志提示）；tick 里挂版本轮询入口与看门狗 `watchInstances` |
| updatecheck.go | 版本轮询自动更新：实例开 `auto_update`（设置页开关，存 config.json）后，每 30 分钟用 api.steamcmd.net 查 public 分支 buildid 对比本地 `steamapps/appmanifest_<appid>.acf`，落后且实例无任务在跑（`HasRunningFor`）时更新。玩家门槛 `autoUpdateReady`：服务没开或模板无 `format=players` REST 命令→直接更；有玩家→广播通知（REST Broadcast 优先，每小时最多一次）并等待；无玩家持续 10 分钟（内存态 `autoStates`，面板重启重计）→才起 `auto-update` 任务走 `updateInstance` 流程（停→更→回写配置→拉起）。广播通知与首次无玩家两个等待节点会写事件日志 |
| backup.go / monitor.go / netinfo.go / util.go | 备份打包/恢复（恢复后按面板记录重写 ini 端口/密码/服务器名）/上传（跨服迁移存档：新机建同名模板实例→上传备份包或 deploy 放好 saves/→恢复）；备份/恢复 tar 带 `-P`：实例外路径以绝对名进归档（旧全相对名备份兼容），恢复前 `checkBackupMembers` 拒 `..` 逃逸/越界绝对名，恢复后实例外文件 chown 回 games；`backupAndSync`（手动/定时备份共用入口）备份成功后按 `sync_targets` 列表逐目标 rsync 异地同步（`syncBackup`，远端只留最新一份）；/proc 资源监控；公网 IP 探测；chown 等杂项 |
| tools/palworld-ue4ss/ | Palworld 给物品的框架侧物料：ue4ss-linux 源码补丁（12 项修复，含 EH 运行时修复）、build/install/uninstall/硬链接副本测试脚本、`shim-eh/`（__gxx_personality_v0 拦截垫片，产物 libgxxfix.so 提交进 assets）；`extract-items-zh.py` 从 pak 离线提取物品中文名基线；运行时资产（垫片/mod/布局表/设置）在 `assets/palworld-ue4ss/`（embed 进面板二进制） |
| mods.go | Palworld 扩展命令（UE4SS）面板侧管理：二进制上传/导入/下载（`data/ue4ss/`）、实例安装/卸载/状态（`assets/palworld-ue4ss/` 下发，含 libgxxfix.so 垫片 + start.sh 注入 + `Instance.UE4SS`）、mod 磁盘/在役版本比对（mod-alive 的 PID 必须匹配 systemd MainPID）；资产用新 inode 原子替换，不能截断运行中 mmap 的 .so |
| palworlditems.go | 帕鲁物品库（「给物品」用）：内置基线 `assets/palworld-items/palworld-zh.json`（2466 项 ID+中文名，pak 离线提取）+ 运行时缓存 `data/items/palworld.json`；`GET /instances/{name}/items`（搜索/分类）、`POST .../items/refresh`（mod 导 ID + 按 ID 合并中文名）、`GET .../players`（whojson 在线玩家）等 handler 按模板分发（七日杀分支在 `items7dtd.go`） |
| items7dtd.go | 七日杀「给物品」数据/通道：物品库直接解析游戏文件（`Data/Config/items.xml` + `Localization.csv` + `Mods/*/Config/*`，中文名取 schinese 列，僵尸拳头/任务占位等内部物品过滤，1 分钟缓存+文件指纹失效）；telnet `lp` 在线玩家解析（玩家名右边界=下一个 `key=`，给物品用**实体 ID**防名字带空格）；`dtdGive`+`dtdGiveOK`（telnet 无成功标记，按错误措辞判定）；`giveKindOf` 判通道（palworld=mod / 7dtd=telnet） |
| sandbox.go / tools/7dtd-sandbox/ | 七日杀沙盒选项（SandboxCode）编辑器：编解码（`'A'`+3 字符块：2 字符 base-26 选项 ID + 1 字符值索引，默认项省略，ID=SandboxOptions 枚举声明序）+ 选项表 `assets/7dtd-sandbox/options.json`（165 项含中文名/说明/值标签/默认值，`extract.py` 从游戏 Assembly-CSharp.dll + Localization.csv 提取，游戏更新后重跑）；API `GET /api/sandbox/tables/{id}`、`POST /api/sandbox/decode|encode`、`GET .../sandbox/live`（gso）；前端「沙盒选项编辑器」（openSandboxEditor） |

## 模板系统（改动重灾区，坑都在这）

- 必填：`id`（小写/数字/中划线 ≤40）、`name`、`steam_app_id`、`executable`；
  `ports` 每项 key 唯一、proto 仅 udp/tcp；`stop_mode` 仅 `rcon`/`sigterm`
- `executable` 相对实例目录，start.sh 会先 cd 进去再 exec；`default_args` 创建实例时拷贝，
  **无变量替换**，要写实例路径就用相对路径（如 dst 模板的 `-persistent_storage_root .`）
- 配置 format：
  - `option-settings`：Palworld 单行 `OptionSettings=(...)`，就地替换保留引号风格
  - `kv`：扁平 `key=value` 行；**section 头会被忽略**；**写入时文件里不存在的键会追加到
    文件末尾**——带 section 的 ini（如 DST）schema 里只能放游戏默认生成文件中已有的键
  - `xmlkv`：XML 属性式 `<property name="K" value="V"/>`（七日杀），就地替换保留注释/排版，
    缺失键插到根闭合标签前；值做实体转义
  - `raw`：整文替换；raw 写入会创建文件但**不会创建父目录**
- **`type:"bool"` 下拉固定提交大写 `"True"/"False"`**（static/app.js:505，为 Palworld 设计）。
  需要小写 `true/false` 的游戏（DST 等）必须改用 `select` + `options:["true","false"]`
- `seed_from`：从 steamcmd 下载的游戏文件里复制种子配置（游戏不自带默认配置的游戏用不了）
- `backup_paths`/`world_paths` 路径语法（pathspec.go）：相对实例目录、`~/...`（games 家目录，游戏把数据写 HOME 的用它）
  与 `{config:配置键}` 占位符（按实例配置文件当前值展开，如 `Saves/{config:GameWorld}/{config:GameName}`）；
  `!模式`（仅 backup_paths）= tar --exclude 排除模式（按归档内成员名匹配，* 含 /），挡 mod 客户端资产等大块内容；
  `world_paths` 是「创建新世界」时删除的路径，可列多个候选（不存在的跳过，全部不存在时日志提示）
- 配置字段通用能力：`presets`（预设一键填充按钮，label/value/note）、`decode_command`
  （「解码当前值」按钮：在服务器上执行该命令展示输出）与 `sandbox_options`（「沙盒选项编辑器」，
  值为选项表 ID，七日杀 SandboxCode 专用）。七日杀 SandboxCode 已用：17 个官方预设
  （难度 6 + 整活 11）一键填充 + `gso` 在线解码 + 165 项可视化编辑
- `rest_api.commands`：把控制台命令路由到游戏 REST API；`console_buttons` 缺省用内置默认按钮
- `rcon.type`：`source`（RCON，需 port_key）/ `telnet`（七日杀式行式控制台，`config_path` 指向含
  TelnetEnabled/TelnetPort/TelnetPassword 的配置文件；stop_mode=rcon 时优雅停服走 say→saveworld→shutdown）
- `mod_manager.dir`：文件夹式 Mod 管理（前端出「Mod 管理」页）；禁用目录为 `<dir>.disabled`
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
- **game unit 必须带 `Environment=HOME=/home/games`**（`renderUnit` 已内置）：`games` 用户 passwd
  家目录是 `/usr/games`（root 属主写不进），systemd 默认把 HOME 设成它；Unity/Mono 游戏启动会写
  `~/.local/share/...`（七日杀 EOS 初始化建 `~/.local/share/7DaysToDie`），Permission denied 后走
  关闭流程 SEGV，systemd 5 秒一拉形成崩溃循环，每次还砸 10MB `mono_crash.mem.*.blob`（可删）
- **带 steamclient.so 的游戏（七日杀等）还需 `~/.steam/sdk64/steamclient.so` 软链**指向实例里的
  steamclient.so，否则 `Could not initialize GameServer`、Steam 玩家连不进；`ensureSteamSDK64`
  已在安装/更新收尾自动创建（机器级，指向任意实例皆可）
- **面板 unit 不能设 `CapabilityBoundingSet`**：setuid root 的 `sudo` 也受它约束，缺 CAP_SETUID 会导致 `sudo -u games` 失败（表现为「环境」页依赖检测全 false）；限权只用 `AmbientCapabilities`
- **Palworld 给物品（原生 Linux）调用链已修复，实际到账验证见「当前进展与状态」**：官方没有 give 命令（v1.0.4 实测 RCON 一律 `Unknown command`；
  REST 只有 info/players/metrics/announce/kick/ban/unban/save/stop/shutdown/settings/game-data），
  我们用**自行修复并重编译的 ue4ss-linux**（LD_PRELOAD + Lua mod）实现；上游预编译版在 1.0.4 上必修的
  8 个 bug（GUObjectArray 校验/上限、GMalloc vtable 锚点、FMalloc vtable 基类槽数、FName 解析器与 ABI、
  `bit_cast_mfp` 零初始化、chunk 遍历越界）全部在 `tools/palworld-ue4ss/patches/` + README 里。
  面板：**面板原生管理**——「设置/环境 → Palworld 扩展命令」上传/按路径导入/URL 下载 libUE4SS.so
  （存 `data/ue4ss/libUE4SS.so`，不入 git），实例「设置 → 扩展命令」一键安装/卸载/更新
  （`mods.go` + `Instance.UE4SS` 标志）；
  `writeStartScript` 在 `UE4SS=true` 时生成 `exec env LD_PRELOAD=libgxxfix.so:libUE4SS.so 游戏二进制`
  （LD_PRELOAD 绝不能进 shell；垫片排最前），所以面板重写 start.sh 不再丢注入。
  控制台页有「扩展: 在线玩家/给物品/给经验」按钮（`api.go` `mod-command` + 文件队列
  `<实例>/Pal/Binaries/Linux/gspanel-mod/cmd.txt|res.txt`，因为 ProcessConsoleExec hook 在本游戏装不上）。
  **「给物品」是物品选择对话框**（static/app.js `openGiveItemDialog`）：在线玩家下拉（mod `whojson`）+
  全量物品搜索（游戏内中文名/内部 ID，2466 项，前端本地过滤）+ 数量快捷档 + 最近使用 +
  「从游戏刷新物品库」。物品库：内置基线 `assets/palworld-items/palworld-zh.json`
  （`tools/palworld-ue4ss/extract-items-zh.py` 从 pak 的 L10N/zh-Hans/DT_ItemNameText_Common 离线提取，
  回退链 zh-Hans→zh-Hant→en→base→ID），运行时从 mod 拿 ID 后按 ID 合并名字存 `data/items/palworld.json`。
  内置资产（EH 垫片/mod/布局表/设置）在 `assets/palworld-ue4ss/`（embed 进二进制，安装时下发）；
  框架补丁/构建脚本/硬链接副本测试在 `tools/palworld-ue4ss/`（游戏更新后重新编译 .so 并在面板重新「安装」即可）；
  **mod 的 Lua 语法/加载期错误**经 LuaMadeSimple 补丁打 stderr 不杀进程，但仍必须先
  `luac5.4 -p` 校验（已踩坑：Lua 5.4 无全局 unpack、嵌套函数不能用 `...`）。
  已实测：UE4SS 完整初始化、mod 加载、文件队列 who/whojson/hello/items（2466 个物品 ID）、
  `probe throw`/`probe throwasync`（故意触发绑定层 C++ throw）只报错不崩；
  **2026-09-14 修复**见下方「当前进展与状态」：旧版虽加载正常，实际 ProcessEvent/参数类型错误，不能把加载成功当成给物品成功。
- **玩家进服一会儿就 SIGABRT 的根因（已修复，2026-09-13）**：不是 Lua 代码本身，而是
  **进程级 C++ 异常运行时被劫持**：① libsteam_api.so 静态链旧 libstdc++ 并导出无版本的
  `__gxx_personality_v0`，抢先被动态链接器选中，与系统 libgcc_s unwinder 不兼容，安装
  landing pad 时直接 abort —— UE4SS 的 TRY/catch 全部失效，任何 C++ 异常（如绑定层
  overload throw）都杀进程；② PalServer 主程序又导出整套静态 libstdc++ 符号
  （`__cxa_throw`/`typeinfo`…），throw/catch 被劫持到游戏那份运行时，修了 personality 后
  会在 unwind 途中 SEGV。修复（双层）：a) `tools/palworld-ue4ss/shim-eh/` 产出
  `libgxxfix.so`（LD_PRELOAD 最前，personality 转发回系统 libstdc++ 真身，启动时
  console.log 可见 `[gxxfix] ... interposed`）；b) libUE4SS 用 `-static-libstdc++
  -Wl,--exclude-libs,ALL` 重链，C++ 异常运行时完全自洽。两者都在补丁包与面板资产里，
  重装/更新不会丢。**验证垫片在役**：控制台 `probe throw` 应返回
  `survived binding throw: pcall ok=false err=No overload found...` 且服务器不崩。
- **UE4SS 读 FText 不可取（本 build）**：`FText:ToString()` →
  `UKismetTextLibrary:Conv_TextToString` 的 native macro 里 path 式 `StaticFindObject`
  找不到 UFunction 会抛错（EH 修复后只是 Lua 错误，不再致命，但依然取不到文本）；
  `FText::StaticSize_Private` 也从未初始化（二进制里是 -1）。所以 mod 里
  **不要访问 FText，也不要访问不确定存在的属性**（1.0.4 的 `DT_ItemDataTable` 行结构体
  `PalStaticItemDataStruct` 没有 Name/TypeA/TypeB/Rarity/MaxStackCount），
  物品中文名只能离线提取；`UDataTable:ForEachRow`/`UStruct:ForEachFunction` 的 Lua 回调
  在本 build 也有栈 bug，安全用法只有 `GetRowNames()` + `FindRow()`（且只读确定存在的字段）。
- **计划任务「每日重启」会把已手动停掉的实例直接拉起来**（`gracefulRestart` 对非运行实例直接 start）；
  想长期停用实例要禁计划任务+关 auto_update+`systemctl disable`（见「当前进展与状态」帕鲁条目）
- `data/config.json` 曾被提交进 git（含面板密码哈希/实例 RCON 密码）：已用 filter-repo 重写
  全部历史并 force push（`081edfd` 起历史中无此文件），`.gitignore` 已排除 `data/` 和二进制；
  仓库必须 private（`saves/` 迁移存档的 ini 里含游戏管理员/RCON 密码）
- **七日杀 ModInfo.xml 必须有 `DisplayName` 字段**（V2/V3 系）：只有 `Name` 会被拒载
  （`does not define a non-empty DisplayName, ignoring`）；`Name` 只是内部 ID，面板列表显示 DisplayName
- **NexusMods 下载直链（download_link.json）要 Premium**；免费账号从下载页「Mod Manager 下载」
  按钮复制 nxm:// 链接（内含 key/expires 临时参数）粘到面板即可在线安装；搜索/文件列表免费
- 七日杀 telnet **TelnetPassword 留空 = 只监听本机回环**（面板走 127.0.0.1，最安全）；
  设了密码会监听所有网卡，需自行在防火墙放行 TelnetPort
- **七日杀 telnet 客户端直接 close 会往游戏 console.log 刷 IOException**（`TelnetClient_127.0.0.1:<端口>
  ... The socket has been shut down` + 堆栈，每条 telnet 命令一条，无害）：服务端要等下次往这条死连接
  写数据才发现断开，所以报错常在下一次 telnet 连接时才出现、端口是上一条连接的。面板 `telnetExit`
  收尾先发 `exit`（服务端自己干净关闭，只记 `connection closed`）已消除；**密码认证前绝不能发 exit**
  ——会被当成错误口令计入 TelnetFailedLoginLimit，10 次封本机
- **七日杀 V3.2 世界存档不在实例目录**：在 games 家目录
  `/home/games/.local/share/7DaysToDie/Saves/<世界>/<GameName>/`（RWG 世界的 `<世界>` 目录名可能是种子名），
  旧模板 backup_paths 只写实例目录 `Saves/`，世界存档从没进过备份（2026-09-30 已修，用 `~/` 路径语法）；
  同机多个七日杀实例共享这份 HOME 数据目录（serveradmin.xml 等同根，靠 GameName 区分存档）

## 当前进展与状态

- **给物品修复（2026-09-14，mod `2026-09-14.2`，已生产实测到账）**：只读核对生产 ELF vtable 与 `/proc/<pid>/mem` 反射数据定位：
  ① Linux UObjectBase 有双析构槽，布局漏了一个导致 ProcessEvent=0x260 指向 `ret`（0x441dff0），正确为 0x268（0x7b802a0）；
  ② FProperty 复用 FField 尾 padding，ArrayDim/ElementSize=0x34/0x38，不是 0x38/0x3C；
  ③ UEnum.Names=0x48，不是 0x40。已更新 `assets/palworld-ue4ss/layouts/`。
  **在线补测发现还必须重编译 `.so`**：`push_structproperty` 对真实 FStructProperty 做 `CastField` 返回空（Linux 早退未初始化 StaticClassStorage），
  随后解引用 `nullptr+0x78` → SEGV；隔离空世界 CDO `PlayerUId` 复现，栈在 `/tmp/gsp-cdo-backtrace.log`。
  新增 `patches/ue4ss-linux-palworld-properties.patch`：运行时 cast flags 校验+防空、FName setter 非 userdata 防护，
  `GSPanelPropertyBindingsVersion=1` 能力标记；Lua `2026-09-14.2` 无该标记拒绝玩家/写操作，selftest 也读 CDO GUID 四分量。
  .2 与新框架已在 00:52 部署：隔离/生产 selftest（含 GUID）、playercheck、离线查询/发放拒绝全部通过。
  当前 PID=798975、NRestarts=0；框架 SHA256 `c4cec6b7864ddaf366d8bceb0080f7b8d9639cdb1a002b611db8ef1a613bc0d4`。
  新框架已同步到面板库存 `data/ue4ss/libUE4SS.so`、生产实例及 `/home/games/ue4ss-build/libUE4SS.so`；
  匹配 debug 备份 `/home/games/ue4ss-build/libUE4SS-properties.so.debug`。构建脚本依次应用基础补丁与 properties 补丁，脏源码树拒绝覆盖。
  `probe playercheck` 可在空世界 CDO 覆盖 flags/bool/Owner/GUID/name，不需要真人重复上线复现（新框架已通过）。
  **背包 CDO 不能调用物品查询**：`GetInventoryTypeFromStaticItemID` 需要 GameInstance，默认对象无世界，PalUtility.cpp:3140 会 fatal；
  `probe inventorycheck <UID>` 已改为只读真实在线玩家归属校验后的 Wood 类型/数量，不碰 CDO 背包。新的纯 Lua 回归 106 assertions/28 命令（含旧框架能力缺失拒绝），`mods_test.go` 校验资产原子替换不伤硬链接源。
- 旧 mod 的 `PlayerName` 属性不存在（实际 `PlayerNamePrivate`）；GetPlayerName/GetInventoryData 因空 ProcessEvent 得到空值。
  新版每条命令在游戏线程现场扫描，排除 CDO/inactive/bot/无 NetConnection 对象，使用 PlayerUId（FGuid A/B/C/D 格式为 32 位大写 hex），
  不缓存 UObject、不用序号猜玩家、同名拒绝；背包 OwnerPlayerUId 必须与目标一致。SteamID 不再虚假宣称支持。
- 给物品固定 `inv:AddItem_ServerInternal(FName(id), count, false, 0.0, true)`，UFunction 是**可调用 userdata**，
  不可用 `type(f)=="function"` 判可用；**字符串不能代替 FName**（旧试探曾在 push_nameproperty SEGV，pcall 拦不住）。
  返回 `EPalItemOperationResult`（0=Success，1=NoOperation，16=空间不足）；仅返回 0 且 `CountItemNum` 前后恰好增加指定数量才报成功，
  不做候选重试，部分到账/异常提示先核对。数量 1～10000，默认 1，前端确认+防重复提交，API 校验物品 ID 来自物品库。
- `modqueue.go` 协议 v2：请求/响应首行 `gsp2:<随机请求ID>`，响应 tmp+rename；忽略迟到/旧响应，超时不重发。
  只有 mod-alive 版本+PID 匹配当前游戏进程才发送；安装新脚本不代表运行版本更新，须重启。mod `probe selftest` 校验 ProcessEvent/FString/FName/UEnum，旧布局 fail closed。
- 隔离验证脚本已修正：旧 `cp -al` 后 cp 覆盖 `.so/mod` 会反向写坏生产 inode；新版可变目录全部断链、新建空世界，
  独立 transient unit/端口、网络仅 localhost、内存/CPU 上限、结束必停；目录 `/home/games/gsp-ue4ss-test.*` 保留排查。
- **验证完成**：106 项纯 Lua assertions/28 队列命令、Go test/vet/build、JS/shell/Lua 语法通过；
  `NATIVE_CHECK=1 test-ue4ss.sh` 调用 `test-native.py`（严格限定隔离目录），覆盖原生 selftest、玩家 CDO GUID/名字/flags、离线查询/发放拒绝。
  生产 `/players` 正确显示 `some_test0` + 稳定 UID；真实背包归属校验与 Wood 类型/数量查询通过。
  **00:55:56 经面板 API 仅发 Wood ×1，返回枚举 0，CountItemNum 0→1，ok=true（已核验）**，PID=798975/NRestarts=0；随后 REST Save=200。
  生产 API 负数/小数/10001/未知 ID 均 HTTP 400 拦截，离线 UID 返回失败；未对真实满背包/多玩家/特殊物品做实测（这些分支有模拟回归）。
  停服备份：`/home/games/backups/manual-pre-give-fix/20260914-{001736,005157}/before.tar.gz`（含 Pal/Saved、旧插件/布局/start.sh，games:games 600），
  旧面板 `/tmp/gspanel-before-give-20260914-{001736,005157}`。原有 `/tmp/catch3.sh` 循环 gdb 已停止并 detach；无残留测试游戏 unit。
- **七日杀控制台/配置/Mod 三件套（2026-09-30）**：① 控制台命令经 telnet（新增 telnet.go，模板
  `rcon.type=telnet`），快捷按钮 lp/saveworld/say/give/kick/ban 等，优雅停服走 say→saveworld→shutdown（实测存档干净退出）；
  ② serverconfig.xml 从 raw 文本升级为全量 GUI（format `xmlkv` + 68 项 schema，全部中文详解分组折叠）；
  ③ Mod 管理页（上传 zip/URL 直链/NexusMods 在线，启停/删除，mods ↔ Mods.disabled）；
  Nexus key 存 config.json `nexus_api_key`（设置页配置）。实测：telnet 命令往返、真实 mod
  解压安装后游戏 `Loaded Mod`、DisplayName 拒载坑已写入解析器与已知坑。回归：configfile/telnet/modmgr/nxm 单测 + GSP_TELNET_LIVE=1 在线冒烟。
- **七日杀实例上线（2026-09-30，`7days`/AppID 294420）**：修复上面两条坑后启动跑通
  （世界生成完成、`GameServer.LogOn successful`）；游戏端口 26900 TCP+UDP（另 26902 UDP EOS），
  ufw 已放行 26900:26902 TCP/UDP，**云安全组需自行放行**。面板新增 `Environment=HOME`（renderUnit）
  与 `ensureSteamSDK64`，go test/vet 通过。实例目录 `libstdc++.so.6.i386-unused`（32 位误放）
  已改名作废可删；palworld-1 实例仍在面板里（存档/备份未动），迁移删除由用户决定。
- **帕鲁已彻底停机（2026-10-01）**：用户专注 7days，palworld-1 已停+unit disable（开机不再自启），
  两个计划任务（每日 04:30 备份 / 05:00 重启）已停用、`auto_update` 关闭，config.json 直改后重启面板生效。
  **坑：面板停掉的实例会被每日重启计划任务拉起来**——`gracefulRestart` 对未运行实例是「直接启动」，
  手动停服到点照样开机（journal 里 Started 时间=计划任务 last_run 即可确认）。要长期停用某实例必须
  ①禁用它的全部计划任务（尤其 restart）②关 auto_update（updateInstance 虽不启动停掉的实例，但白耗磁盘）
  ③`systemctl disable gspanel-<名>`（面板的停止只 systemctl stop，unit 仍 enabled，重启机器会自启）。
- **七日杀 SandboxCode 预设（2026-09-30）**：17 个官方内置预设（难度 6 档 + 整活 11 档）已做进
  配置页一键填充 + `gso` 解码按钮。编码格式：`'A'` + 若干 3 字符块（2 字符 base-26 选项 ID + 1 字符值
  索引，默认项省略）；选项 ID = `SandboxOptions` 枚举声明序（dnfile 从 Assembly-CSharp.dll 提取，V3.2 共 166 项）。
  V3.0 预设代码 → V3.2 翻译规则（源：Labyricorn/7D2D-Sandbox-Settings-Reports）：147/166 个 ID 不变；
  44/45/46 昼夜拆分（旧 BiomeEnemyDensity 等 → Day 保留原位 + 夜间孪生 156/157/158 同值）；
  77/100/136（Loot/Crafting/TraderMaxTier）值数组前置了 Default 档，值索引 +1。
  **两个复杂预设（Caveman Life/Undead Matinee）真机回环校验通过**（游戏重新编码与生成代码逐字符一致）；
  游戏升级后预设需按此规则重新翻译（`/tmp/presets_v32.json` 生成脚本思路见上）。
- **七日杀 V3 配置/难度机制（2026-09-30 实测 V3.2.0）**：V3.0 起 30 个玩法键（GameDifficulty、
  DropOnDeath/DropOnQuit/DeathPenalty、血月系列、LootAbundance、XPMultiplier 等）已从
  serverconfig.xml 移除，全部编入 SandboxCode——用户问「死亡掉落怎么配置里没有」属正常，
  它是沙盒选项（DropOnDeath/DeathPenalty/LoseItemsOnDeath*/DegradeItemsOnDeath* 4 项）。
  **权威核对用 telnet `gso true`**（读当前世界实际生效值，控制台页「沙盒选项」/配置页
  「解码当前值」按钮即它）；改 SandboxCode 保存+重启即对已有存档生效（沙盒层追溯生效），
  **无需新世界**，只有 GameWorld/WorldGenSeed/WorldGenSize 需开新档。坑：SandboxCode
  解码失败不报错、静默跑默认规则，改完必须 gso 核对；`setgamepref` 可在线改单项但不持久，
  重启回代码值；生成代码的客户端版本须与服务器一致（3.1 起部分选项结构变了）。
  实测 7days 服当前 `Sandbox Code: A`（全默认：DropOnDeath=1/All 全掉、DeathPenalty=1/XP Only）。
  逐项修改用面板配置页「沙盒选项编辑器」（见下条），不用再去游戏客户端生成代码。
- **七日杀沙盒选项可视化编辑器（2026-09-30）**：配置页 SandboxCode 字段新增「沙盒选项编辑器…」
  （`sandbox.go` + 前端 `openSandboxEditor`）：165 项按官方分类分组下拉（中文名/悬停说明/值标签/
  默认值标注），逐项改完自动生成代码填回配置框；支持「从服务器读取生效值」（gso）、粘贴代码
  载入、只看已修改、搜索。选项表 `assets/7dtd-sandbox/options.json` 由
  `tools/7dtd-sandbox/extract.py` 从游戏本体提取（DLL：SandboxOptions 枚举序=选项 ID、
  SetupOptions IL 的值集数组/默认值（InitializeArray 读 FieldRVA）；Localization.csv：中文文案），
  **已与运行中服务器 gso 全量比对：165/165 默认值索引一致**；游戏更新后重跑脚本+重启面板即可。
  API：`GET /api/sandbox/tables/{id}`、`POST /api/sandbox/decode|encode`、`GET .../sandbox/live`。
  坑：官方预设代码可能含等于当前默认值的块（如 Warrior 的 `BND`=AISmellMode，V3.0 旧默认残留），
  规范编码会省略这种块（语义不变）；单测往返按语义等价比较而非字符串相等。
- **Mod 中文翻译（2026-09-30）**：Mod 管理页名称/描述一键「翻译成中文」（内置词典覆盖游戏自带
  TFP 组件，免费接口兜底，可选配 DeepSeek 等 OpenAI 兼容接口提升质量，译文缓存 data/modlang.json）；
  七日杀自带的 3 个 mod 是官方组件：0_TFP_Harmony（Harmony 补丁框架封装，其他组件的依赖）、
  TFP_CommandExtensions（服务器命令扩展）、Xample_MarkersMod（网页地图标记示例），勿删。
  踩坑：`modLangStore` 持锁调 `modLangLoad` 导致重入死锁，单测抓出已修（确保加载要在加锁前）。
- **Mod 商店（2026-09-30，本地目录模式）**：全量拉取 Nexus 目录（9028 个）缓存到
  `data/nexus-mods.json`，浏览/搜索/分类/端侧/版本筛选/排序/翻页**全在前端内存做**（瞬时、无翻页地狱）；
  「同步 Mod 库」后台任务全量刷新（80/批、createdAt 正反两遍翻页补齐、按 mod_id 去重，实测 2.5 分钟）。
  页面大小 50/100/200/500 可选；分类/常见标签中文走内置词典（离线秒翻）。
  **坑：v1 REST 没有 mods/search.json**（会被路由成 mod id）；浏览/搜索走 GraphQL v2
  （`POST api.nexusmods.com/v2/graphql`，`mods(filter/sort/offset/count)`，单次上限 80 条，
  `nameStemmed:[{value,op:WILDCARD}]` 搜索、`categoryName` 字符串过滤；分类表 v1 `games/7daystodie.json`）。
  **Nexus 无服务端/客户端结构化字段**，端侧徽标（server/client/both/client?）是名称/简介/标签关键词启发式；
  版本提示只认 `V3.2`/`A21` 类明确标记（早期版本把裸数字当版本号，80% 误报，已收紧），
  `modSideHint`/`gameVersionHint` 有单测。安装：免费账号 nxm 链接引导，Premium 自动切一键安装；
  API key 在 config.json `nexus_api_key`（明文备份 data/nexuskey.bak，不入 git）。
- **七日杀「给物品」物品选择对话框（2026-10-01，与帕鲁同款）**：控制台页「管理: 给物品…」，
  `openGiveItemDialog` 两游戏共用（后端 `give_kind` 视图字段切换细节）：在线玩家下拉（telnet `lp`
  解析，**用实体 ID 发 give**，玩家名带空格也安全）、全量物品搜索（游戏文件解析 1047 项含 mod，
  中文名来自 Localization.csv，18 个分类 chips）、数量快捷档/满栈、品质 1-6 可选（telnet 专属）、
  最近使用。服务端 `POST /instances/{name}/give` 统一入口（物品 ID 必须来自物品库，数量 1-10000，
  品质 0-6，成败按 telnet 回显错误措辞 `dtdGiveOK` 判定），成功/失败都进事件日志；
  **give 是把物品掉在玩家面前，不是进背包**。模板里手输 give 快捷按钮已删（防重复）。
  物品库缓存 1 分钟+文件指纹（items.xml/Localization.csv/Mods 增删自动失效），
  对话框「重新扫描游戏文件」手动强刷。回归 `items7dtd_test.go`（解析/分类/过滤/lp 解析/
  give 成败判定 + 真机冒烟），已部署并 API 实测（物品库/玩家列表/give 错误路径）。
- `.pi/` 为会话任务/诊断产物（可能含私密上下文），已加入 gitignore，不提交。
- **Mod 安装器支持嵌套压缩包 + 未识别内容报告（2026-09-30）**：起因是用户上传「（3.0-3.2）1武器大师-
  现代战争2.9正式版」后只装出 4 个安安实用件（大背包/HUD/DLL/减草，160KB），与 39 款枪械的武器包对不上——
  排查结论：上传事件 21:43:13 成功、无中断，但嵌套子压缩包/不认识的文件被静默跳过，无法判断包内结构
  （tmp 解压目录用完即删）。现在：嵌套 zip/tar.gz 再解一层装出；包里没被安装的内容列入 `skipped`
  （上传响应/弹窗/事件日志都会体现）；根目录无 ModInfo.xml 的报错也附未识别清单。排查此类问题先看事件日志。
  另一个通用坑：**装完 mod 必须重启游戏进程才加载**（客户端还要装同版本 mod 才能进服）。
- **Mod 安装支持 7z/rar + 备份排除大资产（2026-10-01）**：用户两次上传「（3.0-3.2）1武器大师-现代战争2.9正式版」失败
  的根因是**面板只支持 zip/tar.gz，中文 mod 站常用 7z**（另：21:43 传的那个 zip 里其实只有 4 个安安实用件，
  与包名对不上，属于站方打包/选错文件）。现在：① extractArchive 支持 7z/rar（外部 `7z`/`7za`，已 apt 装
  p7zip-full；`check7zEntries` 预检 -slt 目录防路径穿越/符号链接/加密条目）；② `backup_paths` 新增 `!模式`
  tar --exclude，7dtd 模板已排 `!Mods/*/Resources`、`!Mods/*/UIAtlases`（否则武器类 mod 的 1.2GB unity3d
  每份备份都打包，备份盘必爆）。武器大师 mod 已手动装好（`Mods/（3.0-3.2）1武器大师-现代战争2.9正式版/`，
  1.2GB，目录775/文件664，p7zip 默认 700 已归一）并重启验证：`WeaponMaster_ModernWarfare (3.3)` + ANAN 四件
  全部 Loaded。实测带 mod 的备份仅 4.3MB（含 ModInfo/dll/Config+世界存档）。
  磁盘注意：/ 仅剩 ~5.7G（91%），大 mod/存档增多时先清旧备份（`/home/games/backups/`）。
- **备份/新世界支持实例外路径（2026-09-30）**：模板 `backup_paths`/`world_paths` 新增 `~/`（games 家目录）与
  `{config:配置键}` 占位符（pathspec.go，展开值过校验，配置值带 `..`/绝对路径直接报错不静默）；备份 tar 带 `-P`
  （实例外路径以绝对名进归档，旧全相对名备份恢复兼容），恢复前 `checkBackupMembers` 拒 `..` 逃逸/越界绝对名，
  恢复后实例外文件 chown 回 games。7dtd 模板补 `~/.local/share/7DaysToDie`（实测备份
  20260930-234845 已含 Saves/Navezgane/MyGame）+ world_paths → 实例页「创建新世界」按钮对 7dtd 可用
  （停→备份→删当前世界存档→重启；候选含 GameWorld/WorldGenSeed 两种目录名，任务日志列出实际删除路径）。
  单测 pathspec_test.go（路径解析/占位符/成员校验/归档成员命名）；go test/vet/build 通过，已部署。
- **七日杀 telnet 断连 IOException 已消除（2026-10-02）**：用户报 console.log 里
  `IOException in TelnetClient_...: The socket has been shut down` 刷屏，排查定性为面板每条命令
  连完即关、服务端往死连接写数据的无害噪音（每条命令一条，报错滞后到下一次连接时才打）。
  修复：`telnetExec` 收尾发 `exit` 让服务端干净关闭（`telnetExit`，认证前不发防封 IP），
  真机验证只剩 `connection closed`；回归 `TestTelnetExitOnlyAfterAuth` + `GSP_TELNET_LIVE=1` 冒烟通过，已部署。
- **异地备份当前失效（2026-09-30 发现）**：sync_targets 的 yecao2/yecao 主机名 DNS SERVFAIL 解析不了
  （面板用户 ssh config 里也只有 huoshan），备份任务因此报「同步到 yecao2, yecao 失败」；与备份改动无关，
  待恢复 DNS/hosts/Host 配置或改 sync_targets。

## 会话恢复 checklist

1. 读本文件 → 2. 看 `git log --oneline -5` 和 `git status` → 3. 改完更新本文件。
