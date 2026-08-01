package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---------- 备份：tar.gz 打包实例存档目录，带保留策略 ----------

type BackupInfo struct {
	File    string    `json:"file"`
	Size    uint64    `json:"size"`
	Created time.Time `json:"created"`
}

func backupDir(inst *Instance) string {
	return BackupsDir + "/" + inst.Name
}

func listBackups(inst *Instance) ([]BackupInfo, error) {
	entries, err := os.ReadDir(backupDir(inst))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			File:    e.Name(),
			Size:    uint64(fi.Size()),
			Created: fi.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

var syncTargetRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+(@[a-zA-Z0-9_.-]+)?$`)

// backupAndSync createBackup 成功后，若配置了 sync_target 则把新备份推送到远端；
// 同步失败视为任务失败（本地备份已保留），让异地备份失效能在事件日志里暴露
func (sv *Server) backupAndSync(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate, retention int) error {
	name, err := createBackup(ctx, log, inst, tmpl, retention)
	if err != nil {
		return err
	}
	sv.state.mu.RLock()
	targets := append([]string(nil), sv.state.SyncTargets...)
	sv.state.mu.RUnlock()
	if len(targets) == 0 {
		return nil
	}
	// 逐目标推送：某个目标失败不影响其他目标，但汇总报错让任务失败、进事件日志
	var failed []string
	for _, target := range targets {
		fmt.Fprintf(log, "同步备份到 %s ...\n", target)
		if err := syncBackup(ctx, log, inst, name, target); err != nil {
			fmt.Fprintf(log, "同步到 %s 失败: %v\n", target, err)
			failed = append(failed, target)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("备份已完成，但同步到 %s 失败", strings.Join(failed, ", "))
	}
	return nil
}

// syncBackup 用 rsync 把备份包推到远端固定文件名 <实例>-latest.tar.gz（远端只留最新一份）
func syncBackup(ctx context.Context, log io.Writer, inst *Instance, file, target string) error {
	mkdir := newCancellableCmd(ctx, "ssh", "-o", "BatchMode=yes", target, "mkdir -p ~/gspanel-saves")
	mkdir.Stdout, mkdir.Stderr = log, log
	if err := mkdir.Run(); err != nil {
		return fmt.Errorf("ssh 连接失败: %w", err)
	}
	rsync := newCancellableCmd(ctx, "rsync", "-az", "-e", "ssh -o BatchMode=yes",
		backupDir(inst)+"/"+file, target+":gspanel-saves/"+inst.Name+"-latest.tar.gz")
	rsync.Stdout, rsync.Stderr = log, log
	if err := rsync.Run(); err != nil {
		return fmt.Errorf("rsync 失败: %w", err)
	}
	fmt.Fprintf(log, "已同步为 %s:gspanel-saves/%s-latest.tar.gz\n", target, inst.Name)
	return nil
}

// createBackup 打包模板声明的 backup_paths；running 时给出一致性提示
func createBackup(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate, retention int) (string, error) {
	if len(tmpl.BackupPaths) == 0 {
		return "", fmt.Errorf("模板未声明备份路径")
	}
	if err := mkdirForGames(backupDir(inst)); err != nil {
		return "", err
	}
	name := time.Now().Format("20060102-150405") + ".tar.gz"
	dest := backupDir(inst) + "/" + name

	args := []string{"czf", dest}
	for _, p := range tmpl.BackupPaths {
		if _, err := os.Stat(inst.Dir + "/" + p); err == nil {
			args = append(args, p)
		}
	}
	if len(args) == 2 {
		return "", fmt.Errorf("没有可备份的内容（%s 尚不存在）", strings.Join(tmpl.BackupPaths, ", "))
	}
	fmt.Fprintf(log, "打包 %s -> %s\n", strings.Join(args[2:], ", "), dest)
	cmd := newCancellableCmd(ctx, "tar", args...)
	cmd.Dir = inst.Dir
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("tar 打包失败: %w", err)
	}
	_ = chownToGames(dest)

	// 保留策略
	if retention <= 0 {
		retention = 10
	}
	backups, _ := listBackups(inst)
	for i, b := range backups {
		if i >= retention {
			fmt.Fprintf(log, "清理旧备份 %s\n", b.File)
			_ = os.Remove(backupDir(inst) + "/" + b.File)
		}
	}
	return name, nil
}

// restoreBackup 停服 -> 解包 -> 按面板记录重写端口/密码 -> （由调用方决定是否）启动
func (sv *Server) restoreBackup(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate, file string) error {
	if strings.Contains(file, "/") || strings.Contains(file, "..") {
		return fmt.Errorf("非法备份文件名")
	}
	src := backupDir(inst) + "/" + file
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("备份不存在: %w", err)
	}
	st := serviceStatus(inst)
	wasRunning := st.ActiveState == "active"
	if wasRunning {
		fmt.Fprintln(log, "停止服务器...")
		if _, err := systemctl("stop", unitName(inst)); err != nil {
			return fmt.Errorf("停止失败: %s", err)
		}
	}
	fmt.Fprintf(log, "从 %s 恢复...\n", src)
	cmd := newCancellableCmd(ctx, "tar", "xzf", src)
	cmd.Dir = inst.Dir
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("解包失败: %w", err)
	}
	// 迁移场景：备份里的 ini 是旧机器的端口/管理员密码，按面板记录重写，恢复后即可用
	if tmpl.RCON != nil {
		if err := sv.applyInstanceConfig(inst, tmpl, log); err != nil {
			return fmt.Errorf("重写实例配置失败: %w", err)
		}
	}
	chownRecursive(inst.Dir)
	if wasRunning {
		fmt.Fprintln(log, "重新启动服务器...")
		if _, err := systemctl("start", unitName(inst)); err != nil {
			return fmt.Errorf("启动失败: %s", err)
		}
	}
	return nil
}

// saveUploadedBackup 把上传的备份包存入实例备份目录；拒绝覆盖同名文件
func saveUploadedBackup(inst *Instance, name string, src io.Reader) error {
	name = filepath.Base(name)
	if !strings.HasSuffix(name, ".tar.gz") {
		return fmt.Errorf("仅支持 .tar.gz 备份包")
	}
	if err := mkdirForGames(backupDir(inst)); err != nil {
		return err
	}
	dest := backupDir(inst) + "/" + name
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("同名备份已存在，请先删除或改名后上传")
		}
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(dest)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dest)
		return err
	}
	return chownToGames(dest)
}

func deleteBackup(inst *Instance, file string) error {
	if strings.Contains(file, "/") || strings.Contains(file, "..") {
		return fmt.Errorf("非法备份文件名")
	}
	return os.Remove(backupDir(inst) + "/" + file)
}

func backupPath(inst *Instance, file string) (string, error) {
	if strings.Contains(file, "/") || strings.Contains(file, "..") || !strings.HasSuffix(file, ".tar.gz") {
		return "", fmt.Errorf("非法备份文件名")
	}
	p := filepath.Join(backupDir(inst), file)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}
