# GSPanel · 轻量游戏服务器管理面板

自用的游戏服管理后台。Go 单二进制（无运行时依赖，内存占用 ~11MB），前端页面内嵌，
游戏进程由 systemd 托管（面板重启/崩溃不影响游戏），新游戏通过 JSON 模板扩展。

- 面板源码与二进制：clone 到任意目录皆可（本机在 `/home/ubuntu/gspanel/`，属主 `ubuntu`），状态跟随二进制所在目录
- 面板服务：`gspanel.service`（开机自启，以普通用户运行，本机为 `ubuntu`）
- 面板权限：systemd `User=ubuntu` + `AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER`
  （FOWNER 用于对 games 属主文件 chmod，如安装扩展命令时复制 libUE4SS.so/start.sh）；
  写 unit、启停实例、装 32 位依赖经 `/usr/local/sbin/gspanel-priv`（sudoers 白名单），切 `games` 身份走 `sudo -u games`
- 游戏实例服务：`gspanel-<实例名>.service`（独立 unit，崩溃自拉起、开机自启）
- 访问：`http://100.64.0.3:8800`（仅 Tailscale 内网）或 `http://gspanel.tail.yyplab.site:8800`

## 架构

```
浏览器 ──HTTP──> gspanel (Go, 单进程)
                  ├─ 内嵌前端（static/ 经 go:embed 打进二进制）
                  ├─ REST API（api.go，会话 Cookie 认证，登录限流）
                  ├─ steamcmd 任务执行器（安装/更新，日志 SSE 推送）
                  ├─ systemd 生成器（每实例一个 unit + start.sh）
                  ├─ Source RCON 客户端（控制台命令/优雅停机）
                  ├─ 游戏 REST API 客户端（Palworld 等 RCON 丢响应的游戏，查询/广播/踢禁走 REST）
                  ├─ 计划任务调度（定时重启/备份/更新）
                  ├─ 版本轮询（实例开启自动更新后，每 30 分钟对比 Steam buildid；有玩家在线则广播等待，无玩家持续 10 分钟才更新）
                  ├─ 事件日志（data/events.jsonl：启停/更新/备份/计划任务/异常退出时间线，侧栏「事件日志」页；看门狗识别非面板发起的进程退出）
                  └─ 监控（/proc + systemctl show，无外部 agent）
```

- 面板以普通用户运行（本机 `/home/ubuntu/gspanel`），只有写 unit / 管实例 / 装依赖走特权助手
- 游戏以 `games` 用户（uid 5）运行，实例目录 `/home/games/instances/<名>/`
- 备份在 `/home/games/backups/<名>/`（tar.gz，保留策略按份数）
- 状态都在 `面板目录/data/`：`config.json`（账号/实例/计划任务）+ `events.jsonl`（事件日志），无数据库

## 新服务器安装

一键脚本（幂等，可重复跑）：装 Go、建 `games` 用户、构建、写 systemd unit、配特权助手、开机自启。
面板以普通用户运行（默认取 sudo 调用者；`PANEL_USER=用户名 sudo ./deploy.sh` 可覆盖）。
clone 到任意目录都行：`BaseDir` 运行时取二进制所在路径（main.go），面板状态 `data/` 和
用户模板 `templates/` 都跟随它；游戏侧路径固定 `/home/games`（实例/备份/steamcmd），与面板位置无关。

```bash
git clone <仓库地址> gspanel && cd gspanel          # 位置随意，脚本以所在目录为准
sudo ./deploy.sh                                  # 面板默认以调用者身份运行
```

跑完按提示：`journalctl -u gspanel | grep 密码` 拿首次随机密码登录，
然后到「设置/环境」装 steamcmd 与 32 位依赖，即可开始建实例。
防火墙按需放行 8800（建议仅内网，见「网络与安全」）。

### 从旧服务器迁移存档

推荐走 git 仓库带存档（`saves/`，每实例只存最新一份 tar.gz，作迁移种子）：

1. 旧机：实例 → 备份 → 立即备份（切换前先停服再备，保证一致），然后
   `./push-saves.sh` 收进 `saves/`，手动 `git add saves/ && git commit -m 'update saves' && git push`
