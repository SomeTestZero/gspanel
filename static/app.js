/* GSPanel 前端：无框架单页应用 */
"use strict";

const $app = document.getElementById("app");
let S = {
  authed: false,
  instances: [],
  templates: [],
  system: null,
  route: { page: "dashboard" },
  es: null,          // 当前 EventSource
  pollers: [],       // 定时器
  taskStream: null,
};

/* ---------- 工具 ---------- */
function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));
}
function fmtBytes(b) {
  if (!b) return "0";
  const u = ["B", "K", "M", "G", "T"];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return b.toFixed(b >= 100 ? 0 : 1) + u[i];
}
function toast(msg, ok = true) {
  const d = document.createElement("div");
  d.className = "toast " + (ok ? "ok" : "err");
  d.textContent = msg;
  document.body.appendChild(d);
  setTimeout(() => d.remove(), 3500);
}
async function api(path, opts = {}) {
  const r = await fetch(path, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    ...opts,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
  });
  if (r.status === 401 && !path.endsWith("/login")) {
    // 会话失效：直接切到登录页（不能调 render()，否则 /api/me 的 401 会造成无限递归重绘）
    S.authed = false;
    clearTimers();
    renderLogin();
    throw new Error("未登录");
  }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
  return data;
}
function clearTimers() {
  S.pollers.forEach(clearInterval);
  S.pollers = [];
  if (S.es) { S.es.close(); S.es = null; }
}
function addPoll(fn, ms) {
  S.pollers.push(setInterval(fn, ms));
}
function dotClass(st) {
  if (!st) return "off";
  if (st.active_state === "active") return "on";
  if (st.active_state === "failed") return "fail";
  return "off";
}
function statusText(st, installed) {
  if (!installed) return "未安装";
  const map = { active: "运行中", inactive: "已停止", failed: "失败", activating: "启动中", deactivating: "停止中", "not-installed": "未安装" };
  return (st && map[st.active_state]) || (st ? st.active_state : "未知");
}
function statusBadge(st, installed) {
  const t = statusText(st, installed);
  const cls = t === "运行中" ? "green" : (t === "失败" ? "red" : (t === "未安装" ? "" : "yellow"));
  return `<span class="badge ${cls}">${esc(t)}</span>`;
}

/* ---------- 路由 ---------- */
function nav(page, arg) {
  const h = arg ? `#/${page}/${arg}` : `#/${page}`;
  if (location.hash === h) render(); // hash 相同不会触发 hashchange（如渲染被竞态覆盖后），需手动重绘
  else location.hash = h;
}
function parseRoute() {
  const h = location.hash.replace(/^#\/?/, "");
  const [page, arg, arg2] = h.split("/");
  S.route = { page: page || "dashboard", arg, arg2 };
}
window.addEventListener("hashchange", render);

/* ---------- 登录 ---------- */
function renderLogin() {
  $app.innerHTML = `
  <div class="login-wrap"><div class="login-box">
    <h1>GS<span style="color:var(--accent)">Panel</span></h1>
    <p>轻量游戏服务器管理面板</p>
    <label>管理员密码</label>
    <input type="password" id="pw" autofocus placeholder="输入密码">
    <button class="primary" id="loginBtn">登 录</button>
  </div></div>`;
  const doLogin = async () => {
    try {
      await api("/api/login", { method: "POST", body: { password: document.getElementById("pw").value } });
      S.authed = true;
      nav("dashboard");
      render();
    } catch (e) { toast(e.message, false); }
  };
  document.getElementById("loginBtn").onclick = doLogin;
  document.getElementById("pw").onkeydown = e => { if (e.key === "Enter") doLogin(); };
}

/* ---------- 框架 ---------- */
function renderLayout(content) {
  const instItems = S.instances.map(i => {
    const st = i.status;
    return `<a class="nav-item ${S.route.page === "instance" && S.route.arg === i.name ? "active" : ""}" href="#/instance/${esc(i.name)}">
      <span class="dot ${dotClass(st)}" style="display:inline-block;margin-right:7px"></span>${esc(i.display_name)}</a>`;
  }).join("");
  $app.innerHTML = `
  <div class="layout">
    <div class="sidebar">
      <div class="brand">GS<span>Panel</span></div>
      <a class="nav-item ${S.route.page === "dashboard" ? "active" : ""}" href="#/dashboard">仪表盘</a>
      <div class="nav-group">游戏实例</div>
      ${instItems || '<div class="nav-item" style="cursor:default">暂无实例</div>'}
      <a class="nav-item ${S.route.page === "new" ? "active" : ""}" href="#/new">＋ 新建实例</a>
      <div class="nav-group">系统</div>
      <a class="nav-item ${S.route.page === "tasks" ? "active" : ""}" href="#/tasks">后台任务</a>
      <a class="nav-item ${S.route.page === "settings" ? "active" : ""}" href="#/settings">设置 / 环境</a>
      <a class="nav-item" id="logoutBtn">退出登录</a>
    </div>
    <div class="main" id="main">${content}</div>
  </div>`;
  document.getElementById("logoutBtn").onclick = async () => {
    await api("/api/logout", { method: "POST" }).catch(() => {});
    S.authed = false;
    render();
  };
}

/* 复制到剪贴板（http 非安全上下文时用 execCommand 兜底） */
window.copyText = async function (text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const ta = document.createElement("textarea");
      ta.value = text;
      ta.style.cssText = "position:fixed;opacity:0";
      document.body.appendChild(ta);
      ta.select();
      document.execCommand("copy");
      ta.remove();
    }
    toast("已复制: " + text);
  } catch (e) { toast("复制失败，请手动复制: " + text, false); }
};

