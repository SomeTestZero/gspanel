package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- HTTP API 层 ----------

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		jsonError(w, http.StatusBadRequest, "请求体 JSON 解析失败: "+err.Error())
		return false
	}
	return true
}

func (sv *Server) getInstance(w http.ResponseWriter, r *http.Request) *Instance {
	name := r.PathValue("name")
	sv.state.mu.RLock()
	inst := sv.state.Instances[name]
	sv.state.mu.RUnlock()
	if inst == nil {
		jsonError(w, http.StatusNotFound, "实例不存在")
		return nil
	}
	return inst
}

func (sv *Server) templateOf(inst *Instance) *GameTemplate {
	return sv.getTemplate(inst.Template)
}

// instanceView 实例的 API 视图（附带模板信息与实时状态）
func (sv *Server) instanceView(inst *Instance) map[string]any {
	tmpl := sv.templateOf(inst)
	st := serviceStatus(inst)
	view := map[string]any{
		"name":          inst.Name,
		"template":      inst.Template,
		"template_name": "",
		"display_name":  inst.DisplayName,
		"dir":           inst.Dir,
		"ports":         inst.Ports,
		"args":          inst.Args,
		"installed":     inst.Installed,
		"auto_update":   inst.AutoUpdate,
		"created_at":    inst.CreatedAt,
		"schedules":     inst.Schedules,
		"status":        st,
		"cpu_percent":   sv.monitor.InstanceCPU(inst.Name),
	}
	if tmpl != nil {
		view["template_name"] = tmpl.Name
		view["has_rcon"] = tmpl.RCON != nil
		view["stop_mode"] = tmpl.StopMode
		view["console_buttons"] = tmpl.ConsoleButtons
		var pp []map[string]any
		for _, p := range tmpl.Ports {
			if p.Public {
				pp = append(pp, map[string]any{
					"key": p.Key, "proto": p.Proto, "desc": p.Desc, "port": inst.Ports[p.Key],
				})
			}
		}
		view["public_ports"] = pp
	}
	return view
}

