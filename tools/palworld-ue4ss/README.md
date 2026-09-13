# Palworld 给物品（原生 Linux，UE4SS + 自定义修复）

> 状态（2026-09-13）：**已打通并在 `palworld-1` 生产实例上启用**。
> UE4SS 在 Palworld 1.0.4 (buildid 25080279) 原生 Linux 服务端上完整初始化，
> Lua mod 可访问 UE 对象；面板控制台页新增「扩展: 在线玩家 / 给物品 / 给经验」按钮。
>
> 已实测：`FindAllOf` / `FindObject` / `GetFullName` / `ForEachUObject`
> （154k 对象）/ `StaticFindObject` 正常；文件队列 `who`/`give`(离线玩家错误路径) 正常。
> **未实测**：对在线玩家真正执行 `AddItem_ServerInternal`（等玩家上线后验证）。

## 1. 调研结论

- 官方无给物品能力（v1.0.4 实测：RCON 未知命令统一回 `Unknown command`；
  REST 只有 `info/players/metrics/announce/kick/ban/unban/save/stop/shutdown/settings/game-data`）。
- Windows/Wine 侧有 PalDefender（`/give`、`/givepal`）等成品；官方 1.0 server-side mod
  框架也仅限 Windows。
- 原生 Linux 走 `XarminaEu/ue4ss-linux`（LD_PRELOAD + Lua mod）。上游预编译版在
  Palworld 1.0.4 上直接崩（issue #3/#8/#10）；本目录是**自行修复并重新编译**的版本。

## 2. 我们修了哪些上游 bug（全部在 `patches/ue4ss-linux-palworld-1.0.4.patch`）

| # | 问题 | 修复 |
|---|---|---|
| 1 | `FName::FName` AOB 扫描在 1.0.4 上给错地址 → `setup_unreal_properties` 空指针段错误 | 新增 patternsleuth 风格的 Linux 解析器：用 `Engine/Renderer/AnimGraphRuntime/Landscape/RenderCore` 五个 UTF-16 字符串 → 找引用 → 找附近 `BA 01 00 00 00 E8` 的 call 目标 → 取交集（唯一命中 `0x7976040`） |
| 2 | `FName::ToString` 手工 AOB 扫描不可靠 | 新增解析器：`SkySphereMesh` 字符串 + `E8 ?? ?? ?? ?? 49 8B 5F 10 48 8D 7C 24 30 BE <imm32>` 模式 |
| 3 | FName 构造函数按“自由函数返回 FName”调用，漏掉 Itanium ABI 的隐式 `this`（rdi），导致把字符串当 this、把 EFindName 当字符串指针 | Linux 下用 `void(*)(FName*, const char16_t*, EFindName)` 重新解释函数指针后调用（`NameTypes.hpp`） |
| 4 | `bit_cast_mfp`（union 实现）第二个 8 字节 `this_adjustment` 未初始化 → 所有虚函数调用 `this += 栈垃圾` | `union u{}` 零初始化 |
| 5 | FMalloc vtable 偏移按 1 个基类槽算，实际 Palworld 1.0.4 有 2 个 → 所有 `FMalloc::Malloc/Free/QuantizeSize` 调错函数 | `fexec_size = 2`（`FMalloc::Malloc` 从 0x10 → 0x18，`QuantizeSize` 0x38 → 0x40） |
| 6 | `GMalloc` 启发式“第一个 ptr→ptr 都指向可写段”命中的是假地址 | 改用 vtable 锚点：`dlsym("_ZTV<FMallocXxx>") + 0x10` 作为 vptr 集合，在 `.bss` 反查指向该 vptr 的全局（实测命中 `FMallocBinned2`） |
| 7 | `GUObjectArray` 启发式解析到栈地址（把 int32 计数器数组当成对象数组），且 `max_elements` 上限 1000 万把真实值 3361 万过滤掉 | 强化校验：候选必须在主程序可写段（.bss），遍历 chunk 0 前 8 项校验 vtable 在主程序只读段、`InternalIndex == 下标`；上限抬到 1<<30、MaxChunks 65536；跳过只会命中假地址的堆/栈扫描 |
| 8 | `ForEachUObject_Chunked` 对每个 chunk 都遍历 65536 项，读到最后 chunk 未初始化区域的垃圾指针 → `FindAllOf` 崩溃 | 改为按全局索引只遍历到 `NumElements`，并加指针范围防御 |
| 9 | 上游预编译是 Ubuntu 24.04 (glibc 2.38) 构建，22.04 (glibc 2.35) 加载失败 | 本机源码编译，天然兼容；`shim/` 是给预编译版用的垫片（已不需要，保留备用） |

## 3. 部署 / 使用

### 方式 A：面板管理（推荐，当前生产用的就是这个）

1. 「设置/环境 → Palworld 扩展命令」：上传/按服务器路径导入/URL 下载 `libUE4SS.so`
   （存 `data/ue4ss/libUE4SS.so`，不入 git）；
2. 实例「设置 → 扩展命令」：点「安装」下发资产并注入 `start.sh`，重启实例生效；
   「卸载」会恢复原始 `start.sh` 并删除注入文件。

框架 `.so` 用下面的脚本编译后，用「按路径导入」填入 `/home/games/ue4ss-build/libUE4SS.so` 即可。
mod/布局表由面板从 `assets/palworld-ue4ss/`（embed 进二进制）下发，改 mod 后重新点「安装」。

### 方式 B：命令行脚本（无面板/离线部署）

