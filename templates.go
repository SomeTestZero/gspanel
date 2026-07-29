package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ---------- 游戏模板：新增游戏只需加一个 JSON ----------

type PortSpec struct {
	Key     string `json:"key"`     // 实例 Ports 映射中的键，如 game / rcon / query
	Default int    `json:"default"`
	Proto   string `json:"proto"`   // udp | tcp
	Desc    string `json:"desc"`
	Public  bool   `json:"public"`  // 是否需要对公网开放（防火墙提示用）
}

type ConfigField struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Group   string   `json:"group,omitempty"` // 配置页分组标题（可折叠），空则归入「其他」
	Type    string   `json:"type"` // string | password | int | float | bool | select
	Default any      `json:"default,omitempty"`
	Options []string `json:"options,omitempty"` // select
	Min     *float64 `json:"min,omitempty"`     // int/float 输入约束（仅在有官方/可靠依据时填写）
	Max     *float64 `json:"max,omitempty"`
	Desc    string   `json:"desc,omitempty"`
	Note    string   `json:"note,omitempty"` // 补充说明（已知问题/注意事项）
}

type ConfigSpec struct {
	Path     string        `json:"path"`      // 相对实例目录
	Format   string        `json:"format"`    // option-settings | kv | raw
	SeedFrom string        `json:"seed_from"` // 相对实例目录，安装后若目标不存在则从它生成
	Label    string        `json:"label"`
	Schema   []ConfigField `json:"schema,omitempty"` // 为空则按 raw 文本编辑
}

type RCONSpec struct {
	Type    string `json:"type"`     // source
	PortKey string `json:"port_key"` // 对应 Ports 里的键
}

// RESTCommandSpec 把一条控制台命令映射到游戏 REST API（部分游戏 RCON 丢响应，REST 更可靠）
type RESTCommandSpec struct {
	Method string            `json:"method"` // GET | POST
	Path   string            `json:"path"`   // 如 /v1/api/players
	Format string            `json:"format,omitempty"` // players | metrics | kv，空则原样返回 body
	Body   map[string]string `json:"body,omitempty"`   // POST JSON body；值 "$arg" 会被命令参数替换
}

type RESTSpec struct {
	PortKey  string                     `json:"port_key"` // 对应 Ports 里的键
	Commands map[string]RESTCommandSpec `json:"commands"` // 键为命令动词（大小写不敏感），如 ShowPlayers
}

// ConsoleButton 控制台快捷按钮；prompt 非空时点击先弹输入框，输入内容作为命令参数
type ConsoleButton struct {
	Label   string `json:"label"`
	Command string `json:"command"`
	Prompt  string `json:"prompt,omitempty"`
}

type GameTemplate struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Description    string       `json:"description"`
	SteamAppID     int          `json:"steam_app_id"`
	AnonymousLogin bool         `json:"anonymous_login"`
	Executable     string       `json:"executable"`      // 相对实例目录的启动命令
	DefaultArgs    []string     `json:"default_args"`
	StopMode       string       `json:"stop_mode"`       // rcon | sigterm
	StopWarnSecs   int          `json:"stop_warn_secs"`  // rcon 停止前提前广播秒数
	RCON           *RCONSpec    `json:"rcon,omitempty"`
	RestAPI        *RESTSpec    `json:"rest_api,omitempty"` // 可选：部分命令改走游戏 REST API（RCON 丢响应的游戏用）
	ConsoleButtons []ConsoleButton `json:"console_buttons,omitempty"` // 可选：控制台快捷按钮，缺省用内置默认
	Ports          []PortSpec   `json:"ports"`
	Configs        []ConfigSpec `json:"configs,omitempty"`
	BackupPaths    []string     `json:"backup_paths"`            // 相对实例目录
	WorldPaths     []string     `json:"world_paths,omitempty"`   // 世界存档路径（相对实例目录，「创建新世界」时删除）
	Notes          string       `json:"notes,omitempty"`
}

