package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------- 游戏 REST 管理 API ----------
// 部分游戏（如 Palworld）的 RCON 实现会丢失命令响应，官方 REST API 则稳定可靠。
// 模板可通过 rest_api.commands 把指定控制台命令路由到这里。

func restExec(addr, password string, spec RESTCommandSpec, arg string) (string, error) {
	var bodyReader io.Reader
	if spec.Body != nil {
		body := map[string]string{}
		needArg := false
		for k, v := range spec.Body {
			if v == "$arg" {
				needArg = true
				v = arg
			}
			body[k] = v
		}
		if needArg && arg == "" {
			return "", fmt.Errorf("该命令需要参数（在命令后空格跟上内容）")
		}
		data, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(spec.Method, "http://"+addr+spec.Path, bodyReader)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth("admin", password)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("REST API 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("REST API 读取响应失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return "", fmt.Errorf("REST API 返回 HTTP %d: %s", resp.StatusCode, msg)
	}
	switch spec.Format {
	case "players":
		return formatRESTPlayers(data), nil
	case "metrics":
		return formatRESTMetrics(data), nil
	case "kv":
		return formatRESTKV(data), nil
	}
	out := strings.TrimSpace(string(data))
	if out == "" {
		out = "(已执行，无返回内容)"
	}
	return out, nil
}

// formatRESTPlayers 把 {"players":[{name,level,ping,userId,...}]} 格式化为易读列表
func formatRESTPlayers(data []byte) string {
	var v struct {
		Players []struct {
			Name   string  `json:"name"`
			Level  int     `json:"level"`
			Ping   float64 `json:"ping"`
			UserID string  `json:"userId"`
		} `json:"players"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	if len(v.Players) == 0 {
		return "当前没有玩家在线"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "在线玩家 %d 人:", len(v.Players))
	for _, p := range v.Players {
		fmt.Fprintf(&b, "\n%s (Lv.%d, 延迟 %.0fms, %s)", p.Name, p.Level, p.Ping, p.UserID)
	}
	return b.String()
}

// formatRESTMetrics 把 Palworld /v1/api/metrics 格式化为易读指标
func formatRESTMetrics(data []byte) string {
	var v struct {
		CurrentPlayers int     `json:"currentplayernum"`
		MaxPlayers     int     `json:"maxplayernum"`
		ServerFPS      int     `json:"serverfps"`
		ServerFPSAvg   float64 `json:"serverfpsaverage"`
		Days           int     `json:"days"`
		BaseCampNum    int     `json:"basecampnum"`
		Uptime         int     `json:"uptime"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	up := time.Duration(v.Uptime) * time.Second
	uptime := fmt.Sprintf("%d 小时 %d 分", int(up.Hours()), int(up.Minutes())%60)
	if d := int(up.Hours()) / 24; d > 0 {
		uptime = fmt.Sprintf("%d 天 %d 小时", d, int(up.Hours())%24)
	}
	return fmt.Sprintf("在线: %d/%d 人\n服务器FPS: %d (平均 %.0f)\n游戏内天数: %d\n据点数量: %d\n持续运行: %s",
		v.CurrentPlayers, v.MaxPlayers, v.ServerFPS, v.ServerFPSAvg, v.Days, v.BaseCampNum, uptime)
}

// formatRESTKV 把扁平 JSON 对象格式化为 key: value 行
func formatRESTKV(data []byte) string {
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return string(data)
	}
	var b strings.Builder
	for k, val := range v {
		fmt.Fprintf(&b, "%s: %v\n", k, val)
	}
	return strings.TrimRight(b.String(), "\n")
}
