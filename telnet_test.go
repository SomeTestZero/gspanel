package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTelnet 模拟 7DTD telnet：欢迎语 -> 密码提示 -> 命令响应
func fakeTelnet(t *testing.T, password string, respond func(cmd string) string) (addr string, done func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
				fmt.Fprintf(rw, "*** Connected with 7DTD server.\n\n")
				rw.Flush()
				if password != "" {
					fmt.Fprintf(rw, "password required, please enter: ")
					rw.Flush()
					line, _ := rw.ReadString('\n')
					if strings.TrimSpace(line) != password {
						fmt.Fprintf(rw, "Password incorrect\n")
						rw.Flush()
						return
					}
					fmt.Fprintf(rw, "*** Logged in. Press 'help' for commands.\n")
					rw.Flush()
				}
				for {
					line, err := rw.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimSpace(line)
					fmt.Fprint(rw, respond(cmd))
					rw.Flush()
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestTelnetExecNoPassword(t *testing.T) {
	addr, done := fakeTelnet(t, "", func(cmd string) string {
		return fmt.Sprintf("2026-09-30 INF Executing command '%s'\nresult of %s\n", cmd, cmd)
	})
	defer done()
	out, err := telnetExec(addr, "", "lp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "result of lp") {
		t.Fatalf("响应缺失: %q", out)
	}
}

func TestTelnetExecWithPassword(t *testing.T) {
	addr, done := fakeTelnet(t, "secret", func(cmd string) string {
		return "ok:" + cmd + "\n"
	})
	defer done()
	out, err := telnetExec(addr, "secret", "saveworld")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ok:saveworld") {
		t.Fatalf("响应缺失: %q", out)
	}
	if _, err := telnetExec(addr, "wrong", "lp"); err == nil {
		t.Fatal("密码错误应返回错误")
	}
}

// recordTelnet 模拟 7DTD telnet 并记录收到的每一行（含密码行，前缀 pw:），
// 收到 exit 时像真服务端一样自行关闭连接
func recordTelnet(t *testing.T, password string) (addr string, lines func() []string, done func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	rec := func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				fmt.Fprintf(c, "*** Connected with 7DTD server.\n\n")
				if password != "" {
					fmt.Fprintf(c, "password required, please enter: ")
					line, _ := r.ReadString('\n')
					s := strings.TrimSpace(line)
					rec("pw:" + s)
					if s != password {
						fmt.Fprintf(c, "Password incorrect\n")
						return
					}
					fmt.Fprintf(c, "*** Logged in.\n")
				}
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					cmd := strings.TrimSpace(line)
					rec(cmd)
					if cmd == "exit" {
						return // 真服务端：exit 由服务端自己关闭连接
					}
					fmt.Fprintf(c, "ok:%s\n", cmd)
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}, func() { ln.Close() }
}

// TestTelnetExitOnlyAfterAuth 收尾行为：认证成功后最后一条必须是 exit（服务端干净关闭，
// 避免 IOException 刷屏）；认证失败不得发 exit（会被当成错误口令计入 TelnetFailedLoginLimit）
func TestTelnetExitOnlyAfterAuth(t *testing.T) {
	addr, lines, done := recordTelnet(t, "")
	if _, err := telnetExec(addr, "", "lp"); err != nil {
		t.Fatal(err)
	}
	done()
	if got := strings.Join(lines(), ","); got != "lp,exit" {
		t.Fatalf("无密码收尾不对: %q", got)
	}

	addr2, lines2, done2 := recordTelnet(t, "secret")
	if _, err := telnetExec(addr2, "secret", "lp"); err != nil {
		t.Fatal(err)
	}
	if _, err := telnetExec(addr2, "wrong", "lp"); err == nil {
		t.Fatal("密码错误应返回错误")
	}
	done2()
	if got := strings.Join(lines2(), ","); got != "pw:secret,lp,exit,pw:wrong" {
		t.Fatalf("密码路径收尾不对（错密后不得发 exit）: %q", got)
	}
}

func TestParseNxmLink(t *testing.T) {
	l, err := parseNxmLink("nxm://7daystodie/mods/12345/files/67890?key=abc&expires=1700000000")
	if err != nil {
		t.Fatal(err)
	}
	if l.ModID != 12345 || l.FileID != 67890 || l.Key != "abc" || l.Expires != 1700000000 {
		t.Fatalf("解析结果不对: %+v", l)
	}
	if _, err := parseNxmLink("https://example.com/x"); err == nil {
		t.Fatal("非 nxm 链接应报错")
	}
	if _, err := parseNxmLink("nxm://7daystodie/mods/x/files/1"); err == nil {
		t.Fatal("非法 mod id 应报错")
	}
}

// TestTelnetExecLive 可选：对本机真实 7DTD telnet 冒烟（GSP_TELNET_LIVE=1 且服务器开着才跑）
func TestTelnetExecLive(t *testing.T) {
	if os.Getenv("GSP_TELNET_LIVE") == "" {
		t.Skip("设置 GSP_TELNET_LIVE=1 才跑在线冒烟")
	}
	port := os.Getenv("GSP_TELNET_PORT")
	if port == "" {
		port = "8081"
	}
	out, err := telnetExec("127.0.0.1:"+port, os.Getenv("GSP_TELNET_PASSWORD"), "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Game version") && !strings.Contains(out, "Server version") {
		t.Fatalf("响应异常（%s）: %q", time.Now().Format(time.RFC3339), out)
	}
}