func LoadTemplates(embedded embed.FS, userDir string) (map[string]*GameTemplate, error) {	out := map[string]*GameTemplate{}

	load := func(data []byte, src string) error {
		var t GameTemplate
		if err := json.Unmarshal(data, &t); err != nil {
			return fmt.Errorf("模板 %s 解析失败: %w", src, err)
		}
		if t.ID == "" || t.SteamAppID == 0 || t.Executable == "" {
			return fmt.Errorf("模板 %s 缺少 id/steam_app_id/executable", src)
		}
		out[t.ID] = &t
		return nil
	}

	entries, err := fs.ReadDir(embedded, "templates")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := embedded.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, err
		}
		if err := load(data, "内置:"+e.Name()); err != nil {
			return nil, err
		}
	}

	// 用户自定义模板（可选，同 ID 覆盖内置）
	if userEntries, err := os.ReadDir(userDir); err == nil {
		for _, e := range userEntries {
			if filepath.Ext(e.Name()) != ".json" || e.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join(userDir, e.Name()))
			if err != nil {
				return nil, err
			}
			if err := load(data, e.Name()); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// ---------- 模板校验与在线导入 ----------

var templateIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

func validateTemplate(t *GameTemplate) error {
	if !templateIDRe.MatchString(t.ID) {
		return fmt.Errorf("模板 id 须为小写字母/数字/中划线")
	}
	if t.Name == "" {
		return fmt.Errorf("缺少 name")
	}
	if t.SteamAppID <= 0 {
		return fmt.Errorf("steam_app_id 非法")
	}
	if t.Executable == "" {
		return fmt.Errorf("缺少 executable")
	}
	if t.StopMode != "" && t.StopMode != "rcon" && t.StopMode != "sigterm" {
		return fmt.Errorf("stop_mode 须为 rcon/sigterm")
	}
	seen := map[string]bool{}
	for _, p := range t.Ports {
		if p.Key == "" || p.Default < 1 || p.Default > 65535 {
			return fmt.Errorf("端口定义非法: %+v", p)
		}
		if seen[p.Key] {
			return fmt.Errorf("端口键重复: %s", p.Key)
		}
		seen[p.Key] = true
		if p.Proto != "udp" && p.Proto != "tcp" {
			return fmt.Errorf("端口 %s 协议须为 udp/tcp", p.Key)
		}
	}
	if t.RCON != nil {
		if !seen[t.RCON.PortKey] {
			return fmt.Errorf("rcon.port_key %q 未在 ports 中定义", t.RCON.PortKey)
		}
		if t.RCON.Type != "source" {
			return fmt.Errorf("暂只支持 source 类型 RCON")
		}
	}
	if t.RestAPI != nil {
		if !seen[t.RestAPI.PortKey] {
			return fmt.Errorf("rest_api.port_key %q 未在 ports 中定义", t.RestAPI.PortKey)
		}
		for name, c := range t.RestAPI.Commands {
			if c.Method != http.MethodGet && c.Method != http.MethodPost {
				return fmt.Errorf("rest_api 命令 %s 的 method 须为 GET/POST", name)
			}
			if !strings.HasPrefix(c.Path, "/") {
				return fmt.Errorf("rest_api 命令 %s 的 path 须以 / 开头", name)
			}
		}
	}
	for _, b := range t.ConsoleButtons {
		if b.Label == "" || b.Command == "" {
			return fmt.Errorf("console_buttons 存在缺 label/command 的按钮")
		}
	}
	for _, c := range t.Configs {
		if c.Path == "" || strings.Contains(c.Path, "..") {
			return fmt.Errorf("配置路径非法: %q", c.Path)
		}
		switch c.Format {
		case "option-settings", "kv", "raw":
		default:
			return fmt.Errorf("配置 %s 的 format 须为 option-settings/kv/raw", c.Path)
		}
	}
	return nil
}

// importTemplateFromURL 从 URL 下载模板 JSON 并注册（同时落盘到模板目录）
func (sv *Server) importTemplateFromURL(rawURL string) (*GameTemplate, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("仅支持 http/https 链接")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var t GameTemplate
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("模板 JSON 解析失败: %w", err)
	}
	if err := validateTemplate(&t); err != nil {
		return nil, fmt.Errorf("模板校验失败: %w", err)
	}
	// 落盘（目录即面板启动时加载的用户模板目录）
	pretty, _ := json.MarshalIndent(&t, "", "  ")
	dst := filepath.Join(BaseDir, "templates", t.ID+".json")
	if err := os.WriteFile(dst, pretty, 0644); err != nil {
		return nil, fmt.Errorf("保存模板文件失败: %w", err)
	}
	sv.tplMu.Lock()
	sv.templates[t.ID] = &t
	sv.tplMu.Unlock()
	return &t, nil
}
