-- GSPanel 扩展命令 mod（Palworld 原生 Linux，基于 ue4ss-linux / LD_PRELOAD）
-- 版本：2026-09-13.3
--   面板会比对版本：旧版 mod 不支持 players/items 队列命令。
-- 功能：给物品 / 给经验 / 在线玩家索引 / 物品表导出（ID + 分类/堆叠上限）
--
-- 物品中文名不在这里取：本 build 的 UE4SS FText 转换有 bug（FText::ToString →
-- KismetTextLibrary native macro 找不到 UFunction，throw 后 abort 游戏进程），
-- 中文名由面板侧从游戏 pak 的 L10N/DT_ItemNameText 离线提取并内置（见 palworlditems.go）。
-- 这里的 items 命令只导出 DT_ItemDataTable 的行名（物品 ID）与少量非文本字段。
--
-- 两种驱动方式：
--   1) 控制台命令（RCON 或游戏内控制台）：give/giveexp/gspwho
--   2) 文件队列（gspanel-mod/cmd.txt），面板用：give/giveexp/who/whojson/hello/items
local LOG = "[gspanel]"
local MOD_VERSION = "2026-09-13.3"
local unpack = table.unpack or unpack -- Lua 5.4 只提供 table.unpack；直接调全区 unpack 会抛错并导致 UE4SS abort
local function log(fmt, ...)
  local ok, s = pcall(string.format, fmt, ...)
  print(LOG .. " " .. (ok and s or tostring(fmt)) .. "\n")
end

log("mod loading... (version " .. MOD_VERSION .. ")")

-- ============ 玩家索引 ============
local players = {}      -- key(小写) -> { obj=, name=, uid=, steam= }
local playerList = {}   -- 保持对象引用

local function guidStr(g)
  if g == nil then return nil end
  local ok, s = pcall(function() return g:ToString() end)
  if ok and s and s ~= "" then return s end
  ok, s = pcall(function()
    return string.format("%08X%08X%08X%08X", g.A, g.B, g.C, g.D)
  end)
  if ok then return s end
  return nil
end

