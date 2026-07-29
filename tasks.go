package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// ---------- 后台任务管理：安装/更新/备份等，日志可订阅（SSE） ----------

type Task struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // install | update | backup | restore | setup-steamcmd | setup-deps | restart | stop
	Instance  string    `json:"instance,omitempty"`
	Desc      string    `json:"desc"`
	Status    string    `json:"status"` // running | success | failed | canceled
	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`

	mu     sync.Mutex
	buf    []byte
	subs   map[chan []byte]struct{}
	cancel context.CancelFunc
}

const taskLogCap = 512 * 1024

func (t *Task) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > taskLogCap {
		t.buf = t.buf[len(t.buf)-taskLogCap:]
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	for ch := range t.subs {
		select {
		case ch <- cp:
		default:
		}
	}
	t.mu.Unlock()
	return len(p), nil
}

func (t *Task) logf(format string, args ...any) {
	fmt.Fprintf(t, "[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func (t *Task) Log() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]byte, len(t.buf))
	copy(out, t.buf)
	return out
}

func (t *Task) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	t.mu.Lock()
	t.subs[ch] = struct{}{}
	t.mu.Unlock()
	return ch
}

func (t *Task) unsubscribe(ch chan []byte) {
	t.mu.Lock()
	delete(t.subs, ch)
	t.mu.Unlock()
}

func (t *Task) finish(status string) {
	t.mu.Lock()
	t.Status = status
	t.EndedAt = time.Now()
	for ch := range t.subs {
		close(ch)
	}
	t.subs = map[chan []byte]struct{}{}
	t.mu.Unlock()
}

func (t *Task) Cancel() {
	t.mu.Lock()
	c := t.cancel
	t.mu.Unlock()
	if c != nil {
		c()
	}
}

type TaskManager struct {
	mu    sync.Mutex
	tasks []*Task
	seq   int
}

func NewTaskManager() *TaskManager {
	return &TaskManager{}
}

// Run 创建并异步执行一个任务
func (m *TaskManager) Run(kind, instance, desc string, fn func(ctx context.Context, log io.Writer, t *Task) error) *Task {
	m.mu.Lock()
	m.seq++
	t := &Task{
		ID:        fmt.Sprintf("%d-%d", time.Now().Unix(), m.seq),
		Kind:      kind,
		Instance:  instance,
		Desc:      desc,
		Status:    "running",
		CreatedAt: time.Now(),
		subs:      map[chan []byte]struct{}{},
	}
	m.tasks = append([]*Task{t}, m.tasks...)
	if len(m.tasks) > 100 {
		m.tasks = m.tasks[:100]
	}
	m.mu.Unlock()

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		t.mu.Lock()
		t.cancel = cancel
		t.mu.Unlock()
		t.logf("任务开始: %s", desc)
		err := fn(ctx, t, t)
		if ctx.Err() != nil {
			t.logf("任务已取消")
			t.finish("canceled")
		} else if err != nil {
			t.logf("任务失败: %v", err)
			t.finish("failed")
		} else {
			t.logf("任务完成")
			t.finish("success")
		}
	}()
	return t
}

func (m *TaskManager) Get(id string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// HasRunningFor 该实例是否有正在运行的任务（版本轮询避免叠加更新任务）
func (m *TaskManager) HasRunningFor(instance string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tasks {
		if t.Instance == instance && t.Status == "running" {
			return true
		}
	}
	return false
}

func (m *TaskManager) List() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, len(m.tasks))
	copy(out, m.tasks)
	return out
}

// newCancellableCmd 构造可随 context 取消的命令（root 身份）
func newCancellableCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	go func() {
		<-ctx.Done()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	return cmd
}
