-- 纯 Lua 回归：模拟 UE4SS 可调用 UFunction（不是普通 function）和原子文件队列。
-- lua5.4 tools/palworld-ue4ss/test-mod.lua [main.lua]
local script = arg[1] or "assets/palworld-ue4ss/mod/scripts/main.lua"
local tests = 0
local function check(v, msg) tests=tests+1; assert(v, msg or ("assertion "..tests)) end
local function contains(s, part) return s:find(part,1,true) ~= nil end
local function object(t)
  t = t or {}
  t.IsValid = function() return true end
  return t
end
local function callable(fn) return setmetatable({}, {__call=function(_,...) return fn(...) end}) end
local function fstr(s) return {ToString=function() return s end} end
local guid = {A=0xA6F9455C, B=0, C=0, D=0}
local uid = "A6F9455C000000000000000000000000"
local inv, player, players, calls, mode, processOK, readFails
local files, renames = {}, 0
local loop
local env = setmetatable({}, {__index=_G})
env.print = function() end
env.EngineTickAvailable = true
env.GSPanelPropertyBindingsVersion = 1
env.EGameThreadMethod = {EngineTick=1}
env.ExecuteInGameThread = function(fn) fn() end
env.RegisterConsoleCommandGlobalHandler = function() end
env.FindAllOf = function() return players end
env.FName = function(id) return {fname=id} end
env.StaticFindObject = function(path)
  if path == "/Script/Pal.Default__PalPlayerState" then return object({PlayerUId=guid}) end
  if path == "/Script/Pal.EPalItemOperationResult" then
    return object({GetNameByValue=function() return fstr("EPalItemOperationResult::Success") end})
  end
  return object({Concat_StrStr=callable(function(_,a,b) return fstr(processOK and a..b or "") end),
    Conv_NameToString=callable(function(_,id) return fstr(id.fname) end)})
end
env.LoopAsync = function(_,fn) loop=fn end
env.io = {open=function(path, m)
  if m == "r" and not files[path] then return nil end
  local buffer = m == "a" and (files[path] or "") or ""
  return {read=function() return files[path] end,
    write=function(_,...) for _,v in ipairs({...}) do buffer=buffer..tostring(v) end end,
    close=function() if m ~= "r" then files[path]=buffer end end}
end}
env.os = {date=function() return "2026-09-14 00:00:00" end,
  remove=function(path) files[path]=nil; return true end,
  rename=function(a,b) renames=renames+1; files[b]=files[a]; files[a]=nil; return true end}
files['/proc/self/stat'] = '123 (PalServer) S'
assert(loadfile(script, "t", env))()
check(type(loop)=="function", "queue registered")
local function reset()
  calls=0; mode=0; processOK=true; readFails=false
  inv=object({OwnerPlayerUId=guid, count=5})
  inv.CountItemNum=callable(function(self,id)
    assert(type(id)=="table" and id.fname, "FName required")
    if readFails and calls>0 then error("read failed") end
    return self.count
  end)
  inv.AddItem_ServerInternal=callable(function(self,id,n,passive,delay,notify)
    check(type(id)=="table" and id.fname=="Wood", "FName(id), not string")
    check(passive==false and delay==0 and notify==true, "exact signature")
    calls=calls+1
    if mode=="partial" then self.count=self.count+1; return 0 end
    if mode=="throws" then self.count=self.count+n; error("after mutation") end
    if mode==0 then self.count=self.count+n end
    return mode
  end)
  player=object({PlayerNamePrivate=fstr('测试 玩家'), PlayerUId=guid, bIsInactive=false, bIsABot=false,
    Owner=object({NetConnection=object()}), HasAnyFlags=function() return false end,
    GetInventoryData=callable(function() return inv end), GetPlayerName=callable(function() return fstr('测试 玩家') end)})
  players={player}
end
local req=0
local function command(verb,...)
  req=req+1
  files['gspanel-mod/cmd.txt']='gsp2:test-'..req..'\n'..verb..'\n'..table.concat({...},'\n')..'\n'
  loop()
  local res=files['gspanel-mod/res.txt'] or ''
  check(contains(res, 'gsp2:test-'..req..'\n'), 'correlated response')
  check(files['gspanel-mod/cmd.txt']==nil and files['gspanel-mod/res.txt.tmp']==nil,'atomic consume/respond')
  return contains(res, '\nOK\n'), res
end
reset()
local ok,res=command('whojson'); check(ok and contains(res,'测试 玩家') and contains(res,uid), 'name/uid')
local a,b=command('give',uid,'Wood','3'); check(a and inv.count==8 and calls==1 and contains(b,'已核验'), 'verified grant')
players={}; a,b=command('give',uid,'Wood','1'); check(not a and calls==1,'no stale player after disconnect')
reset(); players={player,player}; a,b=command('give','测试 玩家','Wood','1'); check(not a and calls==0 and contains(b,'不唯一'),'duplicate name rejects')
reset(); player.bIsInactive=true; a,b=command('whojson'); check(a and contains(b,'[]'),'inactive omitted')
reset(); player.Owner.NetConnection=nil; a,b=command('whojson'); check(a and contains(b,'[]'),'disconnected omitted')
reset(); player.HasAnyFlags=function() return true end; a,b=command('whojson'); check(a and contains(b,'[]'),'CDO omitted')
reset(); inv.OwnerPlayerUId={A=1,B=0,C=0,D=0}; a,b=command('give',uid,'Wood','1'); check(not a and calls==0,'ownership mismatch')
for _,qty in ipairs({'0','-1','1.5','nan','inf','10001',''}) do
  reset(); a,b=command('give',uid,'Wood',qty); check(not a and calls==0,'invalid quantity '..qty)
end
reset(); a,b=command('give',uid,'bad id','1'); check(not a and calls==0,'invalid item id')
for _,result in ipairs({1,3,15,16,26,99}) do
  reset(); mode=result; a,b=command('give',uid,'Wood','2'); check(not a and calls==1 and inv.count==5,'enum failure never retries')
end
reset(); mode='partial'; a,b=command('give',uid,'Wood','3'); check(not a and calls==1 and contains(b,'实际增加 1/3'),'partial not reported as success')
reset(); mode='throws'; a,b=command('give',uid,'Wood','3'); check(not a and calls==1 and contains(b,'不要直接重试'),'exception after side effect')
reset(); readFails=true; a,b=command('give',uid,'Wood','2'); check(not a and calls==1 and contains(b,'不要直接重试'),'verification failure')
reset(); env.GSPanelPropertyBindingsVersion=nil; a,b=command('give',uid,'Wood','1'); check(not a and calls==0 and contains(b,'框架缺少'),'old framework fail closed'); env.GSPanelPropertyBindingsVersion=1
reset(); processOK=false; a,b=command('give',uid,'Wood','1'); check(not a and calls==0 and contains(b,'自检失败'),'old layout fail closed')
reset(); a,b=command('unknown'); check(not a and contains(b,'未知命令'),'dispatch error answered')
check(renames==req,'every response atomic')
print(string.format('PASS: %d assertions, %d queue commands',tests,req))
