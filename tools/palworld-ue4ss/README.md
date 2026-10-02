# Palworld 给物品（原生 Linux，UE4SS + 自定义修复）

> 状态（2026-09-14，mod `2026-09-14.2`）：**修复了玩家名为空和给物品调用链**。
> 旧版“框架初始化成功”不代表 UFunction 真的执行；Linux 布局问题见下方第 2.1 节。
> 玩家进服的 C++ 异常运行时崩溃已在前一版本修复（见 #10/#11/#12 与 shim-eh/）。
> UE4SS 在 Palworld 1.0.4 (buildid 25080279) 原生 Linux 服务端上完整初始化，
> Lua mod 可访问 UE 对象；面板控制台页「扩展: 在线玩家 / 给物品 / 给经验」按钮，
> 其中「给物品」是**物品选择对话框**（在线玩家下拉 + 全量中文名搜索 + 数量快捷档）。
>
> 已实测：`FindAllOf` / `FindObject` / `GetFullName` / `ForEachUObject`
> （154k 对象）/ `StaticFindObject` / `UDataTable:GetRowNames`+`FindRow`
> （导出 2466 个物品 ID）/ 文件队列 `who`/`whojson`/`hello`/`items` 正常；
> **绑定层 C++ 异常回归测试**（mod `probe throw` / `probe throwasync` 故意触发
> 裸 `throw std::runtime_error`）：修复前必然 SIGABRT 杀进程，修复后正确退化为
> 普通 Lua 错误（pcall 捕获 + 完整 traceback 写 res.txt），游戏线程与 async 线程
> 两条路径都验证通过。
> **验证**：Go test/vet/build、Lua 106 项 assertions（28 命令）、隔离游戏
> `probe selftest`（ProcessEvent/FString/FName/UEnum）和离线目标拒绝通过。
> .2 已通过结构体与玩家 CDO 回归，并在生产 `palworld-1` 验证：
> 玩家名/UID 正确，**通过面板 API 发放 Wood ×1，背包 0→1，返回枚举 0、ok=true**，游戏没有重启；随后存档成功。
> 满背包/多玩家/特殊物品分支尚无真人实测，模拟回归已覆盖相关失败处理；详细状态见 `AGENTS.md`。
> Lua `pcall` 不能拦截原生 SEGV；严禁在生产对未知签名/参数类型盲试。
>
> ⚠️ 本 build 的 UE4SS **FText 读取仍不可用**（详见第 5 节），物品中文名
> 改为面板侧从 pak 离线提取（`extract-items-zh.py`）。

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
| 10 | **（崩溃根因①）** libsteam_api.so 静态链了旧 libstdc++ 并导出**无版本**的 `__gxx_personality_v0`，抢先于系统 libstdc++ 被动态链接器选中；它与系统 libgcc_s unwinder 不兼容，安装 landing pad 时直接 abort → **任何 C++ 异常 unwind 即 SIGABRT**（UE4SS 的 TRY/catch 形同虚设）。玩家进服崩溃即此：玩家相关代码路径触发绑定层错误 → 异常 → 死 | 双保险：a) `shim-eh/`（libgxxfix.so）：LD_PRELOAD 排最前，把 `__gxx_personality_v0` 转发回系统 libstdc++ 真身；b) 见 #11 静态链接，让本库人格引用彻底内化 |
| 11 | **（崩溃根因②）** PalServer 主程序导出整套静态 libstdc++ 符号（`__cxa_throw`/`__cxa_begin_catch`/`typeinfo`…），libUE4SS 的异常创建/捕获被劫持到游戏那份运行时，与系统 unwinder/personality 混用 → unwind 途中 SEGV（修了 personality 后暴露的第二层） | 链接 libUE4SS 时 `-static-libstdc++ -Wl,--exclude-libs,ALL`：本库 C++ 异常运行时完全自洽（personality/__cxa_*/typeinfo 全用自己的副本，且不再导出干扰他人），`UE4SS/CMakeLists.txt` |
| 12 | LuaMadeSimple 在「无 Lua error handler」路径裸 `throw std::runtime_error`（mod 加载期/注册期错误直接炸进程，连错误文本都看不到）；`ExecuteInGameThread` 默认 EngineTick 方法在早期 build 不可用时不回退 | LuaMadeSimple 改为打 stderr 日志不抛错；LuaMod 增加游戏线程 id 兜底初始化 + EngineTick/ProcessEvent 钩子自动回退（`LuaMadeSimple.cpp` / `LuaMod.cpp`） |

### 2.1 Linux 运行时布局修复（2026-09-14；assets 布局表）

通过生产 ELF 的 UObject vtable 与**只读** `/proc/<pid>/mem` 反射数据核对：

