-- GSPanel 扩展命令 mod（Palworld 原生 Linux，基于 ue4ss-linux / LD_PRELOAD）
-- 版本：2026-09-13.11
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
-- ★ 2026-09-13.8 修复（保留）★
--   1) 去掉一切“后台自动碰玩家对象”的行为（原来的 LoopAsync(10000) 每 10 秒
--      扫 FindAllOf + 读玩家属性；它会在玩家刚进服的瞬间读到半初始化的对象 → 崩）。
--      现在只在面板真的发命令时（whojson / give / giveexp）才现场刷新玩家索引。
--   2) 不再读 FGuid / FUniqueNetIdRepl 这些**结构体属性**（ps.PlayerUId / ps.UniqueId）：
--      结构体属性读取在本 build 不可靠。玩家只用名字匹配（面板下拉框里的值就是名字）。
--   3) 只调用确定存在的接口：ps:GetPlayerName()（返回 FString 包装，调 :ToString() 取字符串），
--      失败时回退读 PlayerName 属性。
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
local MOD_VERSION = "2026-09-13.11"
local unpack = table.unpack or unpack -- Lua 5.4 只提供 table.unpack；直接调全区 unpack 会抛错并导致 UE4SS abort
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

local function runGT(name, fn)
  local ok, err = pcall(function()
    local function body()
      local ok2, err2 = pcall(fn)
      if not ok2 then
        step(name .. ": gt-error " .. tostring(err2))
        log("[gt] %s 出错: %s", name, tostring(err2))
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
  end
end

-- ============ 玩家索引（只按名字） ============
local players = {}   -- 小写名字 -> { obj=, name= }

-- 读玩家名。GetPlayerName() 返回 UE4SS 的 FString 包装对象，
-- 要调 :ToString() 才是 Lua 字符串（tostring(包装) 只会给出地址）。
-- 把各种“字符串”表示转成 Lua 字符串
local function toLuaString(v)
  if v == nil then return nil, "nil" end
  if type(v) == "string" then return v, "string" end
  local ok, s = pcall(function() return v:ToString() end)
  if ok and type(s) == "string" and s ~= "" then return s, "ToString" end
  local okLen, n = pcall(function() return v:Len() end)
  return nil, string.format("type=%s tostring_ok=%s len=%s", type(v), tostring(ok), tostring(okLen and n))
end

-- 读玩家名：先用属性（wrapper 指向活着的属性内存），再用 GetPlayerName()
local function readName(ps)
  local okP, prop = pcall(function() return ps.PlayerName end)
  if okP and prop ~= nil then
    local s, how = toLuaString(prop)
    if s then return s end
    step("readName: PlayerName 属性读不出 " .. tostring(how))
  else
    step("readName: PlayerName 属性访问失败 ok=" .. tostring(okP) .. " v=" .. tostring(prop))
  end
  local okF, fn = pcall(function() return ps:GetPlayerName() end)
  if okF and fn ~= nil then
    local s2, how2 = toLuaString(fn)
    if s2 then return s2 end
    step("readName: GetPlayerName 读不出 " .. tostring(how2))
  else
    step("readName: GetPlayerName 调用失败 ok=" .. tostring(okF) .. " v=" .. tostring(fn))
  end
  return nil
end

local function indexPlayer(ps, idx)
  if ps == nil then return nil end
  local ok, valid = pcall(function() return ps:IsValid() end)
  if not ok or valid == false then return nil end
  local name = readName(ps)
  local key
  if name and name ~= "" then
    key = name
  else
    -- 名字读不出来也不要丢：用 玩家#序号 当标识（面板可以按序号发命令）
    key = "玩家#" .. tostring(idx or "?")
  end
  local info = { obj = ps, name = key, idx = idx }
  players[string.lower(key)] = info
  if name and name ~= "" and idx then
    players["#" .. tostring(idx)] = info   -- 别名：#1
  end
  return info
end

-- 只在命令里调用（不后台轮询）
local function refreshPlayers()
  step("refreshPlayers: FindAllOf enter")
  local ok, list = pcall(FindAllOf, "PalPlayerState")
  if not ok or type(list) ~= "table" then
    step("refreshPlayers: FindAllOf 失败 ok=" .. tostring(ok) .. " type=" .. type(list))
    return 0
  end
  local n = 0
  for i, ps in ipairs(list) do
    if indexPlayer(ps, i) then n = n + 1 end
  end
  step("refreshPlayers: scanned=" .. tostring(n))
  return n
end

local function findPlayer(who)
  if who == nil or who == "" then return nil end
  local info = players[string.lower(who)]
  if info then return info end
  refreshPlayers()
  return players[string.lower(who)]
end

-- ============ 业务动作 ============
-- 给物品：1.0.4 里不同版本的接口名不同，按候选列表依次尝试，全部包 pcall。
local function callInvMethod(inv, method, ...)
  local args = { ... } -- Lua 不允许在嵌套函数里用 ...，先捕获
  local f = inv[method]
  if type(f) ~= "function" then return false, method .. " 不可用" end
  local ok, res = pcall(function() return f(inv, unpack(args)) end)
  if ok then return true, method .. " => " .. tostring(res) end
  local ok2, res2 = pcall(function() return f(unpack(args)) end)
  if ok2 then return true, method .. " => " .. tostring(res2) end
  return false, method .. " 调用失败: " .. tostring(res)
end

local function getInventory(info)
  if info == nil or info.obj == nil then return nil, "玩家对象为空" end
  local ok, inv = pcall(function() return info.obj:GetInventoryData() end)
  step("getInventory: GetInventoryData ok=" .. tostring(ok) .. " type=" .. type(inv) .. " v=" .. tostring(inv))
  if not ok then return nil, "GetInventoryData 出错: " .. tostring(inv) end
  if inv == nil then return nil, "GetInventoryData 返回空" end
  return inv
end

local function giveItem(info, itemId, count)
  local inv, err = getInventory(info)
  if not inv then return false, err end
  local errs = {}
  local candidates = {
    { "RequestAddItem", { itemId, count, true } },
    { "RequestAddItem_ToServer", { itemId, count, true } },
    { "RequestAddItem_ForDebug", { itemId, count, true } },
    { "AddItem_ServerInternal", { itemId, count, false, 0.0, true } },
  }
  for _, c in ipairs(candidates) do
    local ok, msg = callInvMethod(inv, c[1], unpack(c[2]))
    step("giveItem: " .. c[1] .. " ok=" .. tostring(ok) .. " " .. tostring(msg))
    if ok then return true, string.format("%s x%d: %s", itemId, count, msg) end
    errs[#errs+1] = msg
  end
  return false, "全部候选接口失败: " .. table.concat(errs, " | ")
end

local function giveExp(info, amount)
  local ok, r = pcall(function() return info.obj:AddExp_ServerInternal(amount, false, true, 1.0) end)
  step("giveExp: ok=" .. tostring(ok) .. " r=" .. tostring(r))
  if ok then return true, "AddExp_ServerInternal => " .. tostring(r) end
  return false, "给经验失败: " .. tostring(r)
end

local function doGive(who, itemId, count)
  local info = findPlayer(who)
  if not info then return false, "玩家不在线: " .. tostring(who) end
  return giveItem(info, itemId, count)
end

local function listPlayers()
  refreshPlayers()
  local seen, out = {}, {}
  for _, info in pairs(players) do
    if info.name and not seen[info.name] then
      seen[info.name] = true
      out[#out+1] = info.name
    end
  end
  if #out == 0 then return "当前没有索引到在线玩家" end
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

-- 在线玩家结构化输出（面板下拉框用）。uid/steam 读结构体属性会让本 build 崩，
-- 所以只给名字（面板会退化成用名字匹配）。
local function playersJSON()
  refreshPlayers()
  local seen, out = {}, {}
  for _, info in pairs(players) do
    if info.name and info.name ~= "" and not seen[info.name] then
      seen[info.name] = true
      out[#out+1] = string.format("{\"name\":%s,\"uid\":\"\",\"steam\":\"\"}", jsonStr(info.name))
    end
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
  elseif sub == "names" then
    local ok, list = pcall(FindAllOf, "PalPlayerState")
    if not ok or type(list) ~= "table" then return false, "FindAllOf 失败 ok=" .. tostring(ok) .. " type=" .. type(list) end
    local out = {}
    for i, ps in ipairs(list) do
      local o = {}
      o[#o+1] = string.format("#%d IsValid=%s", i, tostring(select(2, pcall(function() return ps:IsValid() end))))
      o[#o+1] = "  full=" .. tostring(select(2, pcall(function() return ps:GetFullName() end)))
      o[#o+1] = "  class=" .. tostring(select(2, pcall(function() return ps:GetClass():GetFullName() end)))
      local prop = select(2, pcall(function() return ps.PlayerName end))
      o[#o+1] = "  PlayerName type=" .. type(prop) .. " len=" .. tostring(select(2, pcall(function() return prop:Len() end)))
        .. " ToString=" .. tostring(select(2, pcall(function() return prop:ToString() end)))
      local fn = select(2, pcall(function() return ps:GetPlayerName() end))
      o[#o+1] = "  GetPlayerName type=" .. type(fn) .. " len=" .. tostring(select(2, pcall(function() return fn:Len() end)))
        .. " ToString=" .. tostring(select(2, pcall(function() return fn:ToString() end)))
      o[#o+1] = "  PlayerId=" .. tostring(select(2, pcall(function() return ps.PlayerId end)))
      o[#o+1] = "  PlayerUId type=" .. type(select(2, pcall(function() return ps.PlayerUId end)))
      out[#out+1] = table.concat(o, "\n")
    end
    return true, table.concat(out, "\n")
  elseif sub == "who" then
    return true, listPlayers()
  elseif sub == "inv" then
    local info
    if lines[2] and lines[2] ~= "" then info = findPlayer(lines[2]) else
      refreshPlayers()
      for _, v in pairs(players) do info = v break end
    end
    if not info then return false, "没有在线玩家" end
    local inv, err = getInventory(info)
    if not inv then return false, tostring(err) end
    return true, "inv=" .. tostring(inv)
  end
  return false, "未知 probe: " .. tostring(sub)
end

-- ============ 命令分发 ============
local function dispatch(verb, lines)
  if verb == "give" then
    return doGive(lines[2], lines[3], tonumber(lines[4]) or 1)
  elseif verb == "giveexp" then
    local info = findPlayer(lines[2])
    if info then return giveExp(info, tonumber(lines[3]) or 0) end
    return false, "玩家不在线: " .. tostring(lines[2])
  elseif verb == "who" then
    return true, listPlayers()
  elseif verb == "whojson" then
    return playersJSON()
  elseif verb == "hello" then
    return true, string.format("{\"version\":%s,\"verbs\":[\"give\",\"giveexp\",\"who\",\"whojson\",\"hello\",\"items\",\"probe\"]}", jsonStr(MOD_VERSION))
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
    local ok, msg = doGive(args[1], args[2], tonumber(args[3]) or 1)
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
  for line in tostring(s):gmatch("[^\r\n]+") do t[#t+1] = line end
  return t
end

local function writeResult(ok, msg)
  local of = io.open(RES_FILE, "w")
  if of then
    of:write(ok and "OK" or "FAIL", "\n", tostring(msg), "\n")
    of:close()
  end
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
        local verb = lines[1]
        step("queue: cmd=" .. tostring(verb))
        if verb == "probe" and (lines[2] == "env" or lines[2] == "throwasync") then
          -- 只读 Lua 全局 / 纯 async 线程探针，不需要游戏线程
          local ok, msg = dispatch(verb, lines)
          writeResult(ok, msg)
          log("cmd %s -> %s: %s", tostring(verb), ok and "OK" or "FAIL", tostring(msg))
        else
          runGT("cmd:" .. tostring(verb), function()
            if verb == "probe" and lines[2] == "gt" then
              step("gt-selftest: 在游戏线程执行成功")
            end
            local ok, msg = dispatch(verb, lines)
            writeResult(ok, msg)
            log("cmd %s -> %s: %s", tostring(verb), ok and "OK" or "FAIL", tostring(msg))
          end)
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
  f:write(os.date("%Y-%m-%d %H:%M:%S"), " " .. MOD_VERSION, "\n")
  f:close()
  log("mod-alive.txt written")
else
  log("WARN: cannot write mod-alive.txt")
end

log("mod loaded (version " .. MOD_VERSION .. ")")
