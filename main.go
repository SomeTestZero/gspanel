package main

import (
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

//go:embed static templates
var embedded embed.FS

const (
	GamesHome    = "/home/games"
	InstancesDir = GamesHome + "/instances"
	BackupsDir   = GamesHome + "/backups"
	SteamcmdDir  = GamesHome + "/steamcmd"
	GamesUser    = "games"
)

// BaseDir 取二进制所在目录：clone 到任意位置都可运行，
// 面板状态（data/）与用户模板（templates/）跟随二进制。
var BaseDir = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "/root/gspanel"
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	return filepath.Dir(exe)
}()

var DataDir = BaseDir + "/data"

type Server struct {
	state     *State
	tplMu     sync.RWMutex
	templates map[string]*GameTemplate
	tasks     *TaskManager
	monitor   *Monitor
	sched     *Scheduler
	events    *EventLog
	http      *http.Server

	watchMu sync.Mutex
	watch   map[string]*watchState // 看门狗：各实例上次的进程启动时间戳与在/离线状态

	ipMu      sync.Mutex
	ipCache   string
	ipCacheAt time.Time
}

// getTemplate 按 ID 取模板（导入功能会在线增删，需加锁）
func (sv *Server) getTemplate(id string) *GameTemplate {
	sv.tplMu.RLock()
	defer sv.tplMu.RUnlock()
	return sv.templates[id]
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := os.MkdirAll(DataDir, 0700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	state, generatedPassword, err := LoadState(DataDir + "/config.json")
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	if generatedPassword != "" {
		log.Printf("==================================================")
		log.Printf(" 首次启动，生成的管理员密码: %s", generatedPassword)
		log.Printf(" 请登录后立即在「设置」中修改密码")
		log.Printf("==================================================")
	}

	templates, err := LoadTemplates(embedded, BaseDir+"/templates")
	if err != nil {
		log.Fatalf("load templates: %v", err)
	}
	log.Printf("loaded %d game templates", len(templates))

	tm := NewTaskManager()
	s := &Server{
		state:     state,
		templates: templates,
		tasks:     tm,
		monitor:   NewMonitor(state),
		sched:     NewScheduler(state, tm),
		events:    LoadEventLog(DataDir + "/events.jsonl"),
		watch:     map[string]*watchState{},
	}
	tm.OnStart = s.onTaskStart
	tm.OnFinish = s.onTaskFinish
	s.monitor.Start()
	s.sched.Start(s)

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	addr := state.BindAddr()
	s.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.events.Add("", "panel", "面板已启动，监听 http://%s", addr)

	go func() {
		log.Printf("gspanel listening on http://%s", addr)
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	s.sched.Stop()
	_ = s.http.Close()
}
