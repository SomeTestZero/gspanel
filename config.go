package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"
)

// ---------- 实例与计划任务模型 ----------

type Schedule struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // restart | backup | update
	Type      string    `json:"type"` // daily | interval
	Time      string    `json:"time"` // daily: "04:30"
	Hours     int       `json:"hours,omitempty"`
	Enabled   bool      `json:"enabled"`
	Retention int       `json:"retention,omitempty"` // backup: 保留份数
	LastRun   time.Time `json:"last_run,omitempty"`
}

type Instance struct {
	Name          string            `json:"name"`
	Template      string            `json:"template"`
	DisplayName   string            `json:"display_name"`
	Dir           string            `json:"dir"`
	Ports         map[string]int    `json:"ports"` // 端口键 -> 实际端口
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env,omitempty"`
	AdminPassword string            `json:"admin_password,omitempty"` // 游戏内管理员/RCON 密码
	// 面板侧保存的配置快照：配置文件路径 -> 键 -> 值。
	// 部分游戏（如 Palworld）关机时会用内存中的配置重写配置文件，
	// 因此每次启动前以此快照为准重新写入。
	ConfigValues map[string]map[string]string `json:"config_values,omitempty"`
	Installed    bool                         `json:"installed"`
	AutoUpdate   bool                         `json:"auto_update,omitempty"` // 轮询检测到新版本时自动更新
	Schedules     []*Schedule       `json:"schedules,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

var instanceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,29}$`)

// ---------- 面板全局状态 ----------

type State struct {
	mu           sync.RWMutex
	path         string
	Bind         string `json:"bind"`
	Port         int    `json:"port"`
	PublicIP     string `json:"public_ip,omitempty"` // 手动覆盖公网地址，留空自动探测
	PasswordSalt string `json:"password_salt"`
	PasswordHash string `json:"password_hash"`
	Instances    map[string]*Instance `json:"instances"`
	Sessions     map[string]time.Time `json:"sessions,omitempty"` // 持久化会话：面板重启不踢人

	loginMu  sync.Mutex
	loginFail map[string][]time.Time // IP -> 失败时间（内存态）
}

func (s *State) BindAddr() string {
	return fmt.Sprintf("%s:%d", s.Bind, s.Port)
}

func hashPassword(salt, password string) string {
	h := sha256.Sum256([]byte(salt + ":" + password))
	return hex.EncodeToString(h[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func LoadState(path string) (*State, string, error) {
	s := &State{
		path:      path,
		Instances: map[string]*Instance{},
		Sessions:  map[string]time.Time{},
		loginFail: map[string][]time.Time{},
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// 首次启动：生成随机管理员密码
		pw := randomToken(8)
		s.Bind = "0.0.0.0"
		s.Port = 8800
		s.PasswordSalt = randomToken(16)
		s.PasswordHash = hashPassword(s.PasswordSalt, pw)
		if err := s.saveLocked(); err != nil {
			return nil, "", err
		}
		return s, pw, nil
	}
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, "", fmt.Errorf("parse config: %w", err)
	}
	if s.Instances == nil {
		s.Instances = map[string]*Instance{}
	}
	if s.Sessions == nil {
		s.Sessions = map[string]time.Time{}
	}
	// 清理过期会话
	for t, exp := range s.Sessions {
		if time.Now().After(exp) {
			delete(s.Sessions, t)
		}
	}
	s.path = path
	s.loginFail = map[string][]time.Time{}
	return s, "", nil
}

// Save 持久化（调用方需已持有锁或使用 SaveUnlocked 包装）
func (s *State) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *State) saveLocked() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// CheckPassword 校验并返回是否成功
func (s *State) CheckPassword(pw string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return hashPassword(s.PasswordSalt, pw) == s.PasswordHash
}

func (s *State) SetPassword(pw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PasswordSalt = randomToken(16)
	s.PasswordHash = hashPassword(s.PasswordSalt, pw)
	return s.saveLocked()
}