func (sv *Server) registerRoutes(mux *http.ServeMux) {
	// 静态资源（登录页无需鉴权，API 全部鉴权）；no-cache 保证前端更新后浏览器立即生效
	staticFS, _ := fs.Sub(embedded, "static")
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
	}))

	mux.HandleFunc("POST /api/login", sv.handleLogin)
	mux.HandleFunc("POST /api/logout", sv.handleLogout)
	mux.HandleFunc("GET /api/me", sv.auth(func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]any{"ok": true})
	}))

	mux.HandleFunc("GET /api/system", sv.auth(sv.handleSystem))
	mux.HandleFunc("GET /api/events", sv.auth(sv.handleEvents))
	mux.HandleFunc("POST /api/setup/steamcmd", sv.auth(sv.handleSetupSteamcmd))
	mux.HandleFunc("POST /api/setup/deps", sv.auth(sv.handleSetupDeps))
	mux.HandleFunc("POST /api/settings/password", sv.auth(sv.handleChangePassword))
	mux.HandleFunc("POST /api/settings/public-ip", sv.auth(sv.handleSetPublicIP))

	mux.HandleFunc("GET /api/templates", sv.auth(sv.handleTemplates))
	mux.HandleFunc("POST /api/templates/import", sv.auth(sv.handleImportTemplate))

	mux.HandleFunc("GET /api/instances", sv.auth(sv.handleListInstances))
	mux.HandleFunc("POST /api/instances", sv.auth(sv.handleCreateInstance))
	mux.HandleFunc("GET /api/instances/{name}", sv.auth(func(w http.ResponseWriter, r *http.Request) {
		inst := sv.getInstance(w, r)
		if inst != nil {
			jsonOK(w, sv.instanceView(inst))
		}
	}))
	mux.HandleFunc("DELETE /api/instances/{name}", sv.auth(sv.handleDeleteInstance))
	mux.HandleFunc("POST /api/instances/{name}/install", sv.auth(sv.handleInstallInstance))
	mux.HandleFunc("POST /api/instances/{name}/update", sv.auth(sv.handleUpdateInstance))
	mux.HandleFunc("POST /api/instances/{name}/start", sv.auth(sv.handleStart))
	mux.HandleFunc("POST /api/instances/{name}/stop", sv.auth(sv.handleStop))
	mux.HandleFunc("POST /api/instances/{name}/restart", sv.auth(sv.handleRestart))
	mux.HandleFunc("POST /api/instances/{name}/new-world", sv.auth(sv.handleNewWorld))
	mux.HandleFunc("PUT /api/instances/{name}/settings", sv.auth(sv.handleUpdateSettings))

	mux.HandleFunc("GET /api/instances/{name}/console/stream", sv.auth(sv.handleConsoleStream))
	mux.HandleFunc("POST /api/instances/{name}/command", sv.auth(sv.handleCommand))

	mux.HandleFunc("GET /api/instances/{name}/config", sv.auth(sv.handleReadConfig))
	mux.HandleFunc("PUT /api/instances/{name}/config", sv.auth(sv.handleWriteConfig))

	mux.HandleFunc("GET /api/instances/{name}/backups", sv.auth(sv.handleListBackups))
	mux.HandleFunc("POST /api/instances/{name}/backups", sv.auth(sv.handleCreateBackup))
	mux.HandleFunc("POST /api/instances/{name}/backups/upload", sv.auth(sv.handleUploadBackup))
	mux.HandleFunc("GET /api/instances/{name}/backups/{file}/download", sv.auth(sv.handleDownloadBackup))
	mux.HandleFunc("POST /api/instances/{name}/backups/{file}/restore", sv.auth(sv.handleRestoreBackup))
	mux.HandleFunc("DELETE /api/instances/{name}/backups/{file}", sv.auth(sv.handleDeleteBackup))

	mux.HandleFunc("POST /api/instances/{name}/schedules", sv.auth(sv.handleAddSchedule))
	mux.HandleFunc("PUT /api/instances/{name}/schedules/{id}", sv.auth(sv.handleUpdateSchedule))
	mux.HandleFunc("DELETE /api/instances/{name}/schedules/{id}", sv.auth(sv.handleDeleteSchedule))

	mux.HandleFunc("GET /api/tasks", sv.auth(func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, sv.tasks.List())
	}))
	mux.HandleFunc("GET /api/tasks/{id}", sv.auth(func(w http.ResponseWriter, r *http.Request) {
		t := sv.tasks.Get(r.PathValue("id"))
		if t == nil {
			jsonError(w, http.StatusNotFound, "任务不存在")
			return
		}
		jsonOK(w, map[string]any{"task": t, "log": string(t.Log())})
	}))
	mux.HandleFunc("GET /api/tasks/{id}/stream", sv.auth(sv.handleTaskStream))
	mux.HandleFunc("POST /api/tasks/{id}/cancel", sv.auth(func(w http.ResponseWriter, r *http.Request) {
		t := sv.tasks.Get(r.PathValue("id"))
		if t == nil {
			jsonError(w, http.StatusNotFound, "任务不存在")
			return
		}
		t.Cancel()
		jsonOK(w, map[string]any{"ok": true})
	}))
}

// ---------- 认证 ----------

func (sv *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !sv.state.allowLogin(ip) {
		jsonError(w, http.StatusTooManyRequests, "失败次数过多，请 10 分钟后再试")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !sv.state.CheckPassword(req.Password) {
		sv.state.recordLoginFail(ip)
		jsonError(w, http.StatusUnauthorized, "密码错误")
		return
	}
	token := sv.state.newSession()
	setSessionCookie(w, token)
	jsonOK(w, map[string]any{"ok": true, "token": token})
}

func (sv *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := sv.tokenFrom(r); tok != "" {
		sv.state.dropSession(tok)
	}
	clearSessionCookie(w)
	jsonOK(w, map[string]any{"ok": true})
}

// ---------- 系统与环境 ----------

func (sv *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	sv.state.mu.RLock()
	override := sv.state.PublicIP
	sv.state.mu.RUnlock()
	jsonOK(w, map[string]any{
		"stats":              ReadSystemStats(),
		"deps":               depsStatus(),
		"bind_addr":          sv.state.BindAddr(),
		"base_dir":           BaseDir,
		"public_ip":          sv.publicIP(),
		"public_ip_override": override,
		"version":            "1.0.0",
	})
}

// handleEvents 事件日志（时间线）：?instance=<名> 过滤，?limit=<n> 条数（默认 200，上限 eventKeep）
func (sv *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, eventKeep)
	}
	jsonOK(w, map[string]any{"events": sv.events.List(r.URL.Query().Get("instance"), limit)})
}