```bash
# 一键编译（约 30-60 分钟；需要 gcc-13 + rustup，见脚本注释）
tools/palworld-ue4ss/build-ue4ss.sh /tmp/ue4ss-src
# 安装到实例（会备份原 start.sh 为 start.sh.pre-ue4ss，资产从 ../../assets/palworld-ue4ss 拷贝）
sudo tools/palworld-ue4ss/install-to-instance.sh /home/games/instances/palworld-1 \
     /tmp/ue4ss-src/build/Game__Dev__Linux64/lib/libUE4SS.so
sudo systemctl restart gspanel-palworld-1
# 卸载（恢复 start.sh）
sudo tools/palworld-ue4ss/uninstall-from-instance.sh /home/games/instances/palworld-1
```

### 当前生产实例的实际布局（已装好）

```
/home/games/instances/palworld-1/
├── start.sh                       # 已改成 env LD_PRELOAD=... 直接 exec 游戏二进制
├── start.sh.pre-ue4ss             # 原始启动脚本（卸载时恢复）
├── MemberVariableLayout.ini / VTableLayout.ini
└── Pal/Binaries/Linux/
    ├── libUE4SS.so                # 我们编译的版本（debug 140MB；strip 后 22MB 在 /home/games/ue4ss-build/）
    ├── MemberVariableLayout.ini / VTableLayout.ini / UE4SS-settings.ini
    ├── Mods/mods.txt              # gspanel : 1
    ├── Mods/gspanel/scripts/main.lua
    └── gspanel-mod/cmd.txt|res.txt  # 文件队列（游戏进程 cwd 在这里）
```

编译产物备份：`/home/games/ue4ss-build/libUE4SS.so`（strip，22MB）、
`libUE4SS.so.debug`（140MB，查问题用）。

## 4. 控制通道：文件队列

`RegisterConsoleCommandGlobalHandler` 在本游戏不可用（日志：
`Failed to add hook ... ProcessConsoleExec`），所以走文件队列：

- 面板/脚本写 `<实例>/Pal/Binaries/Linux/gspanel-mod/cmd.txt`，格式（每行一项）：
  ```
  give
  some_test0
  Wood
  100
  ```
  或 `who`、`giveexp` + 参数。
- mod 每 500ms 轮询，处理后删 `cmd.txt` 并写 `res.txt`：
  ```
  OK|FAIL
  消息
  ```
- 面板 API：`POST /api/instances/{name}/mod-command`，body `{"verb":"give","args":["some_test0","Wood","100"]}`，
  返回 `{ok, message}`；前端在控制台页渲染「扩展」按钮（`has_give_mod` 为 true 时显示）。
- 给物品按候选接口依次尝试（pcall 包裹）：
  `RequestAddItem` → `RequestAddItem_ToServer` → `RequestAddItem_ForDebug` →
  `AddItem_ServerInternal(itemId, count, false, 0.0, true)`（1.0 SDK 确认存在）。
  命中哪个接口写在返回消息里。

## 5. 注意事项 / 坑

- **LD_PRELOAD 绝不能进 shell**：`start.sh` 里必须 `exec env LD_PRELOAD=... 游戏二进制`，
  否则 bash/PalServer.sh 会加载 UE4SS 并段错误（构造函数假定自己在 UE 进程里）。
- **mod 的 Lua 语法/加载期错误会直接 abort 整个游戏进程**（UE4SS 把 Lua 错误抛成 C++ 异常，
  而 libsteam_api 覆盖了 `__gxx_personality_v0`，unwind 失败 → SIGABRT，systemd 重启循环）。
  改完 mod 必须先 `luac5.4 -p main.lua` 校验再部署（`install-to-instance.sh` 已内置校验）。
  已踩过的坑：Lua 5.4 没有全局 `unpack`（用 `table.unpack`，mod 顶部已兼容）；
  嵌套匿名函数里不能用 `...`（先在 vararg 函数体内 `local args = {...}`）。
- `start.sh` 在面板「安装/更新」时会被 `writeStartScript` 重写并**丢掉 LD_PRELOAD**，
  更新游戏后需要重新执行 `install-to-instance.sh`（或以后把该功能做进面板）。
- 游戏进程 cwd = `Pal/Binaries/Linux`，mod 的相对路径都相对它。
- 上游更新 UE4SS 后，`FMalloc fexec_size`/FName 解析器等修复需要重新适配
  （见补丁里的注释与 `#10` issue 的验证方法）。
- 本机内存 3.6G，Palworld 加载后 ~2.3G；UE4SS 额外占用很小。

## 6. 目录内容

```
assets/palworld-ue4ss/               # 运行时资产（embed 进面板二进制，安装时下发）
├── layouts/{MemberVariableLayout,VTableLayout}.ini
├── mod/scripts/main.lua             # 给物品/给经验/在线玩家 + cmd.txt/res.txt 文件队列
├── UE4SS-settings.ini / mods.txt

tools/palworld-ue4ss/                # 框架侧物料（不参与面板构建）
├── README.md                        # 本文件
├── patches/ue4ss-linux-palworld-1.0.4.patch   # 全部源码修复（指向 fork）
├── build-ue4ss.sh                   # 从 fork 源码编译带修复的 libUE4SS.so
├── install-to-instance.sh / uninstall-from-instance.sh
├── test-ue4ss.sh                    # 硬链接副本安全试跑（新构建验证用）
├── patch-ue4ss-glibc.py / shim/     # （旧）预编译版 glibc 垫片，源码编译用不到
└── mod/template-console-buttons.json
```
