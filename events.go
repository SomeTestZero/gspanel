package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------- 事件日志：面板与实例的关键动作时间线 ----------
// 记录启停/更新/备份/计划任务/异常退出等事件，追加写入 data/events.jsonl（每行一条 JSON），
// 内存保留最近 eventKeep 条供 API 查询；文件超过 eventFileMax 时按内存内容整体重写。

type Event struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance,omitempty"` // 空 = 面板自身
	Kind     string    `json:"kind"`              // start/stop/restart/crash/auto-update/scheduled-*/backup ...
	Message  string    `json:"message"`
}

const (
	eventKeep    = 500       // 内存与 API 返回的事件条数上限
	eventFileMax = 256 << 10 // events.jsonl 体积上限，超过则重写截断
)

type EventLog struct {
	mu     sync.Mutex
	events []Event // 新的在前
	path   string
}

func LoadEventLog(path string) *EventLog {
	l := &EventLog{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return l
	}
	lines := strings.Split(string(data), "\n")
	// 文件按时间追加（旧→新），内存保持新→旧，只取尾部 eventKeep 条
	for i := len(lines) - 1; i >= 0 && len(l.events) < eventKeep; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e Event
		if json.Unmarshal([]byte(line), &e) == nil {
			l.events = append(l.events, e)
		}
	}
	return l
}

// Add 记录一条事件；instance 为空表示面板级事件
func (l *EventLog) Add(instance, kind, format string, args ...any) {
	e := Event{Time: time.Now(), Instance: instance, Kind: kind, Message: fmt.Sprintf(format, args...)}
	line, _ := json.Marshal(e)
	l.mu.Lock()
	l.events = append([]Event{e}, l.events...)
	if len(l.events) > eventKeep {
		l.events = l.events[:eventKeep]
	}
	l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	fi, statErr := f.Stat()
	_ = f.Close()
	if statErr == nil && fi.Size() > eventFileMax {
		l.rewrite()
	}
}

// rewrite 用内存中的事件整体重写文件，限制体积增长；
// 从最新的事件往回装到 eventFileMax 预算为止（宁可丢旧事件），再按旧→新写回
func (l *EventLog) rewrite() {
	l.mu.Lock()
	var lines [][]byte
	size := 0
	for i := 0; i < len(l.events); i++ {
		line, err := json.Marshal(l.events[i])
		if err != nil {
			continue
		}
		if len(lines) > 0 && size+len(line)+1 > eventFileMax {
			break
		}
		lines = append(lines, line)
		size += len(line) + 1
	}
	var b strings.Builder
	for i := len(lines) - 1; i >= 0; i-- {
		b.Write(lines[i])
		b.WriteByte('\n')
	}
	content := b.String()
	l.mu.Unlock()
	_ = os.WriteFile(l.path, []byte(content), 0600)
}

// List 新→旧返回；instance 非空时只取该实例
func (l *EventLog) List(instance string, limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, 0, min(limit, len(l.events)))
	for _, e := range l.events {
		if instance != "" && e.Instance != instance {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// Len 内存中的事件条数
func (l *EventLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// RecentFor 该实例最近 d 内是否有事件（看门狗据此区分面板操作与意外退出）
func (l *EventLog) RecentFor(instance string, d time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-d)
	for _, e := range l.events { // 新→旧，遇到超出窗口的即可结束
		if e.Time.Before(cutoff) {
			break
		}
		if e.Instance == instance {
			return true
		}
	}
	return false
}

// ---------- 任务事件：后台任务（安装/更新/备份/定时任务等）的开始与结束自动入日志 ----------

func (sv *Server) onTaskStart(t *Task) {
	sv.events.Add(t.Instance, t.Kind, "任务开始：%s", t.Desc)
}

func (sv *Server) onTaskFinish(t *Task) {
	d := t.EndedAt.Sub(t.CreatedAt).Round(time.Second)
	switch t.Status {
	case "success":
		sv.events.Add(t.Instance, t.Kind, "任务完成：%s（耗时 %s）", t.Desc, d)
	case "failed":
		sv.events.Add(t.Instance, t.Kind, "任务失败：%s —— %s", t.Desc, t.Err)
	case "canceled":
		sv.events.Add(t.Instance, t.Kind, "任务已取消：%s（运行 %s）", t.Desc, d)
	}
}

// ---------- 看门狗：识别非面板发起的进程退出/重启 ----------
// 依据 unit 的 ActiveEnterTimestampMonotonic（每次进入 active 都会变）判断进程（重）启动。
// 面板自己的操作要么有任务在跑（HasRunningFor），要么刚写过事件（RecentFor），据此排除误报。

type watchState struct {
	init   bool
	lastTs uint64
	up     bool
}

func upState(s string) bool { return s == "active" || s == "activating" }

// watchInstances 每轮 scheduler tick 调用
func (sv *Server) watchInstances() {
	sv.state.mu.RLock()
	insts := make([]*Instance, 0, len(sv.state.Instances))
	for _, inst := range sv.state.Instances {
		if inst.Installed {
			insts = append(insts, inst)
		}
	}
	sv.state.mu.RUnlock()

	for _, inst := range insts {
		st := serviceStatus(inst)
		if !st.ExecStartOK {
			continue // unit 不存在（安装不完整）
		}
		up := upState(st.ActiveState)
		sv.watchMu.Lock()
		w := sv.watch[inst.Name]
		if w == nil {
			w = &watchState{}
			sv.watch[inst.Name] = w
		}
		if !w.init { // 面板启动后的首轮只记录基线，不产生事件
			w.init, w.lastTs, w.up = true, st.ActiveEnterTs, up
			sv.watchMu.Unlock()
			continue
		}
		expected := sv.tasks.HasRunningFor(inst.Name) || sv.events.RecentFor(inst.Name, 2*time.Minute)
		if st.ActiveEnterTs != 0 && st.ActiveEnterTs != w.lastTs {
			if w.up && up && !expected {
				sv.events.Add(inst.Name, "crash", "进程意外退出，已被 systemd 自动拉起")
			}
			w.lastTs = st.ActiveEnterTs
		}
		if w.up && !up && !expected {
			if st.ActiveState == "failed" {
				sv.events.Add(inst.Name, "failed", "服务进入 failed 状态（反复退出），请到控制台查看日志")
			} else {
				sv.events.Add(inst.Name, "exit", "进程已退出（非面板操作，如游戏内 Shutdown），未自动拉起")
			}
		}
		w.up = up
		sv.watchMu.Unlock()
	}
}
