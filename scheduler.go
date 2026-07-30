package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// ---------- 计划任务：每日定时 / 间隔循环，执行重启、备份、更新 ----------

type Scheduler struct {
	state *State
	tasks *TaskManager
	stop  chan struct{}

	lastUpdateCheck time.Time   // 上次版本轮询时间
	updateChecking  atomic.Bool // 防止上一轮检查未结束又开一轮
	// 自动更新的玩家门槛状态（仅 checkUpdates goroutine 读写，无需锁）：
	// 首次观察到无玩家的时间、上次广播通知时间
	autoStates map[string]*autoUpdateState
}

type autoUpdateState struct {
	emptySince time.Time
	notifiedAt time.Time
}

func NewScheduler(state *State, tasks *TaskManager) *Scheduler {
	return &Scheduler{state: state, tasks: tasks, stop: make(chan struct{}), autoStates: map[string]*autoUpdateState{}}
}

func (sc *Scheduler) Start(sv *Server) {
	ticker := time.NewTicker(30 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-sc.stop:
				return
			case now := <-ticker.C:
				sc.tick(sv, now)
			}
		}
	}()
}

func (sc *Scheduler) Stop() { close(sc.stop) }

func (sc *Scheduler) tick(sv *Server, now time.Time) {
	sc.state.mu.Lock()
	var due []*Schedule
	insts := map[string]*Instance{}
	for _, inst := range sc.state.Instances {
		for _, sch := range inst.Schedules {
			if sch.Enabled && scheduleDue(sch, now) {
				sch.LastRun = now
				due = append(due, sch)
				insts[sch.ID] = inst
			}
		}
	}
	if len(due) > 0 {
		_ = sc.state.saveLocked()
	}
	sc.state.mu.Unlock()

	for _, sch := range due {
		inst := insts[sch.ID]
		sv.runScheduled(inst, sch)
	}

	sv.watchInstances()
	sc.maybeCheckUpdates(sv, now)
}

func scheduleDue(sch *Schedule, now time.Time) bool {
	switch sch.Type {
	case "daily":
		var hh, mm int
		if _, err := fmt.Sscanf(sch.Time, "%d:%d", &hh, &mm); err != nil {
			return false
		}
		target := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
		if now.Before(target) {
			return false // 今天还没到点
		}
		// 到点了：今天还没跑过才触发
		return sch.LastRun.Before(target)
	case "interval":
		if sch.Hours <= 0 {
			return false
		}
		return now.Sub(sch.LastRun) >= time.Duration(sch.Hours)*time.Hour
	}
	return false
}

// runScheduled 按计划执行任务
func (sv *Server) runScheduled(inst *Instance, sch *Schedule) {
	tmpl := sv.getTemplate(inst.Template)
	if tmpl == nil {
		return
	}
	sv.tasks.Run("scheduled-"+sch.Kind, inst.Name, scheduleDesc(sch), func(ctx context.Context, log io.Writer, t *Task) error {
		switch sch.Kind {
		case "backup":
			_, err := createBackup(ctx, log, inst, tmpl, sch.Retention)
			return err
		case "restart":
			return sv.gracefulRestart(ctx, log, inst, tmpl)
		case "update":
			return sv.updateInstance(ctx, log, inst, tmpl)
		}
		return nil
	})
}

// gracefulRestart 有 RCON 先广播+存档+优雅关闭，再启动
func (sv *Server) gracefulRestart(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate) error {
	st := serviceStatus(inst)
	wasRunning := st.ActiveState == "active"
	if !wasRunning {
		fmt.Fprintln(log, "服务器未运行，直接启动")
		if err := sv.applySavedConfig(inst, tmpl); err != nil {
			return err
		}
		if _, err := systemctl("start", unitName(inst)); err != nil {
			return fmt.Errorf("启动失败: %s", err)
		}
		return nil
	}
	if err := sv.gracefulStop(ctx, log, inst, tmpl); err != nil {
		return err
	}
	fmt.Fprintln(log, "启动服务器...")
	if err := sv.applySavedConfig(inst, tmpl); err != nil {
		return err
	}
	if _, err := systemctl("start", unitName(inst)); err != nil {
		return fmt.Errorf("启动失败: %s", err)
	}
	return nil
}

