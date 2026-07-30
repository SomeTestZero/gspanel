# GSPanel · 轻量游戏服务器管理面板

自用的游戏服管理后台。Go 单二进制（无运行时依赖，内存占用 ~11MB），前端页面内嵌，
游戏进程由 systemd 托管（面板重启/崩溃不影响游戏），新游戏通过 JSON 模板扩展。

- 面板源码与二进制：clone 到任意目录皆可（本机在 `/root/gspanel/`），状态跟随二进制所在目录
- 面板服务：`gspanel.service`（开机自启）
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

- 游戏以 `games` 用户（uid 5）运行，实例目录 `/home/games/instances/<名>/`
- 备份在 `/home/games/backups/<名>/`（tar.gz，保留策略按份数）
- 状态都在 `面板目录/data/`：`config.json`（账号/实例/计划任务）+ `events.jsonl`（事件日志），无数据库

## 新服务器安装

一键脚本（幂等，可重复跑）：装 Go、建 `games` 用户、构建、写 systemd unit、开机自启。
clone 到任意目录都行：`BaseDir` 运行时取二进制所在路径（main.go），面板状态 `data/` 和
用户模板 `templates/` 都跟随它；游戏侧路径固定 `/home/games`（实例/备份/steamcmd），与面板位置无关。

```bash
git clone <仓库地址> gspanel && cd gspanel          # 位置随意，脚本以所在目录为准
./deploy.sh
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
cd /root/gspanel && ./deploy.sh        # 本机路径；已安装环境只构建+重启面板，幂等
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
`GET /api/tasks`、`POST /api/templates/import`、`POST /api/settings/password|public-ip`。

## 网络与安全

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

## 测试

E2E（headless Chromium，真实浏览器回归）：

```bash
# 1. 临时注入测试密码（测完恢复）
cp /root/gspanel/data/config.json /tmp/cfg.bak
python3 -c 'import json,hashlib;p="/root/gspanel/data/config.json";d=json.load(open(p));
d["password_salt"]="tmp_test_salt";d["password_hash"]=hashlib.sha256(b"tmp_test_salt:testpass123").hexdigest();
json.dump(d,open(p,"w"))'
systemctl restart gspanel
# 2. 跑测试（Node 22 在 /opt/node22，playwright 在 /root/e2e）
cd /root/e2e && /opt/node22/bin/node panel.test.js
# 3. 恢复
cp /tmp/cfg.bak /root/gspanel/data/config.json && systemctl restart gspanel
```

前端是纯手写 JS（无框架），改完 `node --check static/app.js` 再过 E2E。

## 文件清单

```
/root/gspanel/
├── gspanel            # 编译产物（运行时唯一需要的东西，已 gitignore）
├── deploy.sh          # 幂等一键部署：裸机全装 / 已装只构建+重启面板，saves/ 自动就位
├── push-saves.sh      # 收各实例最新备份到 saves/ 作迁移种子（git 提交留人工）
├── saves/             # 迁移存档种子（<实例名>.tar.gz，每实例最新一份，git 跟踪）
├── *.go               # 后端：main/config/auth/api/instance/systemctl/steamcmd/events/
│                      #   rcon/tasks/scheduler/backup/monitor/configfile/templates/netinfo/util/stream
├── static/            # 前端（index.html / app.js / style.css，go:embed 内嵌）
├── templates/         # 游戏模板 JSON（内置+导入都在这里）
└── data/              # 全部状态：config.json（密码哈希、实例、计划任务）+ events.jsonl（事件日志），已 gitignore

/home/games/
├── steamcmd/          # steamcmd 本体
├── instances/<名>/    # 游戏安装目录（含 start.sh、logs/console.log）
└── backups/<名>/      # tar.gz 备份
```
