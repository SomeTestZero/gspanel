package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------- 版本轮询：对比 Steam 最新 buildid 与本地 appmanifest，发现新版本自动更新 ----------
//
// steamcmd 服务端没有补丁推送通道，只能主动查。这里用第三方只读接口
// api.steamcmd.net（等价于 steamcmd +app_info_print 的结果）拿 public 分支 buildid，
// 与实例目录下 steamapps/appmanifest_<appid>.acf 里的 buildid 对比。

const updateCheckInterval = 30 * time.Minute

// 自动更新的玩家门槛：服务器无人且持续该时长后才更新；有玩家时按该间隔重复广播提醒
const (
	updateEmptyDelay     = 10 * time.Minute
	updateNotifyInterval = time.Hour
)

var buildIDRe = regexp.MustCompile(`"buildid"\s+"(\d+)"`)

var updateCheckClient = &http.Client{Timeout: 15 * time.Second}

// localBuildID 读本地 appmanifest 里的 buildid；未安装/文件缺失返回错误
func localBuildID(instDir string, appID int) (int64, error) {
	path := fmt.Sprintf("%s/steamapps/appmanifest_%d.acf", instDir, appID)
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	m := buildIDRe.FindSubmatch(data)
	if m == nil {
		return 0, fmt.Errorf("appmanifest 中未找到 buildid")
	}
	return strconv.ParseInt(string(m[1]), 10, 64)
}