| 错误 | 实际值 / 修复 |
|---|---|
| UObjectBase 的析构函数只留一个槽 | Itanium ABI 有 complete/deleting 两槽；在 `[UObjectBase]` 加占位，子类不重复加。`ProcessEvent` 从 0x260（`ret`@0x441dff0）改为 0x268（0x7b802a0） |
| FProperty.ArrayDim/ElementSize=0x38/0x3C | Linux 复用 FField 尾 padding，实际 0x34/0x38；旧版 enum/array 长度可能为 0 或垃圾 |
| UEnum.Names=0x40 | 实际 0x48；`EPalItemOperationResult` 有 36 条，0=Success，1=SuccessNoOperation，16=FailedNotEnoughSlotSpace |

`probe selftest` 用 KismetStringLibrary 的原生函数拼接固定字符串、FName 转换、enum 名查询，
不接触背包/存档；只有自检通过才接受玩家列表/写命令。不要只检查 `.so` 已加载。
玩家名字读 `PlayerNamePrivate`（不是不存在的 `PlayerName`）；`GetPlayerName()` 和
`GetInventoryData()` 之前由于空 ProcessEvent 而返回空值。

### 2.2 结构体绑定修复（必须重编译 .so，`patches/ue4ss-linux-palworld-properties.patch`）

在线验证发现 `push_structproperty` 里的 `CastField<FStructProperty>` 返回空指针，
后续 `GetStruct()` 解引用 `nullptr+0x78` SEGV；在**隔离空世界的 PalPlayerState CDO** 读取
`PlayerUId` 也可稳定复现，不是玩家半初始化或 GUID 格式的问题。
根因是 Linux 初始化捷径没有填充 `FStructProperty::StaticClassStorage`，按静态类地址进行
类型转换失败，但真实 property 的 cast flags=0x108001（含 FStructProperty 位）。

修复：用运行时 `HasAllCastFlags(CASTCLASS_FStructProperty)` 检查后 static_cast，并检查
property/script_struct 均非空；FName setter 拒绝非 userdata，避免把 Lua 字符串强转成 FName。
新框架暴露 `GSPanelPropertyBindingsVersion=1`，mod .2 在碰结构体之前检查该能力；
**仅升级 Lua/布局而没升级 .so 会明确拒绝操作，不再触发崩溃**。
`probe selftest` 追加 CDO GUID 四字段读取；`probe playercheck` 检查 CDO 的 flags/bool/owner/UID/name。
**不可在背包 CDO 上查询物品类型/数量**：默认背包没有 GameInstance，游戏 PalUtility.cpp 会 fatal。
`probe inventorycheck <UID>` 仅针对真实在线玩家及归属匹配的背包做 Wood 类型/数量只读检查。
`build-ue4ss.sh` 按顺序应用两份补丁，源码树有修改时拒绝覆盖（增量开发用 `ninja -C build -j1 UE4SS`）。

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
├── start.sh                       # 已改成 env LD_PRELOAD=libgxxfix.so:libUE4SS.so 直接 exec 游戏二进制
├── start.sh.pre-ue4ss             # 原始启动脚本（卸载时恢复）
├── MemberVariableLayout.ini / VTableLayout.ini
└── Pal/Binaries/Linux/
    ├── libgxxfix.so               # EH 垫片（__gxx_personality_v0 拦截，shim-eh/ 构建）
    ├── libUE4SS.so                # 我们编译的版本（静态 libstdc++，debug 140MB；strip 后 19MB）
    ├── MemberVariableLayout.ini / VTableLayout.ini / UE4SS-settings.ini
    ├── Mods/mods.txt              # gspanel : 1
    ├── Mods/gspanel/scripts/main.lua
    └── gspanel-mod/cmd.txt|res.txt  # 文件队列（游戏进程 cwd 在这里）