/* 实例连接地址（仪表盘卡片用） */
function connectInfo(i, publicIp) {
  const pubs = i.public_ports || [];
  if (!pubs.length) return "";
  if (!publicIp) {
    return `<div class="meta" style="margin-top:4px">公网地址获取失败，请到「设置/环境」手动填写</div>`;
  }
  return pubs.map(p => `
    <div class="row" style="gap:8px;margin-top:4px">
      <span class="meta">${esc(p.desc || p.key)}</span>
      <span class="mono" style="color:var(--accent);font-size:13px">${esc(publicIp)}:${p.port}</span>
      <button class="small" onclick="copyText('${esc(publicIp)}:${p.port}')">复制</button>
    </div>`).join("");
}

/* ---------- 仪表盘 ---------- */
function statCard(k, v, pct) {
  const cls = pct == null ? "" : (pct > 90 ? "crit" : pct > 70 ? "warn" : "");
  return `<div class="stat-card"><div class="k">${k}</div><div class="v">${v}</div>
    ${pct == null ? "" : `<div class="bar ${cls}"><div style="width:${Math.min(100, pct)}%"></div></div>`}</div>`;
}

async function renderDashboard() {
  const [sys, insts] = await Promise.all([api("/api/system"), api("/api/instances")]);
  S.system = sys;
  S.instances = insts;
  const st = sys.stats;
  const stats = `
  <div class="grid cols-4">
    ${statCard("CPU 负载 (1/5/15分钟)", `${st.load1.toFixed(2)} / ${st.load5.toFixed(2)} / ${st.load15.toFixed(2)}`, st.load1 / st.cpu_cores * 100)}
    ${statCard("内存", `${fmtBytes(st.mem_total - st.mem_avail)} / ${fmtBytes(st.mem_total)}`, st.mem_used_pct)}
    ${statCard("Swap", `${fmtBytes(st.swap_total - st.swap_free)} / ${fmtBytes(st.swap_total)}`, st.swap_total ? (st.swap_total - st.swap_free) / st.swap_total * 100 : 0)}
    ${statCard("磁盘 /", `${fmtBytes(st.disk_total - st.disk_free)} / ${fmtBytes(st.disk_total)}`, st.disk_used_pct)}
  </div>
  <div class="row mt" style="color:var(--text-dim);font-size:12px">
    <span>运行时长: ${esc(st.uptime)}</span><span>·</span><span>CPU 核心: ${st.cpu_cores}</span><span>·</span>
    <span>steamcmd: ${sys.deps.steamcmd ? '<span class="badge green">已安装</span>' : '<span class="badge red">未安装（到「设置/环境」安装）</span>'}</span>
  </div>`;

  const cards = insts.length ? insts.map(i => {
    const running = i.status && i.status.active_state === "active";
    return `<div class="card inst-card">
      <span class="dot ${dotClass(i.status)}"></span>
      <div>
        <div class="name">${esc(i.display_name)}</div>
        <div class="meta">${esc(i.template_name)} · ${esc(i.name)} · ${Object.entries(i.ports).map(([k, v]) => `${k}:${v}`).join(" ")}</div>
        ${connectInfo(i, sys.public_ip)}
      </div>
      <div class="res">${running ? `内存 ${fmtBytes(i.status.memory_bytes)} · CPU ${(i.cpu_percent || 0).toFixed(0)}%` : statusText(i.status, i.installed)}</div>
      <div class="actions">
        ${!i.installed ? `<button class="small primary" data-act="install" data-n="${esc(i.name)}">安装</button>` : ""}
        ${i.installed && !running ? `<button class="small primary" data-act="start" data-n="${esc(i.name)}">启动</button>` : ""}
        ${running ? `<button class="small" data-act="restart" data-n="${esc(i.name)}">重启</button>
                    <button class="small" data-act="stop" data-n="${esc(i.name)}">停止</button>` : ""}
        <button class="small" onclick="nav('instance','${esc(i.name)}')">管理</button>
      </div>
    </div>`;
  }).join("") : `<div class="card empty">还没有实例，点击左侧「＋ 新建实例」创建第一个游戏服务器</div>`;

  // 竞态守卫：await 期间用户可能已跳转其他页面，过期渲染直接丢弃
  if (S.route.page !== "dashboard") return;
  renderLayout(`<div class="page-title">仪表盘</div>${stats}<div class="page-title mt">实例</div>${cards}`);
  document.querySelectorAll("[data-act]").forEach(b => b.onclick = () => instanceAction(b.dataset.n, b.dataset.act));
  addPoll(async () => {
    if (S.route.page !== "dashboard") return;
    try { renderDashboard(); } catch (e) {}
  }, 5000);
}