// latestBuildID 查 Steam public 分支最新 buildid
func latestBuildID(appID int) (int64, error) {
	url := fmt.Sprintf("https://api.steamcmd.net/v1/info/%d", appID)
	resp, err := updateCheckClient.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("查询版本失败: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data map[string]struct {
			Depots struct {
				Branches map[string]struct {
					BuildID string `json:"buildid"`
				} `json:"branches"`
			} `json:"depots"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	app, ok := out.Data[strconv.Itoa(appID)]
	if !ok {
		return 0, fmt.Errorf("接口未返回 app %d 信息", appID)
	}
	pub, ok := app.Depots.Branches["public"]
	if !ok || pub.BuildID == "" {
		return 0, fmt.Errorf("app %d 无 public 分支 buildid", appID)
	}
	return strconv.ParseInt(pub.BuildID, 10, 64)
}

// autoUpdateReady 玩家门槛：有玩家在线则广播通知并等待，无人且持续 updateEmptyDelay 后才放行更新。
// 仅在 checkUpdates 单 goroutine 内调用，直接读写 sc.autoStates。
func (sc *Scheduler) autoUpdateReady(inst *Instance, tmpl *GameTemplate) bool {
	if serviceStatus(inst).ActiveState != "active" {
		return true // 服务器没开，不会有玩家
	}
	n, supported, err := playerCount(inst, tmpl)
	if !supported {
		return true // 模板查不了玩家数，保持直接更新
	}
	name := inst.Name
	if err != nil {
		log.Printf("自动更新检查 %s: 查询在线玩家失败，本轮跳过: %v", name, err)
		return false
	}
	now := time.Now()
	st := sc.autoStates[name]
	if st == nil {
		st = &autoUpdateState{}
		sc.autoStates[name] = st
	}
	if n > 0 {
		st.emptySince = time.Time{}
		if now.Sub(st.notifiedAt) >= updateNotifyInterval {
			if err := broadcastMessage(inst, tmpl,
				fmt.Sprintf("服务器检测到新版本，将在无玩家%d分钟后自动更新重启，请合理安排游戏时间", int(updateEmptyDelay.Minutes()))); err != nil {
				log.Printf("自动更新检查 %s: 广播更新通知失败: %v", name, err)
			} else {
				st.notifiedAt = now
			}
		}
		log.Printf("自动更新检查 %s: %d 名玩家在线，推迟更新", name, n)
		return false
	}
	if st.emptySince.IsZero() {
		st.emptySince = now
		log.Printf("自动更新检查 %s: 已无玩家，持续 %d 分钟无人后开始更新", name, int(updateEmptyDelay.Minutes()))
		return false
	}
	if now.Sub(st.emptySince) < updateEmptyDelay {
		return false
	}
	return true
}

// playerCount 在线玩家数。模板 REST 命令里没有 format=players 的接口时 supported=false
func playerCount(inst *Instance, tmpl *GameTemplate) (n int, supported bool, err error) {
	if tmpl.RestAPI == nil || inst.AdminPassword == "" {
		return 0, false, nil
	}
	for _, spec := range tmpl.RestAPI.Commands {
		if spec.Format == "players" {
			addr := fmt.Sprintf("127.0.0.1:%d", instancePort(inst, tmpl, tmpl.RestAPI.PortKey))
			n, err := restPlayerCount(addr, inst.AdminPassword, spec)
			return n, true, err
		}
	}
	return 0, false, nil
}

// broadcastMessage 给在线玩家广播：模板定义了 REST Broadcast 优先走 REST（RCON 丢响应的游戏），否则走 RCON
func broadcastMessage(inst *Instance, tmpl *GameTemplate, msg string) error {
	if tmpl.RestAPI != nil && inst.AdminPassword != "" {
		for name, spec := range tmpl.RestAPI.Commands {
			if strings.EqualFold(name, "Broadcast") {
				addr := fmt.Sprintf("127.0.0.1:%d", instancePort(inst, tmpl, tmpl.RestAPI.PortKey))
				_, err := restExec(addr, inst.AdminPassword, spec, msg)
				return err
			}
		}
	}
	if tmpl.RCON == nil || inst.AdminPassword == "" {
		return fmt.Errorf("无可用广播通道")
	}
	addr := fmt.Sprintf("127.0.0.1:%d", inst.Ports[tmpl.RCON.PortKey])
	_, err := RconExec(addr, inst.AdminPassword, "Broadcast "+strings.ReplaceAll(msg, " ", "_"))
	return err
}
// maybeCheckUpdates 到点后在后台跑一轮检查（上一轮没跑完则跳过）
func (sc *Scheduler) maybeCheckUpdates(sv *Server, now time.Time) {
	if now.Sub(sc.lastUpdateCheck) < updateCheckInterval {
		return
	}
	if !sc.updateChecking.CompareAndSwap(false, true) {
		return
	}
	sc.lastUpdateCheck = now
	go func() {
		defer sc.updateChecking.Store(false)
		sc.checkUpdates(sv)
	}()
}

func (sc *Scheduler) checkUpdates(sv *Server) {
	type target struct {
		inst *Instance
		tmpl *GameTemplate
	}
	sc.state.mu.RLock()
	var targets []target
	for _, inst := range sc.state.Instances {
		if !inst.AutoUpdate || !inst.Installed {
			continue
		}
		tmpl := sv.getTemplate(inst.Template)
		if tmpl == nil || tmpl.SteamAppID <= 0 {
			continue
		}
		targets = append(targets, target{inst, tmpl})
	}
	sc.state.mu.RUnlock()

	for _, tg := range targets {
		name := tg.inst.Name
		if sc.tasks.HasRunningFor(name) {
			continue // 有任务在跑（可能正是更新），别叠加
		}
		local, err := localBuildID(tg.inst.Dir, tg.tmpl.SteamAppID)
		if err != nil {
			log.Printf("自动更新检查 %s: 读本地版本失败: %v", name, err)
			continue
		}
		latest, err := latestBuildID(tg.tmpl.SteamAppID)
		if err != nil {
			log.Printf("自动更新检查 %s: 查询最新版本失败: %v", name, err)
			continue
		}
		if latest <= local {
			continue
		}
		if !sc.autoUpdateReady(tg.inst, tg.tmpl) {
			continue
		}
		delete(sc.autoStates, name)
		log.Printf("自动更新检查 %s: 发现新版本 build %d（当前 %d），开始自动更新", name, latest, local)
		inst, tmpl := tg.inst, tg.tmpl
		sc.tasks.Run("auto-update", name,
			fmt.Sprintf("自动更新 %s（新版本 build %d）", tmpl.Name, latest),
			func(ctx context.Context, w io.Writer, t *Task) error {
				fmt.Fprintf(w, "检测到新版本：build %d（当前 %d）\n", latest, local)
				return sv.updateInstance(ctx, w, inst, tmpl)
			})
	}
}
