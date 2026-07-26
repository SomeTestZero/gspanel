package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// ---------- 实例生命周期：创建、安装、种子配置、删除 ----------

// portInUse 检查端口是否已被系统占用
func portInUse(port int, proto string) bool {
	if proto == "udp" {
		c, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	}
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

// validatePorts 校验端口：实例间不冲突、系统未占用
func (sv *Server) validatePorts(selfName string, tmpl *GameTemplate, ports map[string]int) error {
	for _, spec := range tmpl.Ports {
		p, ok := ports[spec.Key]
		if !ok {
			ports[spec.Key] = spec.Default
			p = spec.Default
		}
		if p < 1 || p > 65535 {
			return fmt.Errorf("端口 %s(%d) 非法", spec.Key, p)
		}
	}
	sv.state.mu.RLock()
	for _, other := range sv.state.Instances {
		if other.Name == selfName {
			continue
		}
		for k, p := range ports {
			if op, ok := other.Ports[k]; ok && op == p {
				sv.state.mu.RUnlock()
				return fmt.Errorf("端口 %d 已被实例 %q 占用", p, other.Name)
			}
		}
	}
	sv.state.mu.RUnlock()
	for _, spec := range tmpl.Ports {
		if portInUse(ports[spec.Key], spec.Proto) {
			return fmt.Errorf("端口 %s/%d 已被系统其他进程占用", spec.Key, ports[spec.Key])
		}
	}
	return nil
}

// createInstance 创建实例记录与目录
func (sv *Server) createInstance(tmpl *GameTemplate, name, displayName string, ports map[string]int) (*Instance, error) {
	if !instanceNameRe.MatchString(name) {
		return nil, fmt.Errorf("实例名须为小写字母/数字/中划线，2-30 字符")
	}
	sv.state.mu.Lock()
	if _, exists := sv.state.Instances[name]; exists {
		sv.state.mu.Unlock()
		return nil, fmt.Errorf("实例 %q 已存在", name)
	}
	sv.state.mu.Unlock()

	if err := sv.validatePorts(name, tmpl, ports); err != nil {
		return nil, err
	}
	if displayName == "" {
		displayName = name
	}
	inst := &Instance{
		Name:        name,
		Template:    tmpl.ID,
		DisplayName: displayName,
		Dir:         InstancesDir + "/" + name,
		Ports:       ports,
		Args:        append([]string{}, tmpl.DefaultArgs...),
		CreatedAt:   time.Now(),
	}
	if err := mkdirForGames(inst.Dir); err != nil {
		return nil, err
	}
	if err := mkdirForGames(inst.Dir + "/logs"); err != nil {
		return nil, err
	}

	sv.state.mu.Lock()
	sv.state.Instances[name] = inst
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	return inst, err
}

// installInstance 完整安装流程（后台任务）
func (sv *Server) installInstance(inst *Instance, tmpl *GameTemplate) *Task {
	return sv.tasks.Run("install", inst.Name, fmt.Sprintf("安装 %s (AppID %d)", tmpl.Name, tmpl.SteamAppID),
		func(ctx context.Context, log io.Writer, t *Task) error {
			if err := runSteamcmdApp(ctx, log, tmpl.SteamAppID, inst.Dir); err != nil {
				return err
			}
			if err := sv.postInstall(inst, tmpl, log); err != nil {
				return err
			}
			sv.state.mu.Lock()
			inst.Installed = true
			err := sv.state.saveLocked()
			sv.state.mu.Unlock()
			return err
		})
}

// postInstall 安装/更新后的收尾：种子配置、端口写入、启动脚本、unit
func (sv *Server) postInstall(inst *Instance, tmpl *GameTemplate, log io.Writer) error {
	fmt.Fprintln(log, "生成配置与启动脚本...")
	for i := range tmpl.Configs {
		spec := &tmpl.Configs[i]
		if err := seedConfigFile(inst.Dir, spec); err != nil {
			fmt.Fprintf(log, "种子配置 %s 跳过: %v\n", spec.Path, err)
		}
	}
	// Palworld 类模板：把实例端口/管理员密码写进配置文件
	if tmpl.RCON != nil {
		if inst.AdminPassword == "" {
			inst.AdminPassword = randomToken(10)
		}
		if err := sv.applyInstanceConfig(inst, tmpl, log); err != nil {
			fmt.Fprintf(log, "写入实例配置失败（可稍后在配置页手动修改）: %v\n", err)
		}
	}
	if err := writeStartScript(inst, tmpl); err != nil {
		return fmt.Errorf("生成启动脚本: %w", err)
	}
	if err := writeUnit(inst, tmpl); err != nil {
		return fmt.Errorf("生成 systemd unit: %w", err)
	}
	if err := enableUnit(inst); err != nil {
		fmt.Fprintf(log, "设置开机自启失败: %v\n", err)
	}
	chownRecursive(inst.Dir)
	fmt.Fprintln(log, "实例就绪，可以启动")
	return nil
}

// applySavedConfig 启动前把面板保存的配置写回游戏配置文件。
// Palworld 等游戏在优雅关闭时会用内存配置覆盖配置文件，
// 运行期间通过面板修改的值会丢失，故每次启动前以此快照为准。
func (sv *Server) applySavedConfig(inst *Instance, tmpl *GameTemplate) error {
	for path, values := range inst.ConfigValues {
		spec := sv.findConfigSpec(tmpl, path)
		if spec == nil || spec.Format == "raw" || len(values) == 0 {
			continue
		}
		if _, err := os.Stat(inst.Dir + "/" + spec.Path); os.IsNotExist(err) {
			continue // 配置文件尚未生成，跳过
		}
		if err := writeConfigFile(inst.Dir, spec, values, nil); err != nil {
			return fmt.Errorf("写回配置 %s: %w", spec.Path, err)
		}
	}
	return nil
}

// applyInstanceConfig 将实例的端口/密码等写入游戏配置文件
func (sv *Server) applyInstanceConfig(inst *Instance, tmpl *GameTemplate, log io.Writer) error {
	for i := range tmpl.Configs {
		spec := &tmpl.Configs[i]
		if spec.Format != "option-settings" {
			continue
		}
		values := map[string]string{}
		for _, f := range spec.Schema {
			switch f.Key {
			case "PublicPort":
				values["PublicPort"] = fmt.Sprint(inst.Ports["game"])
			case "RCONEnabled":
				values["RCONEnabled"] = "True"
			case "RCONPort":
				values["RCONPort"] = fmt.Sprint(inst.Ports[tmpl.RCON.PortKey])
			case "AdminPassword":
				values["AdminPassword"] = inst.AdminPassword
			case "ServerName":
				if inst.DisplayName != "" {
					values["ServerName"] = inst.DisplayName
				}
			}
		}
		if len(values) > 0 {
			if err := writeConfigFile(inst.Dir, spec, values, nil); err != nil {
				return err
			}
			fmt.Fprintf(log, "已写入实例配置: %s\n", spec.Path)
		}
	}
	return nil
}

// deleteInstance 停服、移除 unit、删除记录，可选删文件
func (sv *Server) deleteInstance(inst *Instance, deleteFiles bool) error {
	removeUnit(inst)
	sv.state.mu.Lock()
	delete(sv.state.Instances, inst.Name)
	err := sv.state.saveLocked()
	sv.state.mu.Unlock()
	if err != nil {
		return err
	}
	if deleteFiles {
		if err := os.RemoveAll(inst.Dir); err != nil {
			return fmt.Errorf("删除实例目录: %w", err)
		}
		_ = os.RemoveAll(backupDir(inst))
	}
	return nil
}
