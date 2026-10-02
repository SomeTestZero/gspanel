-- GSPanel 扩展命令 mod（Palworld 原生 Linux，基于 ue4ss-linux / LD_PRELOAD）
-- 版本：2026-09-14.2
--   功能：给物品 / 给经验 / 在线玩家索引 / 物品表导出
--
-- ★ 2026-09-13.11 崩溃根因修复 ★
--   玩家进服一会儿就 SIGABRT 的根因不是 Lua 代码本身，而是**进程级 C++ 异常
--   处理被劫持**：游戏的 libsteam_api.so 静态链了旧 libstdc++ 并导出一个无版本的
--   __gxx_personality_v0，抢在系统 libstdc++ 前面被动态链接器选中。UE4SS 绑定层
--   一旦抛 C++ 异常（本来会被 TRY/catch 正常兜住），unwind 时装 landing pad 就
--   abort → 全进程死亡。修复 = 随 libUE4SS 一起 LD_PRELOAD 的 libgxxfix.so
--   （tools/palworld-ue4ss/shim-eh/），把 personality 转发回系统 libstdc++ 真身。
--   装好垫片后，绑定层错误退化成普通 Lua 错误（pcall 可捕获、res.txt 可见），
--   服务器不再死。诊断：probe throw / probe throwasync 可故意触发一次绑定层
--   C++ throw，用于验证垫片在役。
--
-- ★ 2026-09-14.1：Linux 布局 + 实际调用链修复 ★
--   UObjectBase 双析构槽：ProcessEvent 应在 0x268（旧 0x260 是空 ret）。
--   FProperty ArrayDim/ElementSize 应在 0x34/0x38（Linux 复用基类尾部 padding）。
--   UFunction 是可调用 userdata，FName 参数必须显式 FName(id)，不能传字符串。
--   玩家以 PlayerUId 为身份，每条命令现场刷新，只接受有连接的非 inactive PlayerState；
--   不缓存 UObject、不用 玩家#序号/背包遍历索引、不对同名玩家猜测。
--   AddItem_ServerInternal 只执行一次，检查 EPalItemOperationResult + 背包前后数量。
--   布局未更新时 selftest 失败，所有玩家/写操作 fail closed。
-- ★ 2026-09-14.2：框架 push_structproperty 不能依赖未初始化的 FStructProperty::StaticClass；
--   改用运行时 cast flags 验证并防空。必须有 GSPanelPropertyBindingsVersion=1 才读 UID。
--   详见 patches/ue4ss-linux-palworld-properties.patch；selftest 增加 CDO GUID 字段回归。
--
-- 说明：EngineTick 钩子在本 build 可用（ExecuteInGameThread 走它）；BeginPlay/
-- 控制台命令等钩子不可用，所以命令入口走文件队列轮询，游戏对象操作投递到游戏线程。
--
-- 物品中文名不在这里取：本 build 的 UE4SS FText 转换有 bug（FText::ToString →
-- KismetTextLibrary native macro 找不到 UFunction），且 1.0.4 物品行结构体没有
-- Name 等字段；中文名由面板侧从游戏 pak 的 L10N/DT_ItemNameText 离线提取并内置
-- （见 palworlditems.go）。有垫片后 FText 读取只会报 Lua 错误不再杀进程，但仍取不到名。
--
-- 两种驱动方式：
--   1) 控制台命令（RCON 或游戏内控制台）：give/giveexp/gspwho
--   2) 文件队列（gspanel-mod/cmd.txt），面板用：give/giveexp/who/whojson/hello/items/probe
local LOG = "[gspanel]"
local MOD_VERSION = "2026-09-14.2"
local function log(fmt, ...)
  local ok, s = pcall(string.format, fmt, ...)
  print(LOG .. " " .. (ok and s or tostring(fmt)) .. "\n")
end

-- [DEBUG] 步骤日志：每次 write+close 立即落盘（print 走 stdout 有缓冲，崩溃时可能丢）
local STEP_FILE = "gspanel-mod/steps.log"
local function step(msg)
  local f = io.open(STEP_FILE, "a")
  if f then
    f:write(os.date("%H:%M:%S"), " ", tostring(msg), "\n")
    f:close()
  end
end
step("=== mod loading " .. MOD_VERSION)