async function instanceAction(name, act) {
  try {
    if (act === "install") {
      const t = await api(`/api/instances/${name}/install`, { method: "POST" });
      toast("安装任务已启动");
      openTaskModal(t.id);
    } else if (act === "stop" || act === "restart") {
      const t = await api(`/api/instances/${name}/${act}`, { method: "POST" });
      openTaskModal(t.id);
    } else {
      await api(`/api/instances/${name}/${act}`, { method: "POST" });
      toast("已执行");
      setTimeout(renderDashboard, 800);
    }
  } catch (e) { toast(e.message, false); }
}

/* ---------- 实例详情 ---------- */
async function renderInstance(name, tab) {
  tab = tab || "console";
  const [inst, tmpl] = await Promise.all([
    api(`/api/instances/${name}`),
    api("/api/templates"),
  ]);
  S.instances = (await api("/api/instances"));
  const t = tmpl.find(x => x.id === inst.template);
  const running = inst.status && inst.status.active_state === "active";
  const tabs = [["console", "控制台"], ["config", "配置"], ["backups", "备份"], ["schedules", "计划任务"], ["settings", "设置"]];
  const tabHtml = tabs.map(([k, label]) =>
    `<div class="tab ${tab === k ? "active" : ""}" onclick="nav('instance','${esc(name)}/${k}')">${label}</div>`).join("");

  let body = "";
  if (tab === "console") body = consoleTab(inst, running);
  else if (tab === "config") body = `<div id="tabBody" class="card">加载中...</div>`;
  else if (tab === "backups") body = `<div id="tabBody" class="card">加载中...</div>`;
  else if (tab === "schedules") body = `<div id="tabBody" class="card">加载中...</div>`;
  else if (tab === "settings") body = settingsTab(inst, t);

  // 竞态守卫：await 期间用户可能已跳转，过期渲染直接丢弃
  if (S.route.page !== "instance" || S.route.arg !== name) return;
  renderLayout(`
    <div class="page-title">
      <span class="dot ${dotClass(inst.status)}"></span> ${esc(inst.display_name)}
      <span class="meta" style="font-size:12px;color:var(--text-dim)">${esc(inst.template_name)}</span>
      ${statusBadge(inst.status, inst.installed)}
      <span class="spacer"></span>
      ${!inst.installed ? `<button class="primary small" id="iInstall">安装游戏服务端</button>` : ""}
      ${inst.installed && !running ? `<button class="primary small" id="iStart">启动</button>` : ""}
      ${running ? `<button class="small" id="iRestart">重启</button><button class="small" id="iStop">停止</button>` : ""}
      ${inst.installed ? `<button class="small" id="iUpdate">更新</button>` : ""}
    </div>
    <div class="tabs">${tabHtml}</div>
    ${body}`);

  const bind = (id, fn) => { const el = document.getElementById(id); if (el) el.onclick = fn; };
  bind("iInstall", () => instanceAction(name, "install"));
  bind("iStart", () => instanceAction(name, "start"));
  bind("iStop", () => instanceAction(name, "stop"));
  bind("iRestart", () => instanceAction(name, "restart"));
  bind("iUpdate", async () => {
    if (!confirm("更新会短暂停服，继续？")) return;
    try {
      const task = await api(`/api/instances/${name}/update`, { method: "POST" });
      openTaskModal(task.id);
    } catch (e) { toast(e.message, false); }
  });

  if (tab === "console") initConsole(inst, running);
  if (tab === "config") initConfigTab(inst, t);
  if (tab === "backups") initBackupsTab(inst);
  if (tab === "schedules") initSchedulesTab(inst);
  if (tab === "settings") initSettingsTab(inst, t);
}

/* 控制台 */
function consoleTab(inst, running) {
  return `
  <div class="card">
    <div class="console" id="console"><div class="dim">加载日志...</div></div>
    <div class="cmd-row">
      <input id="cmdInput" placeholder="${inst.has_rcon ? "输入 RCON 命令回车发送，如: ShowPlayers / Broadcast xxx / Save / Info" : "该游戏不支持 RCON，仅可查看日志"}" ${inst.has_rcon ? "" : "disabled"}>
      <button id="cmdSend" ${inst.has_rcon ? "" : "disabled"}>发送</button>
    </div>
    ${inst.has_rcon ? `<div class="row mt">
      <span class="hint">快捷:</span>
      <button class="small" data-cmd="ShowPlayers">在线玩家</button>
      <button class="small" data-cmd="Info">服务器信息</button>
      <button class="small" data-cmd="Save">立即存档</button>
      <button class="small" onclick="promptBroadcast()">广播…</button>
    </div>` : ""}
  </div>`;
}
window.promptBroadcast = function () {
  const name = S.route.arg;
  const msg = prompt("广播内容（空格会自动转为下划线）:");
  if (msg) sendCommand(name, "Broadcast " + msg.replace(/\s+/g, "_"));
};
function initConsole(inst, running) {
  const box = document.getElementById("console");
  box.innerHTML = "";
  let first = true;
  const es = new EventSource(`/api/instances/${inst.name}/console/stream`);
  S.es = es;
  es.onmessage = e => {
    const nearBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
    const d = document.createElement("div");
    d.textContent = e.data;
    box.appendChild(d);
    while (box.childNodes.length > 2000) box.removeChild(box.firstChild);
    if (nearBottom || first) box.scrollTop = box.scrollHeight;
  };
  es.onerror = () => { /* 自动重连由浏览器处理 */ };
  setTimeout(() => { box.scrollTop = box.scrollHeight; first = false; }, 600);

  const input = document.getElementById("cmdInput");
  const send = () => {
    const v = input.value.trim();
    if (v) { sendCommand(inst.name, v); input.value = ""; }
  };
  document.getElementById("cmdSend").onclick = send;
  input.onkeydown = e => { if (e.key === "Enter") send(); };
  document.querySelectorAll("[data-cmd]").forEach(b => b.onclick = () => sendCommand(inst.name, b.dataset.cmd));
}
async function sendCommand(name, cmd) {
  try {
    const r = await api(`/api/instances/${name}/command`, { method: "POST", body: { command: cmd } });
    toast("响应: " + (r.response || "(空)"));
  } catch (e) { toast(e.message, false); }
}

