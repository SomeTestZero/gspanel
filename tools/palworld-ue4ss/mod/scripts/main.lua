-- GSPanel 扩展命令 mod（Palworld 原生 Linux，基于 ue4ss-linux / LD_PRELOAD）
-- 功能：给物品 / 给经验 / 给帕鲁（可扩展）+ 在线玩家索引
-- 两种驱动方式：
--   1) 控制台命令（RCON 或游戏内控制台）：give/giveexp/givepal/gspwho
--   2) 文件队列（gspanel-mod/queue/*.txt），面板兜底用，无需依赖 RCON 分发实现
local LOG = "[gspanel]"
local unpack = table.unpack or unpack -- Lua 5.4 只提供 table.unpack；直接调全区 unpack 会抛错并导致 UE4SS abort
local function log(fmt, ...)
  local ok, s = pcall(string.format, fmt, ...)
  print(LOG .. " " .. (ok and s or tostring(fmt)) .. "\n")
end

log("mod loading...")

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
      else
        ok, msg = false, "未知命令: " .. tostring(verb)
      end
      local of = io.open(RES_FILE, "w")
      if of then
        of:write(ok and "OK" or "FAIL", "\n", tostring(msg), "\n")
        of:close()
      end
      log("cmd %s -> %s: %s", tostring(verb), ok and "OK" or "FAIL", tostring(msg))
    end
    return false
  end)
  log("queue polling started at " .. CMD_FILE)
end)

-- 简单自检文件，确认 Lua 有文件写权限
local f = io.open("gspanel-mod/mod-alive.txt", "w")
if f then
  f:write(os.date("%Y-%m-%d %H:%M:%S"), "\n")
  f:close()
  log("mod-alive.txt written")
else
  log("WARN: cannot write mod-alive.txt")
end

log("mod loaded")
