package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"
)

// ---------- Source RCON 协议（Valve），Palworld 等 UE/Source 游戏通用 ----------

const (
	rconAuth        = 3
	rconAuthResp    = 2
	rconExec        = 2
	rconExecResp    = 0
	rconMaxBody     = 4096
	rconDialTimeout = 5 * time.Second
)

func rconPacket(id, typ int32, body string) []byte {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, int32(len(body)+10))
	binary.Write(buf, binary.LittleEndian, id)
	binary.Write(buf, binary.LittleEndian, typ)
	buf.WriteString(body)
	buf.Write([]byte{0, 0})
	return buf.Bytes()
}

func rconRead(conn net.Conn) (int32, int32, string, error) {
	var lenBuf [4]byte
	if _, err := readFull(conn, lenBuf[:]); err != nil {
		return 0, 0, "", err
	}
	length := int(binary.LittleEndian.Uint32(lenBuf[:]))
	if length < 10 || length > rconMaxBody+10 {
		return 0, 0, "", fmt.Errorf("非法 RCON 包长度 %d", length)
	}
	payload := make([]byte, length)
	if _, err := readFull(conn, payload); err != nil {
		return 0, 0, "", err
	}
	id := int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ := int32(binary.LittleEndian.Uint32(payload[4:8]))
	body := string(bytes.TrimRight(payload[8:], "\x00"))
	return id, typ, body, nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// RconExec 执行一条 RCON 命令并返回响应
func RconExec(addr, password, command string) (string, error) {
	conn, err := net.DialTimeout("tcp", addr, rconDialTimeout)
	if err != nil {
		return "", fmt.Errorf("RCON 连接失败: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write(rconPacket(1, rconAuth, password)); err != nil {
		return "", err
	}
	// 认证响应：可能先收到一个空的 SERVERDATA_RESPONSE_VALUE，再收到 auth 响应
	authed := false
	for i := 0; i < 4; i++ {
		id, typ, _, err := rconRead(conn)
		if err != nil {
			return "", fmt.Errorf("RCON 认证读包失败: %w", err)
		}
		if id == -1 {
			return "", fmt.Errorf("RCON 密码错误")
		}
		if typ == rconAuthResp && id == 1 {
			authed = true
			break
		}
	}
	if !authed {
		return "", fmt.Errorf("RCON 认证失败")
	}

	if _, err := conn.Write(rconPacket(2, rconExec, command)); err != nil {
		return "", err
	}
	// 读响应：直到读空闲超时为止拼接。
	// 注意：Palworld 的响应包 id 恒为 0（不回显请求 id），因此按 type 匹配。
	var out bytes.Buffer
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, typ, body, err := rconRead(conn)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			if out.Len() > 0 {
				break
			}
			return "", fmt.Errorf("RCON 读取响应失败: %w", err)
		}
		if typ == rconExecResp {
			out.WriteString(body)
		}
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	}
	return out.String(), nil
}