/* 配置 */
async function initConfigTab(inst, tmpl) {
  const body = document.getElementById("tabBody");
  const specs = (tmpl && tmpl.configs) || [];
  if (!specs.length) {
    body.innerHTML = `<div class="empty">该游戏没有声明式配置文件，参数请通过「设置 → 启动参数」调整</div>`;
    return;
  }
  const sel = `<label>配置文件</label><select id="cfgSel">${specs.map(s =>
    `<option value="${esc(s.path)}">${esc(s.label || s.path)}</option>`).join("")}</select><div id="cfgBody" class="mt"></div>`;
  body.innerHTML = sel;
  const load = async (path) => {
    const cb = document.getElementById("cfgBody");
    cb.innerHTML = "加载中...";
    let data;
    try {
      data = await api(`/api/instances/${inst.name}/config?path=${encodeURIComponent(path)}`);
    } catch (e) { cb.innerHTML = `<div class="empty">${esc(e.message)}</div>`; return; }
    if (data.format === "raw" || !data.schema || !data.schema.length) {
      cb.innerHTML = `
        <textarea id="cfgRaw" rows="20">${esc(data.raw)}</textarea>
        <div class="form-actions"><button class="primary" id="cfgSaveRaw">保存</button>
        <span class="hint">保存后需重启服务器生效</span></div>`;
      document.getElementById("cfgSaveRaw").onclick = async () => {
        try {
          await api(`/api/instances/${inst.name}/config`, { method: "PUT", body: { path, raw: document.getElementById("cfgRaw").value } });
          toast("已保存");
        } catch (e) { toast(e.message, false); }
      };
      return;
    }
    const vals = data.values || {};
    const fields = data.schema.map(f => {
      let v = vals[f.key];
      if (v === undefined) v = f.default ?? "";
      let input;
      if (f.type === "bool") {
        const truthy = String(v).toLowerCase() === "true" || v === "1";
        input = `<select data-key="${esc(f.key)}"><option value="True" ${truthy ? "selected" : ""}>是</option><option value="False" ${truthy ? "" : "selected"}>否</option></select>`;
      } else if (f.type === "select") {
        input = `<select data-key="${esc(f.key)}">${(f.options || []).map(o =>
          `<option ${String(v) === o ? "selected" : ""}>${esc(o)}</option>`).join("")}</select>`;
      } else {
        const type = f.type === "password" ? "text" : (f.type === "int" || f.type === "float" ? "number" : "text");
        const step = f.type === "float" ? ' step="any"' : "";
        input = `<input type="${type}"${step} data-key="${esc(f.key)}" value="${esc(v)}">`;
      }
      return `<div><label>${esc(f.label || f.key)} <span class="mono" style="color:#5a6b7d">${esc(f.key)}</span></label>${input}</div>`;
    }).join("");
    cb.innerHTML = `<div class="form-row">${fields}</div>
      <div class="form-actions"><button class="primary" id="cfgSave">保存配置</button>
      <span class="hint">保存后需重启服务器生效</span></div>`;
    document.getElementById("cfgSave").onclick = async () => {
      const values = {};
      cb.querySelectorAll("[data-key]").forEach(el => values[el.dataset.key] = el.value);
      try {
        await api(`/api/instances/${inst.name}/config`, { method: "PUT", body: { path, values } });
        toast("已保存");
      } catch (e) { toast(e.message, false); }
    };
  };
  document.getElementById("cfgSel").onchange = e => load(e.target.value);
  load(specs[0].path);
}