```

编译产物备份：`/home/games/ue4ss-build/libUE4SS.so`（strip，已同步 .2 properties 修复）、
`libUE4SS-properties.so.debug`（匹配的 debug，查问题用；旧 `libUE4SS.so.debug` 不匹配新框架）。
`install-to-instance.sh` 的默认 .so 来源就是这里，重编译后须同步面板库存与此路径。

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
  或 `who`、`whojson`（在线玩家 JSON）、`hello`（mod 版本握手）、`items`（导出物品 ID 列表
  `gspanel-mod/items.json`）、`giveexp` + 参数。
- 面板使用 v2：请求首行加 `gsp2:<随机请求ID>`；mod 响应首行回同一个 ID，
  后面为 `OK|FAIL` 与消息。只有 mod-alive 的版本+PID 与当前进程匹配才发命令，
  旧进程需安装并重启；磁盘脚本版本不等于运行版本。mod 对手工无 ID 请求仍兼容。
- mod 每 500ms 轮询，游戏对象操作在游戏线程执行；消费后删 `cmd.txt`，
  `res.txt.tmp` 写完 rename 到 `res.txt`（原子响应），面板忽略迟到/不匹配响应。
  超时**不自动重试**，需先核对背包。
- 手工无 ID 响应示例：
  ```
  OK|FAIL
  消息
  ```
- 面板 API：`POST /api/instances/{name}/mod-command`，body `{"verb":"give","args":["some_test0","Wood","100"]}`，
  返回 `{ok, message}`；前端在控制台页渲染「扩展」按钮（`has_give_mod` 为 true 时显示）。
- 给物品只调用一次确认的同步服务端接口：
  `inv:AddItem_ServerInternal(FName(itemID), count, false, 0.0, true)`。
  **UFunction 是可调用 userdata，不是 Lua function；FName 参数必须显式包装，传字符串会 SEGV**。
  只有返回枚举 0 且 `CountItemNum(FName(id))` 前后增加指定数量才报成功；
  拒绝/部分到账/核验错误都明确返回，不做候选试错，不把 pcall 成功当作发放成功。
- 在线玩家每条命令现场查，只收有 NetConnection、非 inactive/bot/CDO 的 PlayerState；
  稳定身份是 `PlayerUId`（FGuid 四段 32 位 hex），手动可输入完整名字（同名拒绝）。
  **不支持 SteamID、不使用“玩家#序号”、不按背包数组顺序选目标、不缓存 UObject**。
  背包 `OwnerPlayerUId` 必须与目标一致。
- 面板给物品默认 1，范围 1～10000 整数；提交前确认、防 Enter/双击重复请求，
  API 校验参数/物品库，结果显示真实背包增量并记录事件。

### 回归验证

```bash
PATH=/usr/local/go/bin:$PATH go test ./...
PATH=/usr/local/go/bin:$PATH go vet ./...
lua5.4 tools/palworld-ue4ss/test-mod.lua
node --check static/app.js
# 隔离空世界：独立 transient unit、端口、网络仅 localhost、内存/CPU 上限
sudo NATIVE_CHECK=1 RUN_SECS=240 bash tools/palworld-ue4ss/test-ue4ss.sh \
  /home/games/instances/palworld-1 /home/games/ue4ss-build/libUE4SS.so 18500
