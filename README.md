# GSPanel · 轻量游戏服务器管理面板

自用的游戏服管理后台。Go 单二进制（无运行时依赖，内存占用 ~11MB），前端页面内嵌，
游戏进程由 systemd 托管（面板重启/崩溃不影响游戏），新游戏通过 JSON 模板扩展。

- 面板源码与二进制：`/root/gspanel/`
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
                  ├─ 计划任务调度（定时重启/备份/更新）
                  └─ 监控（/proc + systemctl show，无外部 agent）
```

- 游戏以 `games` 用户（uid 5）运行，实例目录 `/home/games/instances/<名>/`
- 备份在 `/home/games/backups/<名>/`（tar.gz，保留策略按份数）
- 所有状态在一个文件里：`/root/gspanel/data/config.json`（无数据库）

## 日常维护

### 重新构建部署（改代码后）

```bash
cd /root/gspanel && go build -o gspanel . && systemctl restart gspanel
```

面板重启不影响正在运行的游戏。需要 Go 1.22+（`apt install golang-go`）。

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

模板 = 一个 JSON，放 `/root/gspanel/templates/` 重启面板生效，或在「新建实例」页从 URL 导入。
内置 13 个：palworld / valheim / cs2 / 7dtd / rust / satisfactory / zomboid / enshrouded /
gmod / tf2 / ark-se / terraria / corekeeper。

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
  "ports": [
    { "key": "game", "default": 8211, "proto": "udp", "desc": "游戏端口", "public": true }
  ],
  "configs": [{                   // 可选；format: option-settings | kv | raw
    "path": "cfg/server.ini", "format": "kv",
    "seed_from": "cfg/default.ini",       // 安装后从此复制生成
    "schema": [{ "key": "MaxPlayers", "label": "人数上限", "type": "int" }]
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
3. **Palworld RCON `Info` 无响应**是游戏自身 bug（其余命令正常）。
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
├── gspanel            # 编译产物（运行时唯一需要的东西）
├── *.go               # 后端：main/config/auth/api/instance/systemctl/steamcmd/
│                      #   rcon/tasks/scheduler/backup/monitor/configfile/templates/netinfo/util/stream
├── static/            # 前端（index.html / app.js / style.css，go:embed 内嵌）
├── templates/         # 游戏模板 JSON（内置+导入都在这里）
└── data/config.json   # 全部状态（密码哈希、实例、计划任务）

/home/games/
├── steamcmd/          # steamcmd 本体
├── instances/<名>/    # 游戏安装目录（含 start.sh、logs/console.log）
└── backups/<名>/      # tar.gz 备份
```