/* 备份 */
async function initBackupsTab(inst) {
  const body = document.getElementById("tabBody");
  const load = async () => {
    let list;
    try { list = await api(`/api/instances/${inst.name}/backups`); } catch (e) { body.innerHTML = esc(e.message); return; }
    const rows = (list || []).map(b => `<tr>
      <td class="mono">${esc(b.file)}</td><td>${fmtBytes(b.size)}</td>
      <td>${new Date(b.created).toLocaleString("zh-CN")}</td>
      <td>
        <a href="/api/instances/${inst.name}/backups/${encodeURIComponent(b.file)}/download"><button class="small">下载</button></a>
        <button class="small" data-restore="${esc(b.file)}">恢复</button>
        <button class="small danger" data-del="${esc(b.file)}">删除</button>
      </td></tr>`).join("");
    body.innerHTML = `
      <div class="row" style="margin-bottom:12px">
        <button class="primary small" id="mkBackup">立即备份</button>
        <span class="hint">备份内容：存档与配置（tar.gz），保留最近 10 份；建议同时配置计划任务自动备份</span>
      </div>
      ${list && list.length ? `<table><tr><th>文件</th><th>大小</th><th>时间</th><th>操作</th></tr>${rows}</table>` : '<div class="empty">暂无备份</div>'}`;
    document.getElementById("mkBackup").onclick = async () => {
      try {
        const t = await api(`/api/instances/${inst.name}/backups`, { method: "POST" });
        openTaskModal(t.id, load);
      } catch (e) { toast(e.message, false); }
    };
    body.querySelectorAll("[data-restore]").forEach(b => b.onclick = async () => {
      if (!confirm(`确定从 ${b.dataset.restore} 恢复？当前存档将被覆盖，服务器会重启。`)) return;
      try {
        const t = await api(`/api/instances/${inst.name}/backups/${encodeURIComponent(b.dataset.restore)}/restore`, { method: "POST" });
        openTaskModal(t.id, load);
      } catch (e) { toast(e.message, false); }
    });
    body.querySelectorAll("[data-del]").forEach(b => b.onclick = async () => {
      if (!confirm(`删除备份 ${b.dataset.del}？`)) return;
      try {
        await api(`/api/instances/${inst.name}/backups/${encodeURIComponent(b.dataset.del)}`, { method: "DELETE" });
        toast("已删除"); load();
      } catch (e) { toast(e.message, false); }
    });
  };
  load();
}

/* 计划任务 */
async function initSchedulesTab(inst) {
  const body = document.getElementById("tabBody");
  const kindName = { restart: "重启", backup: "备份", update: "更新" };
  const load = async () => {
    const cur = await api(`/api/instances/${inst.name}`);
    const rows = (cur.schedules || []).map(s => `<tr>
      <td>${kindName[s.kind] || esc(s.kind)}</td>
      <td>${s.type === "daily" ? `每天 ${esc(s.time)}` : `每 ${s.hours} 小时`}</td>
      <td>${s.last_run && s.last_run.startsWith("0001") ? "从未" : (s.last_run ? new Date(s.last_run).toLocaleString("zh-CN") : "从未")}</td>
      <td><button class="small" data-toggle="${esc(s.id)}" data-en="${s.enabled ? 0 : 1}">${s.enabled ? "禁用" : "启用"}</button>
          <button class="small danger" data-del="${esc(s.id)}">删除</button></td></tr>`).join("");
    body.innerHTML = `
      <div class="card" style="padding:14px;margin-bottom:14px">
        <div class="form-row">
          <div><label>任务类型</label><select id="schKind">
            <option value="restart">定时重启（推荐 Palworld，释放内存）</option>
            <option value="backup">定时备份</option>
            <option value="update">定时更新游戏</option></select></div>
          <div><label>方式</label><select id="schType">
            <option value="daily">每天固定时间</option><option value="interval">按间隔</option></select></div>
          <div class="daily-only"><label>时间</label><input id="schTime" type="time" value="05:00"></div>
          <div class="interval-only" style="display:none"><label>间隔（小时）</label><input id="schHours" type="number" value="12" min="1"></div>
          <div class="interval-only" style="display:none"><label>备份保留份数</label><input id="schRetention" type="number" value="10" min="1"></div>
        </div>
        <div class="form-actions"><button class="primary small" id="schAdd">添加计划任务</button></div>
      </div>
      ${rows ? `<table><tr><th>任务</th><th>计划</th><th>上次执行</th><th>操作</th></tr>${rows}</table>` : '<div class="empty">暂无计划任务</div>'}`;
    const syncVis = () => {
      const daily = document.getElementById("schType").value === "daily";
      body.querySelectorAll(".daily-only").forEach(e => e.style.display = daily ? "" : "none");
      body.querySelectorAll(".interval-only").forEach(e => e.style.display = daily ? "none" : "");
      document.querySelector(".interval-only:nth-of-type(5)") &&
        (document.querySelectorAll(".interval-only")[1].style.display = (!daily && document.getElementById("schKind").value === "backup") ? "" : "none");
    };
    document.getElementById("schType").onchange = syncVis;
    document.getElementById("schKind").onchange = syncVis;
    document.getElementById("schAdd").onclick = async () => {
      const type = document.getElementById("schType").value;
      const payload = {
        kind: document.getElementById("schKind").value, type,
        time: document.getElementById("schTime").value,
        hours: parseInt(document.getElementById("schHours").value || "0"),
        retention: parseInt(document.getElementById("schRetention").value || "10"),
      };
      try { await api(`/api/instances/${inst.name}/schedules`, { method: "POST", body: payload }); toast("已添加"); load(); }
      catch (e) { toast(e.message, false); }
    };
    body.querySelectorAll("[data-toggle]").forEach(b => b.onclick = async () => {
      try { await api(`/api/instances/${inst.name}/schedules/${b.dataset.toggle}`, { method: "PUT", body: { enabled: b.dataset.en === "1" } }); load(); }
      catch (e) { toast(e.message, false); }
    });
    body.querySelectorAll("[data-del]").forEach(b => b.onclick = async () => {
      if (!confirm("删除该计划任务？")) return;
      try { await api(`/api/instances/${inst.name}/schedules/${b.dataset.del}`, { method: "DELETE" }); load(); }
      catch (e) { toast(e.message, false); }
    });
  };
  load();
}