// gracefulStop RCON 广播 -> Save -> Shutdown，超时后 systemctl stop
func (sv *Server) gracefulStop(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate) error {
	if tmpl.StopMode == "rcon" && tmpl.RCON != nil && inst.AdminPassword != "" {
		addr := fmt.Sprintf("127.0.0.1:%d", inst.Ports[tmpl.RCON.PortKey])
		warn := tmpl.StopWarnSecs
		if warn <= 0 {
			warn = 10
		}
		if _, err := RconExec(addr, inst.AdminPassword, fmt.Sprintf("Broadcast Server_will_restart_in_%d_seconds", warn)); err != nil {
			fmt.Fprintf(log, "RCON 广播失败（继续停止流程）: %v\n", err)
		} else {
			fmt.Fprintf(log, "已广播 %d 秒停机通知，存档并关闭...\n", warn)
		}
		_, _ = RconExec(addr, inst.AdminPassword, "Save")
		_, _ = RconExec(addr, inst.AdminPassword, fmt.Sprintf("Shutdown %d Server_maintenance", warn))
		// 等待进程自行退出
		deadline := time.Now().Add(time.Duration(warn)*time.Second + 30*time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return fmt.Errorf("任务被取消")
			case <-time.After(2 * time.Second):
			}
			if serviceStatus(inst).ActiveState != "active" {
				fmt.Fprintln(log, "服务器已优雅关闭")
				return nil
			}
		}
		fmt.Fprintln(log, "优雅关闭超时，强制停止")
	}
	if _, err := systemctl("stop", unitName(inst)); err != nil {
		return fmt.Errorf("停止失败: %s", err)
	}
	return nil
}

// newWorld 停服 -> 备份 -> 删除世界存档 -> 启动（游戏生成新世界）
func (sv *Server) newWorld(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate) error {
	if len(tmpl.WorldPaths) == 0 {
		return fmt.Errorf("该游戏模板不支持创建新世界")
	}
	st := serviceStatus(inst)
	wasRunning := st.ActiveState == "active"
	if wasRunning {
		fmt.Fprintln(log, "停止服务器...")
		if err := sv.gracefulStop(ctx, log, inst, tmpl); err != nil {
			return err
		}
	}
	fmt.Fprintln(log, "备份当前存档（可用于恢复旧世界）...")
	if _, err := createBackup(ctx, log, inst, tmpl, 10); err != nil {
		return fmt.Errorf("备份失败，已中止（未删除存档）: %w", err)
	}
	for _, p := range tmpl.WorldPaths {
		if p == "" || strings.Contains(p, "..") {
			return fmt.Errorf("世界存档路径非法: %q", p)
		}
		fmt.Fprintf(log, "删除世界存档 %s ...\n", p)
		if err := os.RemoveAll(inst.Dir + "/" + p); err != nil {
			return fmt.Errorf("删除 %s: %w", p, err)
		}
	}
	if wasRunning {
		fmt.Fprintln(log, "启动服务器（将生成新世界）...")
		if err := sv.applySavedConfig(inst, tmpl); err != nil {
			return err
		}
		if _, err := systemctl("start", unitName(inst)); err != nil {
			return fmt.Errorf("启动失败: %s", err)
		}
	} else {
		fmt.Fprintln(log, "世界存档已删除，下次启动时生成新世界")
	}
	return nil
}

// updateInstance 预检版本（已是最新则不动服务器）-> 停服 -> steamcmd validate -> 重新生成脚本/配置 -> 启动。
// 预检失败（查不到版本）时照旧更新：宁可白停一次，不可因第三方接口故障更不上。
func (sv *Server) updateInstance(ctx context.Context, log io.Writer, inst *Instance, tmpl *GameTemplate) error {
	if tmpl.SteamAppID > 0 {
		if local, err := localBuildID(inst.Dir, tmpl.SteamAppID); err != nil {
			fmt.Fprintf(log, "读取本地版本失败（%v），继续执行更新\n", err)
		} else if latest, err := latestBuildID(tmpl.SteamAppID); err != nil {
			fmt.Fprintf(log, "查询最新版本失败（%v），继续执行更新\n", err)
		} else if latest <= local {
			fmt.Fprintf(log, "当前已是最新版本（build %d），无需更新\n", local)
			return nil
		} else {
			fmt.Fprintf(log, "发现新版本：build %d（当前 %d）\n", latest, local)
		}
	}
	st := serviceStatus(inst)
	wasRunning := st.ActiveState == "active"
	if wasRunning {
		fmt.Fprintln(log, "停止服务器...")
		if err := sv.gracefulStop(ctx, log, inst, tmpl); err != nil {
			return err
		}
	}
	if err := runSteamcmdApp(ctx, log, tmpl.SteamAppID, inst.Dir); err != nil {
		return err
	}
	if err := sv.postInstall(inst, tmpl, log); err != nil {
		return err
	}
	if wasRunning {
		fmt.Fprintln(log, "重新启动服务器...")
		if err := sv.applySavedConfig(inst, tmpl); err != nil {
			return err
		}
		if _, err := systemctl("start", unitName(inst)); err != nil {
			return fmt.Errorf("启动失败: %s", err)
		}
	}
	return nil
}