2. 新机：clone 仓库（位置随意）→ `./deploy.sh`（自动把 `saves/` 放进备份目录，
   已有同名实例的机器会跳过，不会回灌来源机）
3. 新机：用同一模板新建**同名**实例 → 安装游戏 → 实例「备份」页直接点「恢复」
   （恢复时自动按面板记录重写 ini 里的端口/管理员密码/服务器名，游戏设置其余项随备份原样落地，无需重配）
4. 计划任务（每日备份/重启）存在旧面板 config.json 里，不随 git 走，需在新面板重新添加
5. 玩家改用新面板首页显示的 IP:端口 连接

也可以不走 git：旧机备份页下载 tar.gz，新机备份页「上传备份」→「恢复」。

注意：仓库必须 private（备份里的 ini 含游戏管理员/RCON 密码）；`saves/` 只放迁移种子，
日常自动备份仍留在本机 `/home/games/backups/`（git 存二进制只增不减，频繁推送会让仓库膨胀）。

## 日常维护

### 重新构建部署（改代码后）

```bash
cd ~/gspanel && sudo ./deploy.sh       # 本机路径（普通用户 ubuntu）；已安装环境只构建+重启面板，幂等
```

面板重启不影响正在运行的游戏。需要 Go 1.22+（脚本会自动检测安装）。

### 查看日志

```bash
journalctl -u gspanel -f                    # 面板
journalctl -u gspanel-palworld-1 -f         # 游戏（同控制台输出）
tail -f /home/games/instances/palworld-1/logs/console.log
```

### 手动操作游戏实例

```bash
systemctl start|stop|restart gspanel-palworld-1
```

### 首次启动/忘记密码

首次启动会在 journal 里打印随机管理员密码（`journalctl -u gspanel | grep 密码`）。
重置密码：停面板 → 删 `data/config.json` 里的 `password_salt`/`password_hash` 两行 →
启动后用打印的新密码登录（实例数据不受影响）。

## 配置（data/config.json）

| 字段 | 说明 |
|---|---|
| `bind` / `port` | 面板监听地址（当前 0.0.0.0:8800，由 ufw 限制只放 tailnet） |
| `public_ip` | 公网地址覆盖（仪表盘分享链接用；留空自动探测，设置页可改） |
| `password_salt/hash` | 管理员密码（设置页修改） |
| `instances` | 实例记录：端口、启动参数、管理员密码、计划任务 |

## 游戏模板（扩展新游戏）

模板 = 一个 JSON，放 `面板目录/templates/` 重启面板生效，或在「新建实例」页从 URL 导入。
内置 14 个：palworld / valheim / cs2 / 7dtd / rust / satisfactory / zomboid / enshrouded /
gmod / tf2 / ark-se / terraria / corekeeper / dst。

```jsonc
{
  "id": "mygame",                 // 小写字母/数字/中划线
  "name": "显示名",
  "steam_app_id": 123456,         // SteamDB 可查
  "anonymous_login": true,
  "executable": "./start.sh",     // 相对实例目录
  "default_args": ["-batchmode"],
  "stop_mode": "sigterm",         // rcon | sigterm
  "stop_warn_secs": 10,           // rcon 模式停机前广播秒数
  "rcon": { "type": "source", "port_key": "rcon" },   // 可选
  "rest_api": {                 // 可选：把指定控制台命令改走游戏 REST API（RCON 丢响应的游戏用）
    "port_key": "rest",         // 对应 ports 里的键
    "commands": {               // 键为命令动词（大小写不敏感）；body 里 "$arg" 会被命令参数替换
      "ShowPlayers": { "method": "GET", "path": "/v1/api/players", "format": "players" },
      "Broadcast":   { "method": "POST", "path": "/v1/api/announce", "body": { "message": "$arg" } }
    }                           // format: players | metrics | kv，空则原样返回
  },
  "console_buttons": [          // 可选：控制台快捷按钮，缺省用内置默认
    { "label": "在线玩家", "command": "ShowPlayers" },
    { "label": "广播…", "command": "Broadcast", "prompt": "广播内容:" }  // prompt: 点击先弹输入框
  ],
  "ports": [
    { "key": "game", "default": 8211, "proto": "udp", "desc": "游戏端口", "public": true }
  ],
  "configs": [{                   // 可选；format: option-settings | kv | raw
    "path": "cfg/server.ini", "format": "kv",
    "seed_from": "cfg/default.ini",       // 安装后从此复制生成
    "schema": [{
      "key": "MaxPlayers", "label": "人数上限", "type": "int",
      "default": 32, "min": 1, "max": 32, // 仅在有官方/可靠依据时填，无依据留空不展示
      "note": "32 为游戏硬上限"           // 补充说明（已知问题/注意事项）
      // 可选能力：presets（预设一键填充）/ decode_command（在服务器执行命令解码当前值）/
      // sandbox_options（沙盒选项编辑器，七日杀 SandboxCode 专用，值为选项表 ID）
    }]                                    // type: string|password|int|float|bool|select
  }],
  "backup_paths": ["save"],
  "notes": "显示在新建实例页的提示"
}
```