/* 设置 */
function settingsTab(inst, tmpl) {
  const ports = (tmpl ? tmpl.ports : []).map(p =>
    `<tr><td>${esc(p.key)}</td><td class="mono">${inst.ports[p.key] || "-"}</td><td>${esc(p.proto)}</td><td>${esc(p.desc)}</td></tr>`).join("");
  return `
  <div class="card">
    <h3>基本</h3>
    <div class="form-row">
      <div><label>显示名称</label><input id="setName" value="${esc(inst.display_name)}"></div>
      <div><label>安装目录</label><input value="${esc(inst.dir)}" disabled></div>
    </div>
    <label>启动参数（空格分隔，修改后重启生效）</label>
    <input id="setArgs" class="mono" value="${esc((inst.args || []).join(" "))}">
    <div class="form-actions"><button class="primary small" id="setSave">保存</button></div>
  </div>
  <div class="card">
    <h3>端口</h3>
    <table><tr><th>用途</th><th>端口</th><th>协议</th><th>说明</th></tr>${ports}</table>
    <div class="hint">端口在创建实例时设定；如需修改请删除实例重建。公网端口记得在防火墙/云安全组放行。</div>
  </div>
  <div class="card">
    <h3>systemd 服务</h3>
    <div class="mono">gspanel-${esc(inst.name)}.service（开机自启，崩溃自动拉起）</div>
  </div>
  <div class="card">
    <h3 style="color:var(--red)">危险操作</h3>
    <button class="danger small" id="instDelete">删除实例</button>
    <label><input type="checkbox" id="delFiles" style="width:auto"> 同时删除游戏文件与备份（不可恢复）</label>
  </div>`;
}
function initSettingsTab(inst, tmpl) {
  document.getElementById("setSave").onclick = async () => {
    const args = document.getElementById("setArgs").value.trim().split(/\s+/).filter(Boolean);
    try {
      await api(`/api/instances/${inst.name}/settings`, { method: "PUT", body: { display_name: document.getElementById("setName").value, args } });
      toast("已保存");
    } catch (e) { toast(e.message, false); }
  };
  document.getElementById("instDelete").onclick = async () => {
    if (!confirm(`确定删除实例 ${inst.name}？`)) return;
    try {
      await api(`/api/instances/${inst.name}?delete_files=${document.getElementById("delFiles").checked}`, { method: "DELETE" });
      toast("已删除");
      nav("dashboard");
    } catch (e) { toast(e.message, false); }
  };
}

