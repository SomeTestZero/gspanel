package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// ---------- 游戏 telnet 控制台（七日杀等无 RCON、但带 telnet 管理口的游戏）----------
//
// 协议（行式、纯文本）：连上后服务器推欢迎语；设置过密码时先要求输入密码；
// 之后每行一条命令，响应直接推送回来。响应没有稳定的结束标记，
// 因此用「静默间隔」判定一条命令的响应结束（本机回环足够可靠）。

const (
	telnetDialTimeout = 5 * time.Second
	telnetQuietGap    = 600 * time.Millisecond // 静默多久视为响应结束
	telnetFirstGap    = 300 * time.Millisecond // 欢迎语/密码提示的静默判定
	telnetMaxWait     = 20 * time.Second       // 单条命令最长收集时间
)

// telnetExec 连接 telnet 控制台执行一条命令，返回响应文本
func telnetExec(addr, password, command string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, telnetDialTimeout)
	if err != nil {
		return "", fmt.Errorf("连接 telnet %s 失败: %w", addr, err)
	}
	loggedIn := false
	defer func() {
		if loggedIn {
			telnetExit(conn) // 发 exit 让服务端干净关闭连接
			return
		}
		_ = conn.Close()
	}()

	// 欢迎语（可能含密码提示）
	greet, err := telnetReadQuiet(conn, telnetFirstGap, 5*time.Second)
	if err != nil {
		return "", fmt.Errorf("读取 telnet 欢迎语失败: %w", err)
	}
	low := strings.ToLower(string(greet))
	if strings.Contains(low, "password") {
		if _, err := fmt.Fprintf(conn, "%s\r\n", password); err != nil {
			return "", fmt.Errorf("发送 telnet 密码失败: %w", err)
		}
		auth, err := telnetReadQuiet(conn, telnetFirstGap, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("telnet 认证失败: %w", err)
		}
		al := strings.ToLower(string(auth))
		if strings.Contains(al, "incorrect") || strings.Contains(al, "invalid") || strings.Contains(al, "failed") {
			return "", fmt.Errorf("telnet 密码错误，请在「配置」页核对 TelnetPassword")
		}
		greet = append(greet, auth...)
	}

	loggedIn = true
	if _, err := fmt.Fprintf(conn, "%s\r\n", command); err != nil {
		return "", fmt.Errorf("发送命令失败: %w", err)
	}
	resp, err := telnetReadQuiet(conn, telnetQuietGap, telnetMaxWait)
	if err != nil {
		return "", fmt.Errorf("读取命令响应失败: %w", err)
	}
	out := strings.TrimSpace(string(resp))
	if out == "" {
		out = strings.TrimSpace(string(greet))
	}
	return out, nil
}

// telnetReadQuiet 读取直到静默 quietGap 或达到 overall 上限。
// overall 超时不算错误（有些命令响应很少），返回已收集内容；
// 连接被对端关闭同样返回已收集内容。
func telnetReadQuiet(conn net.Conn, quietGap, overall time.Duration) ([]byte, error) {
	deadline := time.Now().Add(overall)
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	sawData := false
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(quietGap))
		n, err := conn.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
			sawData = true
			continue
		}
		if err == nil {
			continue
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			if sawData {
				return buf.Bytes(), nil // 静默间隔到达，响应结束
			}
			continue // 还没等到任何数据，继续等
		}
		// 连接关闭/复位：返回已有内容
		return buf.Bytes(), nil
	}
	return buf.Bytes(), nil
}

// telnetExit 礼貌退出 telnet 会话：先发 exit（七日杀 telnet 支持，服务端自己关闭连接、日志只记
// 「connection closed」），再关 socket。直接 close 的话服务端下次往这条连接写数据才发现断开，
// 会把 IOException + 堆栈打进 console.log（无害但刷屏）。
// 仅认证完成后才可调用：密码提示阶段发 exit 会被当成错误口令，计入 TelnetFailedLoginLimit
// 有把本机封掉的风险。服务端不支持 exit 时顶多被回一句未知命令，读到超时即关，无副作用。
func telnetExit(conn net.Conn) {
	_, _ = fmt.Fprintf(conn, "exit\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	tmp := make([]byte, 256)
	for {
		if _, err := conn.Read(tmp); err != nil {
			break // 对端关闭（EOF）或超时即结束
		}
	}
	_ = conn.Close()
}

// ensureTelnetEnabled 安装/更新后确保 telnet 控制台开启（面板控制台命令依赖它）。
// TelnetPassword 保持现状：留空时游戏只监听本机回环（面板连 127.0.0.1 即可），最安全。
func ensureTelnetEnabled(inst *Instance, tmpl *GameTemplate, log io.Writer) {
	spec := findConfigSpecOf(tmpl, tmpl.RCON.ConfigPath)
	if spec == nil {
		return
	}
	vals, _, err := readConfigFile(inst.Dir, spec)
	if err != nil {
		fmt.Fprintf(log, "读取 %s 失败（跳过 telnet 检查）: %v\n", tmpl.RCON.ConfigPath, err)
		return
	}
	if strings.EqualFold(vals["TelnetEnabled"], "true") {
		return
	}
	if err := writeConfigFile(inst.Dir, spec, map[string]string{"TelnetEnabled": "true"}, nil); err != nil {
		fmt.Fprintf(log, "开启 TelnetEnabled 失败（控制台命令将不可用）: %v\n", err)
		return
	}
	fmt.Fprintln(log, "已开启 telnet 控制台（TelnetEnabled=true）")
}

// telnetSettings 从实例配置文件读取 telnet 设置（七日杀风格键名）
func telnetSettings(inst *Instance, tmpl *GameTemplate) (port int, password string, err error) {
	if tmpl.RCON == nil || tmpl.RCON.Type != "telnet" {
		return 0, "", fmt.Errorf("模板未配置 telnet 控制台")
	}
	spec := findConfigSpecOf(tmpl, tmpl.RCON.ConfigPath)
	if spec == nil {
		return 0, "", fmt.Errorf("模板配置里找不到 %s", tmpl.RCON.ConfigPath)
	}
	vals, _, err := readConfigFile(inst.Dir, spec)
	if err != nil {
		return 0, "", fmt.Errorf("读取 %s 失败: %w", tmpl.RCON.ConfigPath, err)
	}
	if !strings.EqualFold(vals["TelnetEnabled"], "true") {
		return 0, "", fmt.Errorf("telnet 未启用：请在「配置」页开启 TelnetEnabled 并重启")
	}
	port = 8081
	if p := strings.TrimSpace(vals["TelnetPort"]); p != "" {
		if n, e := strconv.Atoi(p); e == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	return port, vals["TelnetPassword"], nil
}

// telnetExecConfig 按实例配置执行 telnet 控制台命令（连本机回环）
func telnetExecConfig(inst *Instance, tmpl *GameTemplate, command string) (string, error) {
	port, password, err := telnetSettings(inst, tmpl)
	if err != nil {
		return "", err
	}
	return telnetExec(fmt.Sprintf("127.0.0.1:%d", port), password, command)
}