```

测试副本在 `/home/games/gsp-ue4ss-test.*`，可变目录完整断链，不共享生产存档/日志/mod。
**旧脚本的 `cp -al` 后 cp 覆盖文件会写坏生产 inode；运行中 .so 不能原地截断**。
现在安装（面板/CLI）均写新 inode 后原子替换，测试 unit 退出必停；副本保留方便排查。

### 物品库与中文名（面板侧离线提取）

- 1.0.4 的 `DT_ItemDataTable` 行结构体 `PalStaticItemDataStruct` 是精简表：
  **没有 Name/TypeA/TypeB/Rarity/MaxStackCount**（访问不存在的属性 → throw → abort）；
  物品显示名在 `L10N/<lang>/Pal/DataTable/Text/DT_ItemNameText_Common`
  （行结构 `PalLocalizedTextData`，键 `ITEM_NAME_<物品ID>_TextData`，值在 `TextData` FText 里）。
- 面板内置基线 `assets/palworld-items/palworld-zh.json`（2466 项 ID+中文名）由
  `tools/palworld-ue4ss/extract-items-zh.py` 从 `Pal-LinuxServer.pak` 离线提取：
  ```bash
  pip install repak
  # 先让 mod 导出 ID：面板「给物品」对话框点「从游戏刷新物品库」，或手工写 cmd.txt 队列命令 items
  python3 tools/palworld-ue4ss/extract-items-zh.py \
      /home/games/instances/palworld-1/Pal/Content/Paks/Pal-LinuxServer.pak \
      /home/games/instances/palworld-1/Pal/Binaries/Linux/gspanel-mod/items.json \
      assets/palworld-items/palworld-zh.json
  go build   # 重新 embed 进面板二进制
  ```
  名称回退链：zh-Hans → zh-Hant → en → 基础表 → 物品 ID（游戏未翻译项写的是
  `zh-hans text` 这类占位串，会被过滤；约 556 个内部/未使用项最终显示 ID）。
- 运行时刷新：`POST /items/refresh` 让 mod 导出 ID，面板按 ID 把内置中文名合并进
  `data/items/palworld.json`；**游戏新增物品的中文名需要重跑离线脚本 + 重新编译面板**。

## 5. 注意事项 / 坑

- **LD_PRELOAD 绝不能进 shell**：`start.sh` 里必须 `exec env LD_PRELOAD=... 游戏二进制`，
  否则 bash/PalServer.sh 会加载 UE4SS 并段错误（构造函数假定自己在 UE 进程里）。
- **LD_PRELOAD 顺序**：`libgxxfix.so` 必须在 `libUE4SS.so` 之前（垫片要让
  personality 绑定抢在 libsteam_api 之前；两者都是 preload，按列出顺序排）。
  装好后 console.log 应有 `[gxxfix] __gxx_personality_v0 interposed -> libstdc++`。
- **FText 不能读（本 build）**：`FText:ToString()` 走 `UKismetTextLibrary:Conv_TextToString`
  的 native macro，里面按 path `StaticFindObject("/Script/Engine.KismetTextLibrary:Conv_TextToString")`
  在本 build 返回 nil 并抛错。有 EH 修复后这只是 Lua 错误（不再杀进程），但依然取不到文本。
  同一个原因，`FText::StaticSize_Private` 初始值是 -1 且没人设置，复制 FText 也会报错。
  想恢复 FText 能力需要改 libUE4SS（修 path 式 UFunction 查找 / 给 StaticSize 赋值）后重编译。
- **不要访问不确定存在的属性**：1.0.4 的物品行结构体是精简表（无 Name/TypeA/...），
  访问不存在属性会抛 Lua 错误（EH 修复后非致命）；mod 里只读文档确认存在的字段。
- **回调式 API 有栈 bug**：`UDataTable:ForEachRow` / `UStruct:ForEachFunction` 会把
  回调位取错（`lua_pushvalue(1)` 在 push 了额外值之后失效）→ Lua 报错（EH 修复后非致命）；
  `UStruct:ForEachProperty` 在简单结构体上能用，但仍属高风险，尽量用
  `GetRowNames()` + `FindRow()`。
- **mod 的 Lua 语法/加载期错误**：EH 修复后不再杀进程（LuaMadeSimple 补丁会打 stderr
  日志并跳过），但 mod 加载失败 = 功能静默缺失；改完 mod 仍必须先 `luac5.4 -p main.lua`
  校验再部署（`install-to-instance.sh` 已内置校验）。
  已踩过的坑：Lua 5.4 没有全局 `unpack`（用 `table.unpack`，mod 顶部已兼容）；
  嵌套匿名函数里不能用 `...`（先在 vararg 函数体内 `local args = {...}`）。
- **验证垫片/修复在役**：面板控制台发 `probe throw`（游戏线程）/ `probe throwasync`
  （async 线程），应返回 `survived binding throw: pcall ok=false err=No overload found
  for function 'LoopAsync'...` 且服务器不崩；若服务器直接消失 = 垫片没装上。
- `writeStartScript` 已识别 `Instance.UE4SS`，面板重写不会丢 LD_PRELOAD；框架/布局随游戏更新不兼容时仍需重新适配。
- 游戏进程 cwd = `Pal/Binaries/Linux`，mod 的相对路径都相对它。
- 上游更新 UE4SS 后，`FMalloc fexec_size`/FName 解析器等修复需要重新适配
  （见补丁里的注释与 `#10` issue 的验证方法）。
- 本机内存 3.6G，Palworld 加载后 ~2.3G；UE4SS 额外占用很小。

## 6. 目录内容

```
assets/palworld-ue4ss/               # 运行时资产（embed 进面板二进制，安装时下发）
├── libgxxfix.so                     # EH 垫片预编译产物（源码/构建在 tools/.../shim-eh/）
├── layouts/{MemberVariableLayout,VTableLayout}.ini
├── mod/scripts/main.lua             # 给物品/给经验/在线玩家/物品表导出 + cmd.txt/res.txt 文件队列
├── UE4SS-settings.ini / mods.txt

assets/palworld-items/palworld-zh.json  # 「给物品」内置物品库（2466 项 ID+中文名，embed 进面板）

tools/palworld-ue4ss/                # 框架侧物料（不参与面板构建）
├── README.md                        # 本文件
├── patches/ue4ss-linux-palworld-1.0.4.patch   # 基础源码修复（12 项，打在上游 linux-native 分支）
├── patches/ue4ss-linux-palworld-properties.patch # 追加：结构体 cast 防空 + FName 参数防护 + 能力标记
├── build-ue4ss.sh                   # 从 fork 源码编译带修复的 libUE4SS.so（含静态 libstdc++ 链接）
├── shim-eh/                         # __gxx_personality_v0 拦截垫片（gxxfix.c + build.sh → libgxxfix.so）
├── install-to-instance.sh / uninstall-from-instance.sh
├── extract-items-zh.py              # 从 pak 离线提取物品中文名，生成 assets/palworld-items/
├── test-ue4ss.sh                    # 只读大文件硬链接、可变文件独立的 transient unit 试跑
├── test-mod.lua                    # 纯 Lua 回归：身份/断线/同名/单次发放/enum/增量/协议
├── test-native.py                  # 隔离目录限定的原生 CDO/UID/基础调用/离线拒绝回归
├── patch-ue4ss-glibc.py / shim/     # （旧）预编译版 glibc 垫片，源码编译用不到
└── mod/template-console-buttons.json
```