// handleSetPublicIP 手动覆盖公网地址（空字符串 = 恢复自动探测）
func (sv *Server) handleSetPublicIP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PublicIP string `json:"public_ip"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	v := strings.TrimSpace(req.PublicIP)
	if v != "" && !validPublicAddr(v) {
		jsonError(w, http.StatusBadRequest, "须为合法 IP 或域名")
		return
	}
	sv.state.mu.Lock()
	sv.state.PublicIP = v
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sv.ipMu.Lock()
	sv.ipCache, sv.ipCacheAt = "", time.Time{}
	sv.ipMu.Unlock()
	if v == "" {
		sv.events.Add("", "network", "公网地址恢复为自动探测")
	} else {
		sv.events.Add("", "network", "手动设置公网地址为 %s", v)
	}
	jsonOK(w, map[string]any{"ok": true, "public_ip": sv.publicIP()})
}

func (sv *Server) handleSetupSteamcmd(w http.ResponseWriter, r *http.Request) {
	t := sv.tasks.Run("setup-steamcmd", "", "安装 steamcmd", func(ctx context.Context, log io.Writer, task *Task) error {
		return installSteamcmd(ctx, log)
	})
	jsonOK(w, t)
}

func (sv *Server) handleSetupDeps(w http.ResponseWriter, r *http.Request) {
	t := sv.tasks.Run("setup-deps", "", "安装系统依赖 (lib32gcc-s1/lib32stdc++6)", func(ctx context.Context, log io.Writer, task *Task) error {
		return installDeps(ctx, log)
	})
	jsonOK(w, t)
}

func (sv *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !sv.state.CheckPassword(req.OldPassword) {
		jsonError(w, http.StatusForbidden, "原密码错误")
		return
	}
	if len(req.NewPassword) < 8 {
		jsonError(w, http.StatusBadRequest, "新密码至少 8 位")
		return
	}
	if err := sv.state.SetPassword(req.NewPassword); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sv.events.Add("", "password", "修改面板管理员密码")
	jsonOK(w, map[string]any{"ok": true})
}

// ---------- 模板 ----------

func (sv *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	sv.tplMu.RLock()
	out := make([]*GameTemplate, 0, len(sv.templates))
	for _, t := range sv.templates {
		out = append(out, t)
	}
	sv.tplMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	jsonOK(w, out)
}

// handleImportTemplate 从 URL 导入游戏模板
func (sv *Server) handleImportTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	t, err := sv.importTemplateFromURL(req.URL)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "template": t})
}

// ---------- 实例 ----------

func (sv *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	sv.state.mu.RLock()
	insts := make([]*Instance, 0, len(sv.state.Instances))
	for _, inst := range sv.state.Instances {
		insts = append(insts, inst)
	}
	sv.state.mu.RUnlock()
	sort.Slice(insts, func(i, j int) bool { return insts[i].CreatedAt.Before(insts[j].CreatedAt) })
	out := make([]map[string]any, 0, len(insts))
	for _, inst := range insts {
		out = append(out, sv.instanceView(inst))
	}
	jsonOK(w, out)
}

func (sv *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Template    string         `json:"template"`
		Name        string         `json:"name"`
		DisplayName string         `json:"display_name"`
		Ports       map[string]int `json:"ports"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	tmpl := sv.getTemplate(req.Template)
	if tmpl == nil {
		jsonError(w, http.StatusBadRequest, "未知游戏模板")
		return
	}
	if req.Ports == nil {
		req.Ports = map[string]int{}
	}
	inst, err := sv.createInstance(tmpl, req.Name, req.DisplayName, req.Ports)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	sv.events.Add(inst.Name, "instance", "创建实例（模板 %s）", tmpl.Name)
	jsonOK(w, sv.instanceView(inst))
}