-- ============ 游戏线程执行 ============
-- 本 build 的 UE4SS LoopAsync 回调跑在 async 线程；async 线程做 Lua 绑定调用会和
-- 游戏线程（玩家进服时 UE4SS 在注册类型/回调）争抢 LuaMadeSimple 的全局表 →
-- 表被写坏 → 抛异常 →（libsteam_api 的 __gxx_personality_v0）→ SIGABRT。
-- 修好 UEngine::Tick 钩子后，所有触碰游戏对象（以及 Lua 绑定）的代码都投递到游戏线程执行。
local GT_METHOD = nil
local GT_NAME = "none"
if type(EGameThreadMethod) == "table" then
  if EngineTickAvailable then
    GT_METHOD = EGameThreadMethod.EngineTick
    GT_NAME = "EngineTick"
  elseif ProcessEventAvailable then
    GT_METHOD = EGameThreadMethod.ProcessEvent
    GT_NAME = "ProcessEvent"
  end
end
step("gt method=" .. GT_NAME .. " EngineTick=" .. tostring(EngineTickAvailable) .. " ProcessEvent=" .. tostring(ProcessEventAvailable))

local function runGT(name, fn, onError)
  local ok, err = pcall(function()
    local function body()
      local ok2, err2 = pcall(fn)
      if not ok2 then
        step(name .. ": gt-error " .. tostring(err2))
        log("[gt] %s 出错: %s", name, tostring(err2))
        if onError then onError(tostring(err2)) end
      end
    end
    if GT_METHOD ~= nil then
      ExecuteInGameThread(body, GT_METHOD)
    else
      ExecuteInGameThread(body)
    end
  end)
  if not ok then
    step(name .. ": dispatch-failed " .. tostring(err))
    log("[gt] 投递 %s 失败: %s", name, tostring(err))
    if onError then onError(tostring(err)) end
  end
end

-- ============ 框架自检 / 在线玩家（只在命令的游戏线程内读取） ============
local function valid(obj)
  if obj == nil then return false end
  local ok, v = pcall(function() return obj:IsValid() end)
  return ok and v == true
end

local function toLuaString(v)
  if type(v) == "string" then return v end
  if v == nil then return nil end
  local ok, s = pcall(function() return v:ToString() end)
  if ok and type(s) == "string" then return s end
  return nil
end

