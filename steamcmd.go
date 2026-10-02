package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const steamcmdTarball = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"

func steamcmdScript() string {
	return filepath.Join(SteamcmdDir, "steamcmd.sh")
}

func steamcmdInstalled() bool {
	_, err := os.Stat(steamcmdScript())
	return err == nil
}

// installSteamcmd 下载并解压 steamcmd（作为后台任务运行）
func installSteamcmd(ctx context.Context, log io.Writer) error {
	if steamcmdInstalled() {
		fmt.Fprintln(log, "steamcmd 已安装，执行自更新...")
	} else {
		fmt.Fprintln(log, "下载 steamcmd ...")
		if err := mkdirForGames(SteamcmdDir); err != nil {
			return err
		}
		dl := cmdAsGames("bash", "-c",
			fmt.Sprintf("curl -sfL %q | tar zxvf - -C %q", steamcmdTarball, SteamcmdDir))
		dl.Stdout, dl.Stderr = log, log
		if err := dl.Run(); err != nil {
			return fmt.Errorf("下载/解压 steamcmd 失败: %w", err)
		}
	}
	// 首次运行触发自更新
	fmt.Fprintln(log, "steamcmd 自更新中（首次较慢）...")
	up := cmdAsGames("bash", steamcmdScript(), "+quit")
	up.Dir = SteamcmdDir
	up.Stdout, up.Stderr = log, log
	if err := up.Run(); err != nil {
		return fmt.Errorf("steamcmd 自更新失败: %w", err)
	}
	fmt.Fprintln(log, "steamcmd 就绪")
	return nil
}

// runSteamcmdApp 安装/更新某个 AppID 到指定目录
func runSteamcmdApp(ctx context.Context, log io.Writer, appID int, installDir string) error {
	if !steamcmdInstalled() {
		return fmt.Errorf("steamcmd 未安装，请先在「环境」页安装")
	}
	if err := mkdirForGames(installDir); err != nil {
		return err
	}
	args := []string{
		steamcmdScript(),
		"+force_install_dir", installDir,
		"+login", "anonymous",
		"+app_update", fmt.Sprint(appID), "validate",
		"+quit",
	}
	cmd := cmdAsGames("bash", args...)
	cmd.Dir = SteamcmdDir
	cmd.Stdout, cmd.Stderr = log, log
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return fmt.Errorf("任务被取消")
	case err := <-done:
		if err != nil {
			return fmt.Errorf("steamcmd 执行失败: %w", err)
		}
	}
	return nil
}

// depsStatus 检查 32 位运行库
func depsStatus() map[string]bool {
	check := func(pkg string) bool {
		return cmdOk("dpkg", "-s", pkg)
	}
	return map[string]bool{
		"lib32gcc-s1":   check("lib32gcc-s1"),
		"lib32stdc++6":  check("lib32stdc++6"),
		"steamcmd":      steamcmdInstalled(),
	}
}

func cmdOk(name string, args ...string) bool {
	return cmdAsGames(name, args...).Run() == nil
}

// installDeps 安装系统依赖（root 直接执行 apt；普通用户经特权助手）
func installDeps(ctx context.Context, log io.Writer) error {
	fmt.Fprintln(log, "安装 32 位运行库 lib32gcc-s1 / lib32stdc++6 ...")
	var c *exec.Cmd
	if os.Geteuid() == 0 {
		c = newCancellableCmd(ctx, "bash", "-c",
			"apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq lib32gcc-s1 lib32stdc++6")
	} else {
		c = newCancellableCmd(ctx, "sudo", "-n", privHelper, "install-deps")
	}
	c.Stdout, c.Stderr = log, log
	if err := c.Run(); err != nil {
		return fmt.Errorf("apt 安装失败: %w", err)
	}
	fmt.Fprintln(log, "依赖安装完成")
	return nil
}

// ensureSteamSDK64 把 $GamesHome/.steam/sdk64/steamclient.so 指向实例自带的 steamclient.so。
// Steamworks.GameServer 固定从 ~/.steam/sdk64/steamclient.so 加载 steamclient（unit 里 HOME=/home/games），
// 缺这个软链时 GameServer.Init 失败（"Could not initialize GameServer"），Steam 玩家连不进。
// 已存在则不动（steamclient 各实例同源，指向哪个实例都行）；游戏不带 steamclient.so 的模板自动跳过。
func ensureSteamSDK64(inst *Instance) error {
	src := filepath.Join(inst.Dir, "steamclient.so")
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	dir := filepath.Join(GamesHome, ".steam", "sdk64")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	_ = chownToGames(filepath.Join(GamesHome, ".steam"))
	_ = chownToGames(dir)
	link := filepath.Join(dir, "steamclient.so")
	if _, err := os.Lstat(link); err == nil {
		return nil
	}
	return os.Symlink(src, link)
}