func (sv *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	deleteFiles := r.URL.Query().Get("delete_files") == "true"
	if err := sv.deleteInstance(inst, deleteFiles); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if deleteFiles {
		sv.events.Add(inst.Name, "instance", "删除实例（连同游戏文件与备份）")
	} else {
		sv.events.Add(inst.Name, "instance", "删除实例（保留文件）")
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (sv *Server) handleInstallInstance(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	t := sv.installInstance(inst, sv.templateOf(inst))
	jsonOK(w, t)
}

func (sv *Server) handleUpdateInstance(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	t := sv.tasks.Run("update", inst.Name, "更新 "+tmpl.Name, func(ctx context.Context, log io.Writer, task *Task) error {
		return sv.updateInstance(ctx, log, inst, tmpl)
	})
	jsonOK(w, t)
}

func (sv *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	if !inst.Installed {
		jsonError(w, http.StatusBadRequest, "实例尚未安装")
		return
	}
	// 启动前写回面板保存的配置（游戏关机时可能覆盖了配置文件）
	if err := sv.applySavedConfig(inst, sv.templateOf(inst)); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 日志过大时截断保留尾部 1MB
	logFile := inst.Dir + "/logs/console.log"
	if fi, err := os.Stat(logFile); err == nil && fi.Size() > 50<<20 {
		truncateLog(logFile, 1<<20)
	}
	if out, err := systemctl("start", unitName(inst)); err != nil {
		jsonError(w, http.StatusInternalServerError, "启动失败: "+out)
		return
	}
	sv.events.Add(inst.Name, "start", "手动启动服务器")
	jsonOK(w, map[string]any{"ok": true})
}

func truncateLog(path string, keep int64) {
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	size, _ := f.Seek(0, io.SeekEnd)
	if size <= keep {
		return
	}
	_, _ = f.Seek(size-keep, io.SeekStart)
	tail, _ := io.ReadAll(f)
	_ = f.Truncate(0)
	_, _ = f.Seek(0, io.SeekStart)
	_, _ = f.Write(tail)
}

func (sv *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	t := sv.tasks.Run("stop", inst.Name, "停止 "+inst.DisplayName, func(ctx context.Context, log io.Writer, task *Task) error {
		return sv.gracefulStop(ctx, log, inst, tmpl)
	})
	jsonOK(w, t)
}

func (sv *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	t := sv.tasks.Run("restart", inst.Name, "重启 "+inst.DisplayName, func(ctx context.Context, log io.Writer, task *Task) error {
		return sv.gracefulRestart(ctx, log, inst, tmpl)
	})
	jsonOK(w, t)
}

func (sv *Server) handleNewWorld(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	if len(tmpl.WorldPaths) == 0 {
		jsonError(w, http.StatusBadRequest, "该游戏模板不支持创建新世界")
		return
	}
	t := sv.tasks.Run("new-world", inst.Name, "创建新世界 "+inst.DisplayName, func(ctx context.Context, log io.Writer, task *Task) error {
		return sv.newWorld(ctx, log, inst, tmpl)
	})
	jsonOK(w, t)
}

func (sv *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	var req struct {
		DisplayName string   `json:"display_name"`
		Args        []string `json:"args"`
		AutoUpdate  *bool    `json:"auto_update"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var changes []string
	sv.state.mu.Lock()
	if req.DisplayName != "" && req.DisplayName != inst.DisplayName {
		inst.DisplayName = req.DisplayName
		changes = append(changes, "显示名称")
	}
	if req.Args != nil && strings.Join(req.Args, " ") != strings.Join(inst.Args, " ") {
		inst.Args = req.Args
		changes = append(changes, "启动参数")
	}
	if req.AutoUpdate != nil && *req.AutoUpdate != inst.AutoUpdate {
		inst.AutoUpdate = *req.AutoUpdate
		changes = append(changes, fmt.Sprintf("自动更新=%v", *req.AutoUpdate))
	}
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(changes) > 0 {
		sv.events.Add(inst.Name, "settings", "修改实例设置：%s", strings.Join(changes, "、"))
	}
	if inst.Installed {
		tmpl := sv.templateOf(inst)
		if err := writeStartScript(inst, tmpl); err != nil {
			jsonError(w, http.StatusInternalServerError, "更新启动脚本失败: "+err.Error())
			return
		}
	}
	jsonOK(w, map[string]any{"ok": true})
}

// ---------- 控制台 ----------

func (sv *Server) handleConsoleStream(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	logFile := inst.Dir + "/logs/console.log"
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 先发送最近 32KB 历史
	var offset int64
	if fi, err := os.Stat(logFile); err == nil {
		size := fi.Size()
		start := int64(0)
		if size > 32<<10 {
			start = size - 32<<10
		}
		if f, err := os.Open(logFile); err == nil {
			_, _ = f.Seek(start, io.SeekStart)
			data, _ := io.ReadAll(f)
			_ = f.Close()
			writeSSE(w, data)
			offset = size
		}
	}
	flusher.Flush()

	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(logFile)
			if err != nil {
				continue
			}
			if fi.Size() < offset { // 日志被截断
				offset = 0
			}
			if fi.Size() == offset {
				continue
			}
			f, err := os.Open(logFile)
			if err != nil {
				continue
			}
			_, _ = f.Seek(offset, io.SeekStart)
			data, _ := io.ReadAll(f)
			_ = f.Close()
			offset = fi.Size()
			if len(data) > 0 {
				writeSSE(w, data)
				flusher.Flush()
			}
		}
	}
}

func writeSSE(w io.Writer, data []byte) {
	// SSE data 行不能含裸换行，逐行输出
	for len(data) > 0 {
		i := 0
		for i < len(data) && data[i] != '\n' {
			i++
		}
		fmt.Fprintf(w, "data: %s\n", data[:i])
		if i < len(data) {
			i++
		}
		data = data[i:]
	}
	fmt.Fprint(w, "\n")
}

func (sv *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	if tmpl.RCON == nil {
		jsonError(w, http.StatusBadRequest, "该游戏模板不支持 RCON")
		return
	}
	var req struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &req) || req.Command == "" {
		jsonError(w, http.StatusBadRequest, "命令不能为空")
		return
	}
	if inst.AdminPassword == "" {
		jsonError(w, http.StatusBadRequest, "实例未配置管理员密码，无法使用 RCON")
		return
	}
	// 模板声明了 REST 映射的命令优先走 REST API（Palworld 等游戏 RCON 会丢响应）
	verb, arg := req.Command, ""
	if i := strings.IndexAny(verb, " \t"); i >= 0 {
		verb, arg = verb[:i], strings.TrimSpace(verb[i+1:])
	}
	if tmpl.RestAPI != nil {
		for name, spec := range tmpl.RestAPI.Commands {
			if strings.EqualFold(name, verb) {
				addr := fmt.Sprintf("127.0.0.1:%d", instancePort(inst, tmpl, tmpl.RestAPI.PortKey))
				resp, err := restExec(addr, inst.AdminPassword, spec, arg)
				if err != nil {
					jsonError(w, http.StatusBadGateway, err.Error())
					return
				}
				jsonOK(w, map[string]any{"response": resp})
				return
			}
		}
	}
	addr := fmt.Sprintf("127.0.0.1:%d", inst.Ports[tmpl.RCON.PortKey])
	resp, err := RconExec(addr, inst.AdminPassword, req.Command)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]any{"response": resp})
}

// ---------- 配置文件 ----------

func (sv *Server) findConfigSpec(tmpl *GameTemplate, path string) *ConfigSpec {
	for i := range tmpl.Configs {
		if tmpl.Configs[i].Path == path {
			return &tmpl.Configs[i]
		}
	}
	return nil
}

func (sv *Server) handleReadConfig(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	path := r.URL.Query().Get("path")
	spec := sv.findConfigSpec(tmpl, path)
	if spec == nil {
		jsonError(w, http.StatusBadRequest, "未知配置文件")
		return
	}
	values, raw, err := readConfigFile(inst.Dir, spec)
	if err != nil {
		if os.IsNotExist(err) {
			jsonError(w, http.StatusNotFound, "配置文件尚未生成（需先安装/启动一次）")
			return
		}
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{
		"values": values,
		"raw":    raw,
		"schema": spec.Schema,
		"format": spec.Format,
	})
}

func (sv *Server) handleWriteConfig(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	var req struct {
		Path   string            `json:"path"`
		Values map[string]string `json:"values"`
		Raw    *string           `json:"raw"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	spec := sv.findConfigSpec(tmpl, req.Path)
	if spec == nil {
		jsonError(w, http.StatusBadRequest, "未知配置文件")
		return
	}
	if err := writeConfigFile(inst.Dir, spec, req.Values, req.Raw); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sv.state.mu.Lock()
	// 记录配置快照：游戏关机会用内存值覆盖配置文件，启动前以此为准写回
	if req.Values != nil {
		if inst.ConfigValues == nil {
			inst.ConfigValues = map[string]map[string]string{}
		}
		inst.ConfigValues[req.Path] = req.Values
	}
	// 同步实例侧的管理员密码（RCON 用）
	if v, ok := req.Values["AdminPassword"]; ok && v != inst.AdminPassword {
		inst.AdminPassword = v
	}
	_ = sv.state.saveLocked()
	sv.state.mu.Unlock()
	if req.Raw != nil {
		sv.events.Add(inst.Name, "config", "修改配置文件 %s（整文）", req.Path)
	} else {
		sv.events.Add(inst.Name, "config", "修改配置文件 %s（%d 项）", req.Path, len(req.Values))
	}
	jsonOK(w, map[string]any{"ok": true})
}

// ---------- 备份 ----------

func (sv *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	list, err := listBackups(inst)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, list)
}

func (sv *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	t := sv.tasks.Run("backup", inst.Name, "备份 "+inst.DisplayName, func(ctx context.Context, log io.Writer, task *Task) error {
		_, err := createBackup(ctx, log, inst, tmpl, 10)
		return err
	})
	jsonOK(w, t)
}

// handleUploadBackup 上传备份包（multipart 字段 file），用于跨服务器迁移存档
func (sv *Server) handleUploadBackup(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		jsonError(w, http.StatusBadRequest, "解析上传失败: "+err.Error())
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		jsonError(w, http.StatusBadRequest, "缺少文件字段 file")
		return
	}
	defer f.Close()
	if err := saveUploadedBackup(inst, hdr.Filename, f); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

func (sv *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	p, err := backupPath(inst, r.PathValue("file"))
	if err != nil {
		jsonError(w, http.StatusNotFound, "备份不存在")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+r.PathValue("file"))
	http.ServeFile(w, r, p)
}

func (sv *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	file := r.PathValue("file")
	t := sv.tasks.Run("restore", inst.Name, "恢复备份 "+file, func(ctx context.Context, log io.Writer, task *Task) error {
		return sv.restoreBackup(ctx, log, inst, tmpl, file)
	})
	jsonOK(w, t)
}

func (sv *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	if err := deleteBackup(inst, r.PathValue("file")); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// ---------- 计划任务 ----------

// scheduleDesc 计划任务的人类可读描述，如「定时备份（每天 04:30）」
func scheduleDesc(sch *Schedule) string {
	kind := map[string]string{"restart": "定时重启", "backup": "定时备份", "update": "定时更新"}[sch.Kind]
	if kind == "" {
		kind = sch.Kind
	}
	if sch.Type == "daily" {
		return kind + "（每天 " + sch.Time + "）"
	}
	return fmt.Sprintf("%s（每 %d 小时）", kind, sch.Hours)
}

func (sv *Server) findSchedule(inst *Instance, id string) *Schedule {
	for _, s := range inst.Schedules {
		if s.ID == id {
			return s
		}
	}
	return nil
}

func (sv *Server) handleAddSchedule(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	var sch Schedule
	if !decodeJSON(w, r, &sch) {
		return
	}
	if sch.Kind != "restart" && sch.Kind != "backup" && sch.Kind != "update" {
		jsonError(w, http.StatusBadRequest, "kind 须为 restart/backup/update")
		return
	}
	if sch.Type == "daily" {
		if _, err := time.Parse("15:04", sch.Time); err != nil {
			jsonError(w, http.StatusBadRequest, "daily 类型需要 HH:MM 时间")
			return
		}
	} else if sch.Type == "interval" {
		if sch.Hours < 1 {
			jsonError(w, http.StatusBadRequest, "interval 类型需要 hours >= 1")
			return
		}
	} else {
		jsonError(w, http.StatusBadRequest, "type 须为 daily/interval")
		return
	}
	sch.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
	sch.Enabled = true
	sv.state.mu.Lock()
	inst.Schedules = append(inst.Schedules, &sch)
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sv.events.Add(inst.Name, "schedule", "添加计划任务：%s", scheduleDesc(&sch))
	jsonOK(w, &sch)
}

func (sv *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sv.state.mu.Lock()
	sch := sv.findSchedule(inst, r.PathValue("id"))
	if sch == nil {
		sv.state.mu.Unlock()
		jsonError(w, http.StatusNotFound, "计划任务不存在")
		return
	}
	if req.Enabled != nil {
		sch.Enabled = *req.Enabled
	}
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	toggle := "禁用"
	if sch.Enabled {
		toggle = "启用"
	}
	sv.events.Add(inst.Name, "schedule", "%s计划任务：%s", toggle, scheduleDesc(sch))
	jsonOK(w, sch)
}

func (sv *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	sv.state.mu.Lock()
	id := r.PathValue("id")
	found := false
	desc := ""
	for i, s := range inst.Schedules {
		if s.ID == id {
			desc = scheduleDesc(s)
			inst.Schedules = append(inst.Schedules[:i], inst.Schedules[i+1:]...)
			found = true
			break
		}
	}
	var err error
	if found {
		err = sv.state.saveLocked()
	}
	sv.state.mu.Unlock()
	if !found {
		jsonError(w, http.StatusNotFound, "计划任务不存在")
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	sv.events.Add(inst.Name, "schedule", "删除计划任务：%s", desc)
	jsonOK(w, map[string]any{"ok": true})
}
