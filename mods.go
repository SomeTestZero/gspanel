package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------- Palworld 扩展命令（UE4SS）面板侧管理 ----------
//
// 分工：框架（libUE4SS.so）由仓库 tools/palworld-ue4ss/ 的脚本从 fork 编译，
// 通过设置页上传/导入/下载到 data/ue4ss/；本文件负责把它和内置的 mod/布局表
// 安装到实例、改写 start.sh（LD_PRELOAD 只对游戏二进制生效）。
//
//go:embed assets/palworld-ue4ss
var palworldAssets embed.FS

const (
	palworldAssetRoot = "assets/palworld-ue4ss"
	ue4ssMaxBinary    = 300 << 20 // 上传/导入/下载的 libUE4SS.so 上限
)

func ue4ssAssetDir() string  { return filepath.Join(BaseDir, "data", "ue4ss") }
func ue4ssAssetPath() string { return filepath.Join(ue4ssAssetDir(), "libUE4SS.so") }

// UE4SSBinaryInfo 面板侧保存的框架二进制信息
type UE4SSBinaryInfo struct {
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256,omitempty"`
	Modified string `json:"modified,omitempty"`
}

func ue4ssBinaryInfo() UE4SSBinaryInfo {
	info := UE4SSBinaryInfo{Path: ue4ssAssetPath()}
	st, err := os.Stat(info.Path)
	if err != nil || !st.Mode().IsRegular() {
		return info
	}
	info.Exists = true
	info.Size = st.Size()
	info.Modified = st.ModTime().Format(time.RFC3339)
	if f, err := os.Open(info.Path); err == nil {
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			info.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		_ = f.Close()
	}
	return info
}

// saveUE4SSBinary 落盘到 data/ue4ss/libUE4SS.so（先写 tmp 再 rename）
func saveUE4SSBinary(r io.Reader) error {
	if err := os.MkdirAll(ue4ssAssetDir(), 0700); err != nil {
		return err
	}
	tmp := ue4ssAssetPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, ue4ssMaxBinary+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if n > ue4ssMaxBinary {
		_ = os.Remove(tmp)
		return fmt.Errorf("文件过大（上限 %s）", humanBytes(ue4ssMaxBinary))
	}
	if n < 1024 {
		_ = os.Remove(tmp)
		return fmt.Errorf("文件太小（%d 字节），不像是 libUE4SS.so", n)
	}
	return os.Rename(tmp, ue4ssAssetPath())
}

// UE4SSStatus 实例上的扩展状态
type UE4SSStatus struct {
	Supported    bool            `json:"supported"`     // 模板是否支持（palworld）
	Enabled      bool            `json:"enabled"`       // 面板配置里希望启用
	Installed    bool            `json:"installed"`     // 实例里有 libUE4SS.so
	Mod          bool            `json:"mod"`           // Mods/gspanel/scripts/main.lua
	Queue        bool            `json:"queue"`         // gspanel-mod 命令队列目录
	StartPatched bool            `json:"start_patched"` // start.sh 已注入 LD_PRELOAD
	Binary       UE4SSBinaryInfo `json:"binary"`        // 面板侧二进制
}

func ue4ssStatus(inst *Instance) UE4SSStatus {
	bin := filepath.Join(inst.Dir, "Pal", "Binaries", "Linux")
	st := UE4SSStatus{
		Supported: inst.Template == "palworld",
		Enabled:   inst.UE4SS,
		Binary:    ue4ssBinaryInfo(),
	}
	if _, err := os.Stat(filepath.Join(bin, "libUE4SS.so")); err == nil {
		st.Installed = true
	}
	if _, err := os.Stat(filepath.Join(bin, "Mods", "gspanel", "scripts", "main.lua")); err == nil {
		st.Mod = true
	}
	if fi, err := os.Stat(filepath.Join(bin, "gspanel-mod")); err == nil && fi.IsDir() {
		st.Queue = true
	}
	if data, err := os.ReadFile(filepath.Join(inst.Dir, "start.sh")); err == nil {
		st.StartPatched = strings.Contains(string(data), "LD_PRELOAD")
	}
	return st
}