模板里没有官方的下载源（Valve 只提供 steamcmd），AppID 查 SteamDB，
启动参数/配置格式看各游戏官方文档或社区 wiki。游戏自带的默认配置由
`seed_from` 从 steamcmd 下载的文件里复制，不依赖网络资料。

## API 速查（curl 用）

```bash
TOKEN=$(curl -s -X POST localhost:8800/api/login -H 'Content-Type: application/json' \
  -d '{"password":"xxx"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["token"])')
H="Authorization: Bearer $TOKEN"

curl -s localhost:8800/api/system -H "$H"                    # 系统状态/公网IP/依赖
curl -s localhost:8800/api/instances -H "$H"                 # 实例列表
curl -s -X POST localhost:8800/api/instances/palworld-1/start -H "$H"
curl -s -X POST localhost:8800/api/instances/palworld-1/command -H "$H" \
  -H 'Content-Type: application/json' -d '{"command":"ShowPlayers"}'
```

主要端点：`POST /api/login`、`GET /api/system`、`GET/POST /api/instances`、
`POST /api/instances/{n}/install|update|start|stop|restart`、`GET .../console/stream`(SSE)、
`POST .../command`、`GET/PUT .../config`、`.../backups`、`.../schedules`、
`GET /api/tasks`、`POST /api/templates/import`、`POST /api/settings/password|public-ip`、
`GET /api/sandbox/tables/{id}`、`POST /api/sandbox/decode|encode`、`GET .../sandbox/live`（七日杀沙盒选项编辑）、
`GET .../items`、`GET .../items/refresh`、`GET .../players`、`POST .../give`（给物品对话框：帕鲁走 mod / 七日杀走 telnet，物品 ID 必须来自物品库）。

## 网络与安全

### 运行身份与特权

面板本身以普通用户运行，不落 root 家目录；需要 root 的动作集中在一个受审计的助手脚本：

- `/usr/local/sbin/gspanel-priv`（root:root 0755，由 `deploy.sh` 安装）：只接受
  `unit-write` / `unit-remove` / `ctl <start|stop|restart|enable|disable>` / `install-deps`，
  unit 名严格匹配 `gspanel-<实例名>.service`
- `/etc/sudoers.d/gspanel`：只授权面板运行用户执行上面这个脚本（root），以及 `(games)` 切换
- 面板 unit 用 `AmbientCapabilities=CAP_CHOWN CAP_DAC_OVERRIDE`（写 games 属主文件后 chown 回 games）；
  **不要设 `CapabilityBoundingSet`**，否则会连带限制 setuid root 的 sudo，缺 CAP_SETUID 就切不到 games

- 面板只对 Tailscale 内网开放：`ufw status` 中 `8800/tcp ALLOW 100.64.0.0/10`
- 公网应急通道（tailnet 挂了时）：
  `ssh -L 8800:127.0.0.1:8800 root@115.190.6.220 -p 10086`，然后开 `localhost:8800`
- 游戏端口（8211/udp）公网开放给朋友连，RCON/REST 端口不要公网放行
- Tailscale：`--accept-dns=false` 入网（Headscale 不接管本地 DNS）；
  `/etc/hosts` 固定解析了控制域名防抖动
- Palworld 当前计划任务：每天 04:30 备份（留 10 份）、05:00 重启（防内存泄漏）

## 已知坑（踩过的，别再踩）

