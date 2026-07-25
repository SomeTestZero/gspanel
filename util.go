package main

import (
	"fmt"
	"os"
	"os/exec"
	osuser "os/user"
	"strconv"
	"syscall"
)

var gamesUID, gamesGID uint32

func init() {
	u, err := osuser.Lookup(GamesUser)
	if err != nil {
		gamesUID, gamesGID = 0, 0
		return
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	gamesUID, gamesGID = uint32(uid), uint32(gid)
}

func chownToGames(path string) error {
	return os.Chown(path, int(gamesUID), int(gamesGID))
}

// chownRecursive 递归改属主（忽略个别错误）
func chownRecursive(root string) {
	_ = exec.Command("chown", "-R", GamesUser+":"+GamesUser, root).Run()
}

// cmdAsGames 以 games 用户身份构造命令
func cmdAsGames(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	if gamesUID != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: gamesUID, Gid: gamesGID},
		}
	}
	cmd.Env = append(os.Environ(),
		"HOME="+GamesHome,
		"USER="+GamesUser,
		"STEAM_HOME="+GamesHome,
	)
	return cmd
}

func mkdirForGames(path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	return chownToGames(path)
}

// humanBytes 格式化字节数
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