// hasGiveMod 控制台页「扩展」按钮是否显示
func hasGiveMod(inst *Instance) bool {
	st := ue4ssStatus(inst)
	return st.Mod && st.Queue
}

func palworldAsset(name string) ([]byte, error) {
	return palworldAssets.ReadFile(palworldAssetRoot + "/" + name)
}

func writeAssetFile(dst string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return chownToGames(dst)
}

func copyFileAs(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, mode); err != nil {
		return err
	}
	return chownToGames(dst)
}

// installUE4SS 安装/更新实例上的 UE4SS 与 gspanel mod，并重写 start.sh
func (sv *Server) installUE4SS(inst *Instance, tmpl *GameTemplate) error {
	if tmpl == nil || tmpl.ID != "palworld" {
		return fmt.Errorf("当前仅 Palworld 模板支持扩展命令")
	}
	if !ue4ssBinaryInfo().Exists {
		return fmt.Errorf("面板还没有 UE4SS 二进制（%s）：请到「设置/环境 → Palworld 扩展命令」上传、从本机路径导入或从 URL 下载", ue4ssAssetPath())
	}
	bin := filepath.Join(inst.Dir, "Pal", "Binaries", "Linux")
	for _, d := range []string{
		bin,
		filepath.Join(bin, "Mods", "gspanel", "scripts"),
		filepath.Join(bin, "gspanel-mod"),
	} {
		if err := mkdirForGames(d); err != nil {
			return err
		}
	}
	// 首次安装时备份原始 start.sh，卸载时恢复
	backup := filepath.Join(inst.Dir, "start.sh.pre-ue4ss")
	if _, err := os.Stat(backup); os.IsNotExist(err) {
		if err := copyFileAs(filepath.Join(inst.Dir, "start.sh"), backup, 0755); err != nil {
			return fmt.Errorf("备份 start.sh: %w", err)
		}
	}
	if err := copyFileAs(ue4ssAssetPath(), filepath.Join(bin, "libUE4SS.so"), 0755); err != nil {
		return fmt.Errorf("复制 libUE4SS.so: %w", err)
	}
	for _, f := range []struct{ name, dst string }{
		{"layouts/MemberVariableLayout.ini", filepath.Join(bin, "MemberVariableLayout.ini")},
		{"layouts/VTableLayout.ini", filepath.Join(bin, "VTableLayout.ini")},
		{"UE4SS-settings.ini", filepath.Join(bin, "UE4SS-settings.ini")},
		{"mods.txt", filepath.Join(bin, "Mods", "mods.txt")},
		{"mod/scripts/main.lua", filepath.Join(bin, "Mods", "gspanel", "scripts", "main.lua")},
		// UE4SS 的工作目录兼容：实例根也放一份布局表
		{"layouts/MemberVariableLayout.ini", filepath.Join(inst.Dir, "MemberVariableLayout.ini")},
		{"layouts/VTableLayout.ini", filepath.Join(inst.Dir, "VTableLayout.ini")},
	} {
		data, err := palworldAsset(f.name)
		if err != nil {
			return fmt.Errorf("读取内置资产 %s: %w", f.name, err)
		}
		if err := writeAssetFile(f.dst, data, 0644); err != nil {
			return fmt.Errorf("写入 %s: %w", f.dst, err)
		}
	}
	inst.UE4SS = true
	if err := writeStartScript(inst, tmpl); err != nil {
		return fmt.Errorf("重写 start.sh: %w", err)
	}
	return nil
}

// uninstallUE4SS 恢复 start.sh 并移除注入文件（保留 start.sh.pre-ue4ss 备份）
func (sv *Server) uninstallUE4SS(inst *Instance, tmpl *GameTemplate) error {
	inst.UE4SS = false
	bin := filepath.Join(inst.Dir, "Pal", "Binaries", "Linux")
	backup := filepath.Join(inst.Dir, "start.sh.pre-ue4ss")
	if _, err := os.Stat(backup); err == nil {
		if err := copyFileAs(backup, filepath.Join(inst.Dir, "start.sh"), 0755); err != nil {
			return fmt.Errorf("恢复 start.sh: %w", err)
		}
	} else if err := writeStartScript(inst, tmpl); err != nil {
		return fmt.Errorf("重写 start.sh: %w", err)
	}
	for _, p := range []string{
		filepath.Join(bin, "libUE4SS.so"),
		filepath.Join(bin, "MemberVariableLayout.ini"),
		filepath.Join(bin, "VTableLayout.ini"),
		filepath.Join(bin, "UE4SS-settings.ini"),
		filepath.Join(bin, "Mods", "gspanel"),
		filepath.Join(bin, "gspanel-mod"),
		filepath.Join(inst.Dir, "MemberVariableLayout.ini"),
		filepath.Join(inst.Dir, "VTableLayout.ini"),
	} {
		_ = os.RemoveAll(p)
	}
	return nil
}