1. **Palworld v1.0 配置文件**：`PalWorldSettings.ini` 的字符串值必须带引号
   （`ServerName="xxx"`），不带引号静默回退默认值。面板的写入逻辑已处理（保留原引号风格）。
2. **世界参数固化**：经验倍率等在创建世界时写进存档，改 ini 只对新世界生效；
   服务器名/密码/端口每次启动生效。
3. **Palworld RCON 丢响应**是游戏自身 bug（命令会执行，但响应经常不发回）。面板的
   `ShowPlayers/Info/Metrics/Broadcast/KickPlayer/BanPlayer` 已改走官方 REST API（模板 `rest_api` 声明），其余命令仍走 RCON。
4. **steamcmd 报 "Missing configuration"**：删 `~/steamcmd/appcache` 与 `~/Steam/appcache` 重试。
5. **systemctl show --value 多属性顺序不保证**，解析要用 key=value 形式。
6. 修改 `Pal/Saved/` 下任何文件后属主保持 `games:games`，否则游戏写不动。
7. **面板 unit 不能设 `CapabilityBoundingSet`**：setuid root 的 `sudo` 也受它约束，缺 CAP_SETUID 会导致
   `sudo -u games` 失败（表现为「环境」页依赖检测全部 false）。面板自身权限用 `AmbientCapabilities` 限定即可。
8. **Palworld 管理员给物品**：官方没有 give 类命令；原生 Linux 用**自行修复重编译的 UE4SS**（LD_PRELOAD+Lua mod）
   实现。面板原生管理：「设置/环境 → Palworld 扩展命令」上传/导入/下载框架二进制，
   实例「设置 → 扩展命令」一键安装/卸载/更新（安装后重启生效），控制台页出现
   「扩展: 在线玩家/给物品/给经验」按钮（走文件队列，非 RCON）。
   「给物品」是**物品选择对话框**：在线玩家下拉 + 全量物品搜索（游戏内中文名/内部 ID，2466 项）、
   数量快捷档、最近使用、可「从游戏刷新物品库」。玩家按稳定 UID 选取（支持完整名字，不支持 SteamID），
   数量限 1～10000 整数、提交前确认；只有游戏返回成功且背包增量吻合才报告到账，
   部分到账/超时请先检查背包，勿直接重复发放。中文名来自面板内置基线
   （`assets/palworld-items/palworld-zh.json`，从游戏 pak 的 `L10N/zh-Hans/.../DT_ItemNameText_Common` 离线提取，
   工具 `tools/palworld-ue4ss/extract-items-zh.py`），刷新时用 mod 导出的 ID 与基线按 ID 合并。
   框架补丁与构建脚本在 `tools/palworld-ue4ss/`，运行时资产在 `assets/palworld-ue4ss/`（embed 进面板）；
   游戏更新后重新编译 `libUE4SS.so` 并在面板重新「安装」即可。
9. **Palworld+UE4SS 的 C++ 异常会杀进程（已修复）**：libsteam_api.so 导出坏的
   `__gxx_personality_v0` 抢占进程级符号解析、PalServer 主程序又导出整套静态 libstdc++
   异常运行时符号，导致 UE4SS 绑定层任何 C++ throw 在 unwind 时 abort/SEGV（曾表现为
   「玩家进服一会儿服务器就崩」）。修复是双层的：LD_PRELOAD 最前的 `libgxxfix.so` 垫片
   （personality 转发回系统 libstdc++）+ libUE4SS 用 `-static-libstdc++ --exclude-libs,ALL`
   重链（异常运行时内化）。均在 `tools/palworld-ue4ss/`（shim-eh/ 与补丁包），面板安装时自动下发。
   验证：控制台发 `probe throw` 应返回 Lua 错误信息且服务器不崩。
10. **UE4SS 读 FText 不可取**（本 build）：`FText::ToString` 走的
   `UKismetTextLibrary:Conv_TextToString` native macro 里 `StaticFindObject` 找不到 UFunction
   （path 式函数查找失效）会抛错（EH 修复后只是 Lua 错误，不再致命）。
   因此 mod **不要访问 FText/不确定存在的属性**（`DT_ItemDataTable` 行结构体也没有 Name/TypeA 等字段），
   物品中文名改为面板侧离线提取；`FText::StaticSize_Private` 也未初始化（值为 -1），修复后才可能用 FText。