/* ---------- 新建实例 ---------- */
async function renderNew() {
  const tmpls = await api("/api/templates");
  if (S.route.page !== "new") return; // 竞态守卫
  renderLayout(`
    <div class="page-title">新建实例</div>
    <div class="card">
      <h3>模板库</h3>
      <div class="row">
        <input id="tplUrl" placeholder="粘贴模板 JSON 链接，从 URL 导入新游戏模板" style="flex:1;min-width:260px">
        <button class="small" id="tplImport">导入模板</button>
      </div>
      <div class="hint">模板描述一个游戏的 AppID、启动命令、端口、配置格式与备份路径。也可手动将 JSON 放入 /root/gspanel/templates/ 后重启面板。</div>
    </div>
    <div class="card">
      <h3>1. 选择游戏模板</h3>
      <div class="grid" style="grid-template-columns:repeat(auto-fill,minmax(240px,1fr))" id="tplList">
        ${tmpls.map(t => `<div class="tpl-item" data-id="${esc(t.id)}">
          <div class="t-name">${esc(t.name)}</div>
          <div class="t-desc">${esc(t.description)}</div>
          <div class="t-meta">AppID ${t.steam_app_id} · ${t.ports.map(p => p.default + "/" + p.proto).join(" ")}</div>
        </div>`).join("")}
      </div>
    </div>
    <div class="card" id="newForm" style="display:none">
      <h3>2. 实例参数</h3>
      <div class="form-row">
        <div><label>实例标识（小写字母/数字/中划线）</label><input id="newName" placeholder="如 palworld-1"></div>
        <div><label>显示名称</label><input id="newDisplay" placeholder="如 我的帕鲁服务器"></div>
      </div>
      <div class="form-row" id="newPorts"></div>
      <div class="hint" id="tplNotes"></div>
      <div class="form-actions">
        <button class="primary" id="newCreate">创建并安装</button>
        <button id="newCreateOnly">仅创建（稍后手动安装）</button>
      </div>
    </div>`);
  let sel = null;
  document.getElementById("tplImport").onclick = async () => {
    const url = document.getElementById("tplUrl").value.trim();
    if (!url) return;
    try {
      const r = await api("/api/templates/import", { method: "POST", body: { url } });
      toast(`模板「${r.template.name}」导入成功`);
      renderNew();
    } catch (e) { toast(e.message, false); }
  };
  document.querySelectorAll(".tpl-item").forEach(el => el.onclick = () => {
    document.querySelectorAll(".tpl-item").forEach(x => x.classList.remove("sel"));
    el.classList.add("sel");
    sel = tmpls.find(t => t.id === el.dataset.id);
    document.getElementById("newForm").style.display = "";
    document.getElementById("newPorts").innerHTML = sel.ports.map(p =>
      `<div><label>${esc(p.desc)}（${p.proto}）</label><input type="number" data-port="${esc(p.key)}" value="${p.default}"></div>`).join("");
    document.getElementById("tplNotes").textContent = sel.notes ? "提示: " + sel.notes : "";
  });
  const create = async (install) => {
    if (!sel) return;
    const ports = {};
    document.querySelectorAll("[data-port]").forEach(el => ports[el.dataset.port] = parseInt(el.value));
    try {
      const inst = await api("/api/instances", { method: "POST", body: {
        template: sel.id,
        name: document.getElementById("newName").value.trim(),
        display_name: document.getElementById("newDisplay").value.trim(),
        ports,
      }});
      if (install) {
        const t = await api(`/api/instances/${inst.name}/install`, { method: "POST" });
        toast("安装任务已启动（下载数 GB，请耐心）");
        openTaskModal(t.id, () => nav("instance", inst.name));
      } else {
        nav("instance", inst.name);
      }
    } catch (e) { toast(e.message, false); }
  };
  document.getElementById("newCreate").onclick = () => create(true);
  document.getElementById("newCreateOnly").onclick = () => create(false);
}

/* ---------- 任务 ---------- */
async function renderTasks() {
  const list = await api("/api/tasks");
  if (S.route.page !== "tasks") return; // 竞态守卫
  const badge = s => ({ running: "blue", success: "green", failed: "red", canceled: "yellow" }[s] || "");
  const rows = list.length ? list.map(t => `<tr>
    <td class="mono">${esc(t.id)}</td><td>${esc(t.desc)}</td>
    <td><span class="badge ${badge(t.status)}">${esc(t.status)}</span></td>
    <td>${new Date(t.created_at).toLocaleString("zh-CN")}</td>
    <td><button class="small" data-log="${esc(t.id)}">日志</button>
    ${t.status === "running" ? `<button class="small danger" data-cancel="${esc(t.id)}">取消</button>` : ""}</td></tr>`).join("") : "";
  renderLayout(`<div class="page-title">后台任务</div>
    <div class="card">${rows ? `<table><tr><th>ID</th><th>任务</th><th>状态</th><th>时间</th><th>操作</th></tr>${rows}</table>` : '<div class="empty">暂无任务</div>'}</div>`);
  document.querySelectorAll("[data-log]").forEach(b => b.onclick = () => openTaskModal(b.dataset.log));
  document.querySelectorAll("[data-cancel]").forEach(b => b.onclick = async () => {
    try { await api(`/api/tasks/${b.dataset.cancel}/cancel`, { method: "POST" }); toast("已取消"); renderTasks(); }
    catch (e) { toast(e.message, false); }
  });
  addPoll(() => { if (S.route.page === "tasks") renderTasks(); }, 5000);
}

/* 任务日志弹窗（SSE 实时） */
async function openTaskModal(id, onDone) {
  closeModal();
  const mask = document.createElement("div");
  mask.className = "modal-mask";
  mask.id = "modalMask";
  mask.innerHTML = `<div class="modal">
    <h3>任务日志 <span class="mono" style="color:var(--text-dim);font-size:11px">${esc(id)}</span></h3>
    <div class="console" id="taskLog" style="height:380px"></div>
    <div class="form-actions"><button id="modalClose">关闭</button></div>
  </div>`;
  document.body.appendChild(mask);
  document.getElementById("modalClose").onclick = () => { closeModal(); if (onDone) onDone(); };
  mask.onclick = e => { if (e.target === mask) { closeModal(); if (onDone) onDone(); } };
  const box = document.getElementById("taskLog");
  const append = text => {
    text.split("\n").forEach(line => {
      const d = document.createElement("div");
      d.textContent = line;
      box.appendChild(d);
    });
    box.scrollTop = box.scrollHeight;
  };
  try {
    const data = await api(`/api/tasks/${id}`);
    if (data.log) append(data.log.replace(/\n$/, ""));
    if (data.task.status !== "running") return;
  } catch (e) { append("加载日志失败: " + e.message); return; }

  const es = new EventSource(`/api/tasks/${id}/stream`);
  S.taskStream = es;
  es.onmessage = e => append(e.data);
  es.addEventListener("done", () => {
    es.close();
    S.taskStream = null;
    const h = document.querySelector(".modal h3");
    if (h) h.innerHTML += ' <span class="badge green">结束</span>';
    if (onDone) onDone();
  });
}
function closeModal() {
  if (S.taskStream) { S.taskStream.close(); S.taskStream = null; }
  const m = document.getElementById("modalMask");
  if (m) m.remove();
}