// ---------- HTTP 接口 ----------

func (sv *Server) handleUE4SSInfo(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{
		"binary":    ue4ssBinaryInfo(),
		"asset_dir": ue4ssAssetDir(),
		"max_size":  ue4ssMaxBinary,
	})
}

func (sv *Server) handleUE4SSUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, ue4ssMaxBinary+(8<<20))
	file, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, http.StatusBadRequest, "读取上传文件失败（表单字段名应为 file）: "+err.Error())
		return
	}
	defer file.Close()
	if err := saveUE4SSBinary(file); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	info := ue4ssBinaryInfo()
	sv.events.Add("", "ue4ss", "上传 UE4SS 二进制（%s，sha256 %s）", humanBytes(uint64(info.Size)), shortHash(info.SHA256))
	jsonOK(w, map[string]any{"binary": info})
}

func (sv *Server) handleUE4SSImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	p := strings.TrimSpace(req.Path)
	if p == "" || !filepath.IsAbs(p) {
		jsonError(w, http.StatusBadRequest, "需要服务器上的绝对路径")
		return
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		jsonError(w, http.StatusBadRequest, "文件不存在或不是普通文件: "+p)
		return
	}
	if st.Size() > ue4ssMaxBinary {
		jsonError(w, http.StatusBadRequest, "文件过大（上限 "+humanBytes(ue4ssMaxBinary)+"）")
		return
	}
	f, err := os.Open(p)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	if err := saveUE4SSBinary(f); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	info := ue4ssBinaryInfo()
	sv.events.Add("", "ue4ss", "从本机路径导入 UE4SS 二进制（%s，sha256 %s）", p, shortHash(info.SHA256))
	jsonOK(w, map[string]any{"binary": info})
}

func (sv *Server) handleUE4SSDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	u := strings.TrimSpace(req.URL)
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		jsonError(w, http.StatusBadRequest, "URL 需以 http(s):// 开头")
		return
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Get(u)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "下载失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		jsonError(w, http.StatusBadGateway, fmt.Sprintf("下载失败: HTTP %d", resp.StatusCode))
		return
	}
	if err := saveUE4SSBinary(resp.Body); err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	info := ue4ssBinaryInfo()
	sv.events.Add("", "ue4ss", "从 URL 下载 UE4SS 二进制（%s，sha256 %s）", u, shortHash(info.SHA256))
	jsonOK(w, map[string]any{"binary": info})
}

func (sv *Server) handleInstanceUE4SS(w http.ResponseWriter, r *http.Request) {
	inst := sv.getInstance(w, r)
	if inst == nil {
		return
	}
	tmpl := sv.templateOf(inst)
	var req struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Action {
	case "install", "upgrade":
		if err := sv.installUE4SS(inst, tmpl); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		info := ue4ssBinaryInfo()
		sv.events.Add(inst.Name, "ue4ss", "安装/更新 UE4SS 扩展命令（二进制 %s，sha256 %s）", humanBytes(uint64(info.Size)), shortHash(info.SHA256))
	case "uninstall":
		if err := sv.uninstallUE4SS(inst, tmpl); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		sv.events.Add(inst.Name, "ue4ss", "卸载 UE4SS 扩展命令")
	default:
		jsonError(w, http.StatusBadRequest, "action 须为 install / upgrade / uninstall")
		return
	}
	if err := sv.state.Save(); err != nil {
		jsonError(w, http.StatusInternalServerError, "保存配置失败: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ue4ss": ue4ssStatus(inst), "restart_hint": "重启实例后生效"})
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
