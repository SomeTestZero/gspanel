package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var modCmdMu sync.Mutex
var modItemIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var modDigitsRe = regexp.MustCompile(`^[0-9]+$`)

func validateModCommand(verb string, args []string) error {
	want := 0
	switch verb {
	case "who", "whojson", "hello":
	case "give":
		want = 3
	case "giveexp":
		want = 2
	default:
		return fmt.Errorf("不支持的命令: %s", verb)
	}
	if len(args) != want {
		return fmt.Errorf("%s 需要 %d 个参数", verb, want)
	}
	for _, a := range args {
		if strings.TrimSpace(a) == "" || len(a) > 256 || strings.ContainsAny(a, "\x00\r\n") {
			return fmt.Errorf("参数不能为空、过长或包含换行/NUL")
		}
	}
	if want == 0 {
		return nil
	}
	if verb == "give" && !modItemIDRe.MatchString(args[1]) {
		return fmt.Errorf("物品 ID 格式不正确")
	}
	max := 10000
	if verb == "giveexp" {
		max = 10000000
	}
	qty := args[len(args)-1]
	n, err := strconv.Atoi(qty)
	if err != nil || !modDigitsRe.MatchString(qty) || n < 1 || n > max {
		return fmt.Errorf("数量必须是 1～%d 的整数", max)
	}
	return nil
}

// v2 请求/响应关联：迟到响应绝不能被下一次 give 当作成功。mod 原子 rename 写响应。
func parseModResult(data []byte, id string) (matched, ok bool, message string) {
	parts := strings.SplitN(string(data), "\n", 3)
	if len(parts) != 3 || parts[0] != "gsp2:"+id || (parts[1] != "OK" && parts[1] != "FAIL") {
		return false, false, ""
	}
	return true, parts[1] == "OK", strings.TrimSpace(parts[2])
}

// runModVerb 串行队列；只对确实运行新版 mod 的进程发命令。旧版需安装并重启。
func (sv *Server) runModVerb(inst *Instance, verb string, args []string, timeout time.Duration) (bool, string, error) {
	modCmdMu.Lock()
	defer modCmdMu.Unlock()
	if runningModVersion(inst) != modVersion {
		return false, "", fmt.Errorf("游戏未运行新版扩展命令：请在实例设置中更新扩展命令并重启游戏，等待加载完成")
	}
	dir := modQueueDir(inst)
	if err := mkdirForGames(dir); err != nil {
		return false, "", err
	}
	// 未消费命令不覆盖：它可能是一个等待游戏线程的 give。
	if _, err := os.Stat(dir + "/cmd.txt"); err == nil {
		return false, "", fmt.Errorf("mod 尚有未处理命令，请稍后再试；如上次给物品超时，请先核对背包")
	} else if !os.IsNotExist(err) {
		return false, "", err
	}
	id := randomToken(16)
	content := "gsp2:" + id + "\n" + verb + "\n"
	if len(args) > 0 {
		content += strings.Join(args, "\n") + "\n"
	}
	tmp := dir + "/cmd.txt.tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return false, "", err
	}
	defer os.Remove(tmp)
	if err := chownToGames(tmp); err != nil {
		return false, "", err
	}
	if err := os.Rename(tmp, dir+"/cmd.txt"); err != nil {
		return false, "", err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(dir + "/res.txt"); err == nil {
			if matched, ok, msg := parseModResult(data, id); matched {
				return ok, msg, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	// 不自动重发：超时不等于没有执行，游戏线程可能稍后完成。
	return false, "", fmt.Errorf("mod 未在 %s 内响应；操作结果不确定，请先核对背包，不要直接重复发放", timeout)
}