/* ---------- 设置 / 环境 ---------- */
async function renderSettings() {
  const sys = await api("/api/system");
  if (S.route.page !== "settings") return; // 竞态守卫
  const depRow = (k, label, btn) => `<tr><td>${label}</td>
    <td>${sys.deps[k] ? '<span class="badge green">已安装</span>' : '<span class="badge red">未安装</span>'}</td>
    <td>${!sys.deps[k] && btn ? btn : ""}</td></tr>`;
  renderLayout(`
    <div class="page-title">设置 / 环境</div>
    <div class="card">
      <h3>运行环境（安装游戏前需就绪）</h3>
      <table>
        ${depRow("lib32gcc-s1", "lib32gcc-s1（32位运行库）", '<button class="small primary" id="depBtn">一键安装依赖</button>')}
        ${depRow("lib32stdc++6", "lib32stdc++6（32位运行库）", "")}
        ${depRow("steamcmd", "steamcmd（游戏下载工具）", '<button class="small primary" id="scmBtn">安装 steamcmd</button>')}
      </table>
      <div class="hint">面板地址: <span class="mono">${esc(sys.bind_addr)}</span>（修改请编辑 ${"/root/gspanel/data/config.json"} 后重启面板）</div>
    </div>
    <div class="card">
      <h3>公网地址（仪表盘分享给朋友的连接地址）</h3>
      <div class="form-row">
        <div><label>当前生效</label><input value="${esc(sys.public_ip || "获取失败")}" disabled></div>
        <div><label>手动覆盖（IP 或域名，留空=自动探测）</label><input id="pubIpOverride" value="${esc(sys.public_ip_override || "")}" placeholder="自动探测"></div>
      </div>
      <div class="form-actions"><button class="primary small" id="pubIpSave">保存</button></div>
      <div class="hint">自动探测优先读云厂商元数据，失败则走公网探测服务；使用 DDNS/域名时建议在此手动覆盖。</div>
    </div>
    <div class="card">
      <h3>修改管理员密码</h3>
      <div class="form-row">
        <div><label>原密码</label><input type="password" id="oldPw"></div>
        <div><label>新密码（至少 8 位）</label><input type="password" id="newPw"></div>
      </div>
      <div class="form-actions"><button class="primary small" id="pwSave">修改密码</button></div>
    </div>
    <div class="card">
      <h3>扩展新游戏</h3>
      <div class="hint">将模板 JSON 放入 <span class="mono">/root/gspanel/templates/</span> 并重启面板即可。字段参照内置模板：
      steam_app_id、executable、default_args、ports、configs（option-settings/kv/raw 三种格式）、rcon、backup_paths。</div>
    </div>`);
  const depBtn = document.getElementById("depBtn");
  if (depBtn) depBtn.onclick = async () => {
    try { const t = await api("/api/setup/deps", { method: "POST" }); openTaskModal(t.id, renderSettings); }
    catch (e) { toast(e.message, false); }
  };
  const scmBtn = document.getElementById("scmBtn");
  if (scmBtn) scmBtn.onclick = async () => {
    try { const t = await api("/api/setup/steamcmd", { method: "POST" }); openTaskModal(t.id, renderSettings); }
    catch (e) { toast(e.message, false); }
  };
  document.getElementById("pubIpSave").onclick = async () => {
    try {
      const r = await api("/api/settings/public-ip", { method: "POST", body: { public_ip: document.getElementById("pubIpOverride").value.trim() } });
      toast("已保存，生效地址: " + (r.public_ip || "获取失败"));
      renderSettings();
    } catch (e) { toast(e.message, false); }
  };
  document.getElementById("pwSave").onclick = async () => {
    try {
      await api("/api/settings/password", { method: "POST", body: {
        old_password: document.getElementById("oldPw").value,
        new_password: document.getElementById("newPw").value,
      }});
      toast("密码已修改");
    } catch (e) { toast(e.message, false); }
  };
}

/* ---------- 主渲染 ---------- */
async function render() {
  clearTimers();
  parseRoute();
  if (!S.authed) {
    try { await api("/api/me"); S.authed = true; }
    catch { renderLogin(); return; }
  }
  try {
    S.instances = await api("/api/instances");
  } catch (e) { renderLogin(); return; }
  const { page, arg, arg2 } = S.route;
  try {
    if (page === "dashboard") await renderDashboard();
    else if (page === "instance" && arg) await renderInstance(arg, arg2);
    else if (page === "new") await renderNew();
    else if (page === "tasks") await renderTasks();
    else if (page === "settings") await renderSettings();
    else await renderDashboard();
  } catch (e) {
    if (e.message !== "未登录") toast(e.message, false);
  }
}
render();