local function indexPlayer(ps)
  if ps == nil then return end
  local ok, valid = pcall(function() return ps:IsValid() end)
  if not ok or valid == false then return end
  local info = { obj = ps }
  pcall(function() info.name = ps:GetPlayerName() end)
  pcall(function() info.uid = guidStr(ps.PlayerUId) end)
  if not info.uid then
    pcall(function() info.uid = guidStr(ps:GetPlayerUId()) end)
  end
  pcall(function()
    local u = ps.UniqueId
    if u then info.steam = tostring(u) end
  end)
  if not info.name and not info.uid and not info.steam then return end
  local keys = {}
  if info.name and info.name ~= "" then keys[#keys+1] = string.lower(info.name) end
  if info.uid then
    keys[#keys+1] = string.lower(info.uid)
    keys[#keys+1] = string.lower((info.uid:gsub("-", "")))
  end
  if info.steam then
    keys[#keys+1] = string.lower(info.steam)
    local digits = info.steam:match("(%d+)$")
    if digits then keys[#keys+1] = string.lower(digits) end
  end
  for _, k in ipairs(keys) do players[k] = info end
  playerList[#playerList+1] = info
  log("player indexed: name=%s uid=%s steam=%s", tostring(info.name), tostring(info.uid), tostring(info.steam))
end

local function refreshPlayers()
  local ok, list = pcall(FindAllOf, "PalPlayerState")
  if ok and list then
    for _, ps in ipairs(list) do indexPlayer(ps) end
  end
end

-- 新对象捕获（玩家加入）
pcall(function()
  NotifyOnNewObject("/Script/Pal.PalPlayerState", function(ps)
    indexPlayer(ps)
  end)
  log("NotifyOnNewObject registered")
end)

-- 定期兜底刷新
pcall(function()
  LoopAsync(10000, function()
    pcall(refreshPlayers)
    return false
  end)
  log("LoopAsync started")
end)
pcall(refreshPlayers)

local function findPlayer(who)
  if who == nil or who == "" then return nil end
  local w = string.lower(who)
  if players[w] then return players[w] end
  -- 支持只写 steamid 数字
  local digits = w:match("(%d+)$")
  if digits and players[digits] then return players[digits] end
  -- 兜底：现场再扫一次
  pcall(refreshPlayers)
  return players[w] or (digits and players[digits]) or nil
end

-- ============ 业务动作 ============
-- 给物品：1.0.4 里不同版本的接口名不同，按候选列表依次尝试，
-- 全部包 pcall，不会把游戏线程搞崩。
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

local function giveItem(info, itemId, count)
  local inv = info.obj:GetInventoryData()
  if inv == nil then return false, "GetInventoryData 返回空" end
  local errs = {}
  local candidates = {
    { "RequestAddItem", { itemId, count, true } },
    { "RequestAddItem_ToServer", { itemId, count, true } },
    { "RequestAddItem_ForDebug", { itemId, count, true } },
    { "AddItem_ServerInternal", { itemId, count, false, 0.0, true } },
  }
  for _, c in ipairs(candidates) do
    local ok, msg = callInvMethod(inv, c[1], unpack(c[2]))
    if ok then return true, string.format("%s x%d: %s", itemId, count, msg) end
    errs[#errs+1] = msg
  end
  return false, "全部候选接口失败: " .. table.concat(errs, " | ")
end

local function giveExp(info, amount)
  local ok, r = pcall(function() return info.obj:AddExp_ServerInternal(amount, false, true, 1.0) end)
  if ok then return true, "AddExp_ServerInternal => " .. tostring(r) end
  return false, "给经验失败: " .. tostring(r)
end

local function doGive(who, itemId, count)
  local info = findPlayer(who)
  if not info then return false, "玩家不在线: " .. tostring(who) end
  return giveItem(info, itemId, count)
end

local function listPlayers()
  pcall(refreshPlayers)
  local seen, out = {}, {}
  for _, info in ipairs(playerList) do
    local k = tostring(info.name) .. "|" .. tostring(info.uid)
    if not seen[k] then
      seen[k] = true
      out[#out+1] = string.format("%s | uid=%s | steam=%s", tostring(info.name), tostring(info.uid), tostring(info.steam))
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

local function jsonNum(n)
  if type(n) ~= "number" then return "null" end
  return string.format("%d", n)
end

-- 在线玩家结构化输出（面板下拉框用）
local function playersJSON()
  pcall(refreshPlayers)
  local seen, out = {}, {}
  for _, info in ipairs(playerList) do
    local k = tostring(info.name) .. "|" .. tostring(info.uid)
    if not seen[k] and info.name and info.name ~= "" then
      seen[k] = true
      out[#out+1] = string.format("{\"name\":%s,\"uid\":%s,\"steam\":%s}",
        jsonStr(info.name), jsonStr(info.uid or ""), jsonStr(info.steam or ""))
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

local function findItemTable()
  for _, p in ipairs(ITEM_DT_PATHS) do
    local dt = findObject(p)
    if dt then return dt, p end
  end
  return nil, nil
end

-- 枚举值 -> 名字（EPalItemTypeA:Weapon -> Weapon）
local function nameToStr(v)
  if v == nil then return nil end
  local t = type(v)
  if t == "string" then return v end
  if t == "number" or t == "boolean" then return tostring(v) end
  local ok, s = pcall(function() return v:ToString() end)
  if ok and type(s) == "string" then return s end
  return nil
end

local function enumName(enumObj, value)
  if not enumObj or value == nil or type(value) ~= "number" then return nil end
  local ok, n = pcall(function() return enumObj:GetNameByValue(value) end)
  if not ok or n == nil then return nil end
  local s = nameToStr(n)
  if not s then return nil end
  local short = s:match("::(.+)$")
  return short or s
end

-- 导出 DT_ItemDataTable 的行名（物品 ID）。
-- 注意：1.0.4 的行结构体 PalStaticItemDataStruct 是精简表，没有 Name/TypeA/TypeB/Rarity/
-- MaxStackCount 等字段（实测访问 TypeA 直接 abort）；物品详情在 DataAsset 里。
-- 本 build 又禁止任何 UE4SS 抛错（-> abort），所以这里只做最安全的 GetRowNames。
local function dumpItems()
  local dt, how = findItemTable()
  if not dt then
    return false, "找不到物品数据表 DT_ItemDataTable（游戏是否已加载完？）"
  end
  local okN, ids = pcall(function() return dt:GetRowNames() end)
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
    if i % 500 == 0 then log("items: row %d/%d", i, total) end
  end
  log("items: scan done rows=%d", #parts)

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
    if #args < 3 then reply(Ar, "用法: give <玩家名|UID|SteamID> <物品ID> <数量>") return true end
    local ok, msg = doGive(args[1], args[2], tonumber(args[3]) or 1)
    reply(Ar, (ok and "OK: " or "失败: ") .. msg)
    return true
  elseif cmd == "giveexp" then
    if #args < 2 then reply(Ar, "用法: giveexp <玩家名|UID|SteamID> <经验值>") return true end
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
      -- Parts 可能是整串命令
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
-- 注意：游戏进程启动后 cwd 会切到 Pal/Binaries/Linux，所以路径相对它。
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
    local f = io.open(CMD_FILE, "r")
    if f then
      local content = f:read("*a")
      f:close()
      os.remove(CMD_FILE)
      local lines = splitLines(content)
      local verb, ok, msg = lines[1]
      if verb == "give" then
        ok, msg = doGive(lines[2], lines[3], tonumber(lines[4]) or 1)
      elseif verb == "giveexp" then
        local info = findPlayer(lines[2])
        if info then ok, msg = giveExp(info, tonumber(lines[3]) or 0)
        else ok, msg = false, "玩家不在线: " .. tostring(lines[2]) end
      elseif verb == "who" then
        ok, msg = true, listPlayers()
      elseif verb == "whojson" then
        ok, msg = playersJSON()
      elseif verb == "hello" then
        ok, msg = true, string.format("{\"version\":%s,\"verbs\":[\"give\",\"giveexp\",\"who\",\"whojson\",\"hello\",\"items\"]}", jsonStr(MOD_VERSION))
      elseif verb == "items" then
        ok, msg = dumpItems()
      else
        ok, msg = false, "未知命令: " .. tostring(verb)
      end
      writeResult(ok, msg)
      log("cmd %s -> %s: %s", tostring(verb), ok and "OK" or "FAIL", tostring(msg))
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