11. **UE4SS 初始化成功但玩家名为空/给物品接口不可用（已修复）**：Palworld 1.0.4 Linux
    的 `ProcessEvent` 槽位应为 0x268（旧 0x260 指向空函数），FProperty 与 UEnum 的字段布局也有差异；
    UFunction 是可调用 userdata，物品参数须显式 `FName(id)`。读取玩家 UID 还需结构体绑定补丁：
    原 CastField 依赖未初始化的静态类信息，返回空指针导致 SEGV；新版按运行时类型校验并防空。
    **框架 .so、mod 和布局需一起更新并重启**，Lua .2 会拒绝缺少修复标记的旧框架。
    文件队列 `probe selftest` 无副作用校验原生调用链及默认玩家 UID，详见 `tools/palworld-ue4ss/README.md`。
12. **七日杀 `give` 是把物品掉在玩家面前**，不是直接进背包（自用进包才走 `giveself`）；
    物品 ID 必须精确匹配（大小写敏感），面板「给物品」对话框从游戏文件解析物品表免手敲。
    mod 新增物品点对话框里的「重新扫描游戏文件」。`give` 的 telnet 回显没有稳定成功标记，
    面板按错误措辞判定成败（如 `Playername or entity id not found.`），最终以控制台日志为准。

## 测试

```bash
PATH=/usr/local/go/bin:$PATH go test ./...
PATH=/usr/local/go/bin:$PATH go vet ./...
lua5.4 tools/palworld-ue4ss/test-mod.lua
node --check static/app.js
```

E2E（headless Chromium，真实浏览器回归）：

```bash
# 1. 临时注入测试密码（测完恢复）
cp /home/ubuntu/gspanel/data/config.json /tmp/cfg.bak
python3 -c 'import json,hashlib;p="/home/ubuntu/gspanel/data/config.json";d=json.load(open(p));
d["password_salt"]="tmp_test_salt";d["password_hash"]=hashlib.sha256(b"tmp_test_salt:testpass123").hexdigest();
json.dump(d,open(p,"w"))'
sudo systemctl restart gspanel
# 2. 跑测试（Node 22 在 /opt/node22，playwright 在 /root/e2e）
cd /root/e2e && /opt/node22/bin/node panel.test.js
# 3. 恢复
sudo cp /tmp/cfg.bak /home/ubuntu/gspanel/data/config.json && sudo systemctl restart gspanel
```

前端是纯手写 JS（无框架），改完 `node --check static/app.js` 再过 E2E。

## 文件清单

```
/home/ubuntu/gspanel/
├── gspanel            # 编译产物（运行时唯一需要的东西，已 gitignore）
├── deploy.sh          # 幂等一键部署：裸机全装 / 已装只构建+重启面板，saves/ 自动就位
├── priv/gspanel-priv  # 特权助手（安装到 /usr/local/sbin）：写 unit / 启停实例 / 装依赖，参数严格校验
├── push-saves.sh      # 收各实例最新备份到 saves/ 作迁移种子（git 提交留人工）
├── saves/             # 迁移存档种子（<实例名>.tar.gz，每实例最新一份，git 跟踪）
├── *.go               # 后端：main/config/auth/api/instance/systemctl/steamcmd/events/
│                      #   rcon/rest/telnet/tasks/scheduler/backup/monitor/configfile/templates/
│                      #   modmgr/nexus/modlang/mods/palworlditems/items7dtd/sandbox/netinfo/util/stream
├── tools/             # 游戏侧物料/提取脚本（palworld-ue4ss、7dtd-sandbox 选项表提取）
├── assets/            # 内嵌资产（go:embed）：palworld 物品库/UE4SS、7dtd 沙盒选项表
├── static/            # 前端（index.html / app.js / style.css，go:embed 内嵌）
├── templates/         # 游戏模板 JSON（内置+导入都在这里）
└── data/              # 全部状态：config.json（密码哈希、实例、计划任务）+ events.jsonl（事件日志），已 gitignore

/home/games/
├── steamcmd/          # steamcmd 本体
├── instances/<名>/    # 游戏安装目录（含 start.sh、logs/console.log）
└── backups/<名>/      # tar.gz 备份
```
