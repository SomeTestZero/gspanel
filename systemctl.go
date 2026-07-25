package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------- systemd 托管：每个实例一个 unit，面板重启游戏不受影响 ----------

func unitName(inst *Instance) string {
	return "gspanel-" + inst.Name + ".service"
}

func unitPath(inst *Instance) string {
	return "/etc/systemd/system/" + unitName(inst)
}

// renderUnit 生成 unit 文件内容
func renderUnit(inst *Instance, tmpl *GameTemplate) string {
	logFile := inst.Dir + "/logs/console.log"
	return fmt.Sprintf(`[Unit]
Description=GSPanel game server: %s (%s)
After=network.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
ExecStart=%s/start.sh
Restart=on-failure
RestartSec=5
TimeoutStopSec=60
LimitNOFILE=100000
StandardOutput=append:%s
StandardError=inherit

[Install]
WantedBy=multi-user.target
`, tmpl.Name, inst.Name, GamesUser, GamesUser, inst.Dir, inst.Dir, logFile)
}

// writeUnit 写入 unit 并 reload；start.sh 需已生成
func writeUnit(inst *Instance, tmpl *GameTemplate) error {
	content := renderUnit(inst, tmpl)
	if err := os.WriteFile(unitPath(inst), []byte(content), 0644); err != nil {
		return fmt.Errorf("写 unit 文件: %w", err)
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func removeUnit(inst *Instance) {
	_ = exec.Command("systemctl", "stop", unitName(inst)).Run()
	_ = exec.Command("systemctl", "disable", unitName(inst)).Run()
	_ = os.Remove(unitPath(inst))
	_ = exec.Command("systemctl", "daemon-reload").Run()
}

func systemctl(args ...string) (string, error) {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// enableUnit 开机自启
func enableUnit(inst *Instance) error {
	if out, err := systemctl("enable", unitName(inst)); err != nil {
		return fmt.Errorf("enable: %s", out)
	}
	return nil
}

// ---------- 实例运行状态 ----------

type ServiceStatus struct {
	ActiveState   string `json:"active_state"`  // active / inactive / failed ...
	SubState      string `json:"sub_state"`     // running / dead ...
	MainPID       int    `json:"main_pid"`
	MemoryBytes   uint64 `json:"memory_bytes"`
	CPUUsageNSec  uint64 `json:"cpu_usage_nsec"`
	ExecStartOK   bool   `json:"unit_exists"`
}

func serviceStatus(inst *Instance) *ServiceStatus {
	st := &ServiceStatus{ActiveState: "not-installed", SubState: "-"}
	if _, err := os.Stat(unitPath(inst)); err != nil {
		return st
	}
	st.ExecStartOK = true
	out, err := systemctl("show", unitName(inst),
		"-p", "ActiveState,SubState,MainPID,MemoryCurrent,CPUUsageNSec")
	if err != nil {
		st.ActiveState = "unknown"
		return st
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if kv := strings.SplitN(line, "=", 2); len(kv) == 2 {
			props[kv[0]] = kv[1]
		}
	}
	st.ActiveState = props["ActiveState"]
	st.SubState = props["SubState"]
	st.MainPID, _ = strconv.Atoi(props["MainPID"])
	if v := props["MemoryCurrent"]; v != "[not set]" {
		st.MemoryBytes, _ = strconv.ParseUint(v, 10, 64)
	}
	if v := props["CPUUsageNSec"]; v != "[not set]" {
		st.CPUUsageNSec, _ = strconv.ParseUint(v, 10, 64)
	}
	return st
}

// startScript 生成启动脚本（参数安全引用）
func writeStartScript(inst *Instance, tmpl *GameTemplate) error {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n# 由 gspanel 生成，手动修改可能在下次安装/更新时被覆盖\n")
	b.WriteString("cd \"$(dirname \"$0\")\"\n")
	for k, v := range inst.Env {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
	}
	fmt.Fprintf(&b, "exec %s", tmpl.Executable)
	for _, a := range inst.Args {
		fmt.Fprintf(&b, " %s", shellQuote(a))
	}
	b.WriteString("\n")
	path := filepath.Join(inst.Dir, "start.sh")
	if err := os.WriteFile(path, []byte(b.String()), 0755); err != nil {
		return err
	}
	return chownToGames(path)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("-._/:+=,@%", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