local function guidString(g)
  if g == nil then return nil end
  local ok, s = pcall(function()
    local parts = {}
    for _, k in ipairs({ "A", "B", "C", "D" }) do
      local v = g[k]
      if type(v) ~= "number" or v % 1 ~= 0 then error("无效 GUID 分量") end
      parts[#parts+1] = string.format("%08X", v & 0xffffffff)
    end
    return table.concat(parts)
  end)
  if ok and s ~= string.rep("0", 32) then return s end
  return nil
end

-- 无玩家也能校验真正的 ProcessEvent 是否执行。旧布局只会返回空 FString。
local function selftest()
  if GSPanelPropertyBindingsVersion ~= 1 then
    error("UE4SS 框架缺少结构体绑定修复；请先更新 libUE4SS.so 再重启（仅更新 Lua/布局不够）")
  end
  local lib = StaticFindObject("/Script/Engine.Default__KismetStringLibrary")
  if not valid(lib) then error("UE4SS 自检失败：找不到 KismetStringLibrary；请等待游戏加载") end
  local result = toLuaString(lib:Concat_StrStr("gspanel-", "process-event-ok"))
  if result ~= "gspanel-process-event-ok" then
    error("UE4SS 自检失败：ProcessEvent 没有正确执行。请更新扩展命令（含 Linux 布局表）并重启游戏")
  end
  if toLuaString(lib:Conv_NameToString(FName("Wood"))) ~= "Wood" then
    error("UE4SS 自检失败：FName 参数转换异常")
  end
  local enum = StaticFindObject("/Script/Pal.EPalItemOperationResult")
  if not valid(enum) or toLuaString(enum:GetNameByValue(0)) ~= "EPalItemOperationResult::Success" then
    error("UE4SS 自检失败：UEnum 布局不兼容")
  end
  local ps = StaticFindObject("/Script/Pal.Default__PalPlayerState")
  if not valid(ps) then error("PlayerState CDO 尚未加载") end
  -- CDO 的 UID 允许全零，但四个字段必须可读；覆盖过去空服自检遗漏的崩溃路径。
  local guid = ps.PlayerUId
  for _, key in ipairs({"A", "B", "C", "D"}) do
    if type(guid[key]) ~= "number" then error("PlayerUId 结构体绑定自检失败") end
  end
  return true, "ProcessEvent/FString/FName/UEnum/PlayerUId 自检通过"
end

local function refreshPlayers()
  selftest()
  local list = FindAllOf("PalPlayerState") or {}
  local out = {}
  for _, ps in ipairs(list) do
    if valid(ps) and not ps:HasAnyFlags(0x10) then -- RF_ClassDefaultObject
      local ok, info = pcall(function()
        if ps.bIsInactive or ps.bIsABot then return nil end
        local pc = ps.Owner
        if not valid(pc) or not valid(pc.NetConnection) then return nil end
        local uid = guidString(ps.PlayerUId)
        if not uid then return nil end -- 登录未完成；不创建不稳定的序号标识
        local name = toLuaString(ps.PlayerNamePrivate)
        if not name or name == "" then name = toLuaString(ps:GetPlayerName()) end
        if not name or name == "" then name = "未命名玩家 · " .. uid:sub(1,8) end
        return { obj = ps, name = name, uid = uid }
      end)
      if not ok then error("读取在线玩家失败（拒绝使用不完整列表）：" .. tostring(info)) end
      if info then out[#out+1] = info end
    end
  end
  table.sort(out, function(a,b) return a.uid < b.uid end)
  return out
end

local function findPlayer(who)
  if type(who) ~= "string" or who == "" then return nil, "请指定玩家 UID 或完整名字" end
  local matches = {}
  local uid = who:gsub("-", ""):upper()
  for _, info in ipairs(refreshPlayers()) do
    if info.uid == uid or info.name:lower() == who:lower() then matches[#matches+1] = info end
  end
  if #matches == 1 then return matches[1] end
  if #matches > 1 then return nil, "玩家名字不唯一，请从下拉框选择 UID" end
  return nil, "玩家不在线或尚未加载完成：" .. who .. "（请刷新玩家列表）"
end

-- ============ 业务动作 ============
local MAX_GIVE_COUNT = 10000
local function positiveInteger(v, max)
  return type(v) == "number" and v == v and v % 1 == 0 and v >= 1 and v <= max
end

local function getInventory(info)
  local inv = info.obj:GetInventoryData()
  if not valid(inv) then return nil, "玩家背包尚未初始化，请等待进入世界后重试" end
  if guidString(inv.OwnerPlayerUId) ~= info.uid then
    return nil, "背包所有者与目标玩家 UID 不一致，已拒绝操作"
  end
  return inv
end

local function itemCount(inv, id)
  local n = inv:CountItemNum(id)
  if type(n) ~= "number" or n % 1 ~= 0 or n < 0 then error("背包数量读取异常") end
  return n
end

local itemResultLabels = {
  [1] = "未执行任何操作", [3] = "背包不存在", [4] = "背包槽位已满",
  [5] = "背包槽位不足", [6] = "物品堆叠溢出", [7] = "找不到物品容器",
  [11] = "无法创建动态物品", [13] = "找不到物品容器", [15] = "物品 ID 不存在",
  [16] = "背包空间不足", [26] = "权限不足", [27] = "该物品不允许放入背包",
  [28] = "容器不可操作", [29] = "操作受限", [31] = "背包事务锁定", [32] = "找不到物品表记录",
}

local function giveItem(info, itemId, count)
  if type(itemId) ~= "string" or not itemId:match("^[%w_%-]+$") or #itemId > 128 then
    return false, "物品 ID 格式不正确"
  end
  if not positiveInteger(count, MAX_GIVE_COUNT) then return false, "数量必须是 1～10000 的整数" end
  local inv, err = getInventory(info)
  if not inv then return false, err end
  -- Lua string 不是 FName userdata：直接传字符串会在绑定层 reinterpret_cast 后 SEGV。
  local id = FName(itemId)
  local before = itemCount(inv, id)
  -- 必须走一次确定签名的服务端同步接口，绝不试多个候选/重复调用。
  local called, result = pcall(function() return inv:AddItem_ServerInternal(id, count, false, 0.0, true) end)
  local counted, after = pcall(itemCount, inv, id)
  step(string.format("give uid=%s item=%s count=%d before=%d after=%s result=%s", info.uid, itemId, count, before, tostring(after), tostring(result)))
  if not called then
    return false, "给物品调用异常，结果不确定，请先检查背包，不要直接重试：" .. tostring(result)
  end
  if not counted then return false, "已执行给物品，但无法核验到账数量，请检查背包，不要直接重试：" .. tostring(after) end
  local delta = after - before
  if result ~= 0 or delta ~= count then
    return false, string.format("未完整到账：%s（返回码 %s）；背包 %d → %d，实际增加 %d/%d。请先核对背包，勿重复发放。",
      itemResultLabels[result] or (result == 0 and "数量不符" or "游戏拒绝或未知结果"), tostring(result), before, after, delta, count)
  end
  return true, string.format("已给 %s（%s）%s ×%d；背包 %d → %d（已核验）", info.name, info.uid, itemId, count, before, after)
end

local function giveExp(info, amount)
  if not positiveInteger(amount, 10000000) then return false, "经验必须是 1～10000000 的整数" end
  local ok, r = pcall(function() return info.obj:AddExp_ServerInternal(amount, false, true, 1.0) end)
  if ok then return true, "经验请求已执行（未核验经验增量）：" .. tostring(r) end
  return false, "给经验失败: " .. tostring(r)
end

local function doGive(who, itemId, count)
  local info, err = findPlayer(who)
  if not info then return false, err end
  return giveItem(info, itemId, count)
end

local function listPlayers()
  local out = {}
  for _, info in ipairs(refreshPlayers()) do out[#out+1] = info.name .. " · " .. info.uid end
  if #out == 0 then return "当前没有已加载完成的在线玩家" end
  return table.concat(out, "\n")
end

-- ============ JSON 辅助（无第三方库，手写转义） ============
local function jsonStr(s)
  if s == nil then return "null" end
  s = tostring(s)
  s = s:gsub("\\", "\\\\"):gsub("\"", "\\\""):gsub("\r", "\\r"):gsub("\n", "\\n"):gsub("\t", "\\t")
  s = s:gsub("%c", " ")
  return "\"" .. s .. "\""
end

-- UID 来源于当前连接的 PlayerState，不按 REST/FindAllOf 的数组顺序拼接身份。
local function playersJSON()
  local out = {}
  for _, info in ipairs(refreshPlayers()) do
    out[#out+1] = string.format("{\"name\":%s,\"uid\":%s,\"steam\":\"\"}", jsonStr(info.name), jsonStr(info.uid))
  end
  return true, "[" .. table.concat(out, ",") .. "]"
end

-- ============ 物品表导出（面板「给物品」用） ============
local ITEM_DT_PATHS = {
  "/Game/Pal/DataTable/Item/DT_ItemDataTable.DT_ItemDataTable",
  "/Game/Pal/DataTable/Item/DT_ItemDataTable",
}
local ITEMS_FILE = "gspanel-mod/items.json"

local function findObject(path)
  local ok, obj = pcall(StaticFindObject, path)
  if ok and obj then
    local okv, valid = pcall(function() return obj:IsValid() end)
    if okv and valid ~= false then return obj end
  end
  return nil
end

local function dumpItems()
  local dt, how = nil, nil
  for _, p in ipairs(ITEM_DT_PATHS) do
    dt = findObject(p)
    if dt then how = p break end
  end
  if not dt then
    return false, "找不到物品数据表 DT_ItemDataTable（游戏是否已加载完？）"
  end
  local okN, ids = pcall(function() return dt:GetRowNames() end)
  step("dumpItems: GetRowNames ok=" .. tostring(okN) .. " type=" .. type(ids))
  if not okN or type(ids) ~= "table" then
    return false, "GetRowNames 失败: " .. tostring(ids)
  end
  local total = #ids
  if total == 0 then return false, "物品数据表为空（游戏可能还没加载完物品数据）" end
  log("items: got %d item rows via %s", total, tostring(how))

  local parts = {}
  for i = 1, total do
    local id = ids[i]
    if type(id) ~= "string" then id = tostring(id) end
    parts[#parts+1] = jsonStr(id)
  end

  local json = "{\"ok\":true,\"mod_version\":" .. jsonStr(MOD_VERSION) ..
    ",\"path\":" .. jsonStr(how or "") ..
    ",\"count\":" .. tostring(#parts) ..
    ",\"ids\":[" .. table.concat(parts, ",") .. "]}"
  local f = io.open(ITEMS_FILE, "w")
  if not f then
    return false, "无法写入 " .. ITEMS_FILE
  end
  f:write(json)
  f:close()

  local msg = string.format("已导出 %d 个物品 ID（表 %s，文件 %s）", #parts, tostring(how), ITEMS_FILE)
  log("%s", msg)
  return true, msg
end

-- ============ 诊断（probe） ============
-- probe throw / throwasync：故意触发一次绑定层 C++ throw（错误参数调 LoopAsync，
-- 该绑定对参数类型不符是裸 throw std::runtime_error），验证 libgxxfix.so 垫片
-- 在役——垫片生效时这里只返回一条 Lua 错误；没生效时这一下就会 SIGABRT 杀掉
-- 服务器（仅诊断用，别在生产乱点）。
local function probeThrow()
  local ok, err = pcall(LoopAsync, "not-a-number") -- 参数类型错误 → 绑定层裸 throw
  return string.format("pcall ok=%s err=%s", tostring(ok), tostring(err))
end

local function probe(sub, lines)
  if sub == "env" then
    return true, string.format("EngineTick=%s ProcessEvent=%s method=%s", tostring(EngineTickAvailable), tostring(ProcessEventAvailable), tostring(GT_NAME))
  elseif sub == "throw" or sub == "throwasync" then
    return true, "survived binding throw: " .. probeThrow()
  elseif sub == "gt" then
    return true, "已投递自检任务（看 steps.log 是否有 gt-selftest 行）"
  elseif sub == "selftest" then
    return selftest()
  elseif sub == "playercheck" then
    -- 无在线玩家也能覆盖 PlayerState 的只读属性绑定（使用 CDO，绝不做写操作）。
    selftest()
    local ps = StaticFindObject("/Script/Pal.Default__PalPlayerState")
    if not valid(ps) then return false, "PlayerState CDO 不存在" end
    step("playercheck: flags")
    local flags = ps:HasAnyFlags(0x10)
    step("playercheck: bool")
    local inactive = ps.bIsInactive
    local bot = ps.bIsABot
    step("playercheck: owner")
    local owner = ps.Owner
    step("playercheck: guid")
    local guid = guidString(ps.PlayerUId)
    step("playercheck: name")
    local name = toLuaString(ps.PlayerNamePrivate)
    step("playercheck: done")
    return true, string.format("CDO=%s inactive=%s bot=%s owner=%s uid=%s name=%s", tostring(flags), tostring(inactive), tostring(bot), tostring(valid(owner)), tostring(guid), tostring(name))
  elseif sub == "inventorycheck" then
    -- 背包接口需要有效世界，绝不能调用 Default__PalPlayerInventoryData（游戏会 fatal）。
    local info, err = findPlayer(lines[3])
    if not info then return false, err end
    local inv, invErr = getInventory(info)
    if not inv then return false, invErr end
    local kind = inv:GetInventoryTypeFromStaticItemID(FName("Wood"))
    local count = itemCount(inv, FName("Wood"))
    return true, "uid=" .. info.uid .. " Wood kind=" .. tostring(kind) .. " count=" .. tostring(count)
  elseif sub == "names" or sub == "who" then
    return true, listPlayers()
  elseif sub == "inv" then
    local info, err = findPlayer(lines[3])
    if not info then return false, err end
    local inv, invErr = getInventory(info)
    if not inv then return false, invErr end
    local count = itemCount(inv, FName("Wood"))
    return true, "uid=" .. info.uid .. " Wood=" .. tostring(count)
  end
  return false, "未知 probe: " .. tostring(sub)
end

-- ============ 命令分发 ============
local function dispatch(verb, lines)
  if verb == "give" then
    return doGive(lines[2], lines[3], tonumber(lines[4]))
  elseif verb == "giveexp" then
    local info, err = findPlayer(lines[2])
    if info then return giveExp(info, tonumber(lines[3])) end
    return false, err
  elseif verb == "who" then
    return true, listPlayers()
  elseif verb == "whojson" then
    return playersJSON()
  elseif verb == "hello" then
    return true, string.format("{\"version\":%s,\"protocol\":2,\"verbs\":[\"give\",\"giveexp\",\"who\",\"whojson\",\"hello\",\"items\",\"probe\"]}", jsonStr(MOD_VERSION))
  elseif verb == "items" then
    return dumpItems()
  elseif verb == "probe" then
    return probe(lines[2], lines)
  end
  return false, "未知命令: " .. tostring(verb)
end

-- ============ 控制台命令（RCON 兼容路径） ============
local function reply(Ar, text)
  if Ar then
    local ok = pcall(function() Ar:Log(text) end)
    if ok then return end
  end
  log("%s", text)
end

local function handle(cmd, args, Ar)
  if cmd == "gspwho" then
    reply(Ar, listPlayers())
    return true
  elseif cmd == "give" then
    if #args < 3 then reply(Ar, "用法: give <玩家名> <物品ID> <数量>") return true end
    local ok, msg = doGive(args[1], args[2], tonumber(args[3]))
    reply(Ar, (ok and "OK: " or "失败: ") .. msg)
    return true
  elseif cmd == "giveexp" then
    if #args < 2 then reply(Ar, "用法: giveexp <玩家名> <经验值>") return true end
    local info = findPlayer(args[1])
    if not info then reply(Ar, "失败: 玩家不在线 " .. args[1]) return true end
    local ok, msg = giveExp(info, tonumber(args[2]) or 0)
    reply(Ar, (ok and "OK: " or "失败: ") .. msg)
    return true
  end
  return false
end

for _, name in ipairs({ "gspwho", "give", "giveexp" }) do
  pcall(function()
    RegisterConsoleCommandGlobalHandler(name, function(Cmd, Parts, Ar)
      local args = {}
      if type(Parts) == "table" then
        for i = 2, #Parts do args[#args+1] = Parts[i] end
      end
      if #args == 0 and type(Cmd) == "string" then
        for w in Cmd:gmatch("%S+") do args[#args+1] = w end
        table.remove(args, 1)
      end
      local ok, handled = pcall(handle, name, args, Ar)
      if not ok then reply(Ar, "错误: " .. tostring(handled)) return true end
      return handled
    end)
  end)
end
log("console handlers registered: gspwho / give / giveexp")

-- ============ 文件队列（面板通道） ============
-- 面板写 <实例>/Pal/Binaries/Linux/gspanel-mod/cmd.txt，mod 处理后写 res.txt。
-- 命令文件格式（每行一项）：verb / arg1 / arg2 / arg3
local CMD_FILE = "gspanel-mod/cmd.txt"
local RES_FILE = "gspanel-mod/res.txt"

local function splitLines(s)
  local t = {}
  for line in tostring(s):gmatch("([^\n]*)\n") do t[#t+1] = line:gsub("\r$", "") end
  return t
end

local function writeResult(ok, msg, requestId)
  local of = io.open(RES_FILE .. ".tmp", "w")
  if not of then error("无法写入响应") end
  if requestId then of:write("gsp2:", requestId, "\n") end
  of:write(ok and "OK" or "FAIL", "\n", tostring(msg), "\n")
  of:close()
  assert(os.rename(RES_FILE .. ".tmp", RES_FILE))
end

pcall(function()
  LoopAsync(500, function()
    -- 整体再包一层 pcall：本回调任何漏网错误都不能逃出 Lua（有垫片后错误只是日志）
    local okAll, errAll = pcall(function()
      local f = io.open(CMD_FILE, "r")
      if f then
        local content = f:read("*a")
        f:close()
        os.remove(CMD_FILE)
        local lines = splitLines(content)
        local requestId = lines[1] and lines[1]:match("^gsp2:([%w%-]+)$")
        if requestId then table.remove(lines, 1) end
        local verb = lines[1]
        local function respond(ok, msg) writeResult(ok, msg, requestId) end
        step("queue: cmd=" .. tostring(verb))
        if verb == "probe" and (lines[2] == "env" or lines[2] == "throwasync") then
          -- 只读 Lua 全局 / 纯 async 线程探针，不需要游戏线程
          local ok, msg = dispatch(verb, lines)
          respond(ok, msg)
          log("cmd %s -> %s: %s", tostring(verb), ok and "OK" or "FAIL", tostring(msg))
        else
          runGT("cmd:" .. tostring(verb), function()
            if verb == "probe" and lines[2] == "gt" then
              step("gt-selftest: 在游戏线程执行成功")
            end
            local ok, msg = dispatch(verb, lines)
            respond(ok, msg)
            log("cmd %s -> %s: %s", tostring(verb), ok and "OK" or "FAIL", tostring(msg))
          end, function(err) respond(false, "命令异常：" .. err) end)
        end
      end
    end)
    if not okAll then
      step("queue loop error: " .. tostring(errAll))
      log("queue loop error: %s", tostring(errAll))
    end
    return false
  end)
  log("queue polling started at " .. CMD_FILE)
end)

-- 简单自检文件，确认 Lua 有文件写权限
local f = io.open("gspanel-mod/mod-alive.txt", "w")
if f then
  local stat = io.open("/proc/self/stat", "r")
  local pid = stat and (stat:read("*l") or ""):match("^(%d+)") or "0"
  if stat then stat:close() end
  f:write(os.date("%Y-%m-%d %H:%M:%S"), " " .. MOD_VERSION, " pid=", pid, "\n")
  f:close()
  log("mod-alive.txt written")
else
  log("WARN: cannot write mod-alive.txt")
end

log("mod loaded (version " .. MOD_VERSION .. ")")
