package main

import (
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ---------- 公网地址探测：云厂商元数据优先，公网服务兜底，支持手动覆盖 ----------

var ipLookupURLs = []string{
	"http://100.96.0.96/latest/meta-data/eipv4",     // 火山引擎元数据
	"http://100.100.100.200/latest/meta-data/eipv4", // 阿里云元数据
	"https://api.ipify.org",
	"http://ifconfig.me/ip",
	"http://ip.sb",
}

var domainRe = regexp.MustCompile(`^([a-zA-Z0-9-]+\.)+[a-zA-Z]{2,}$`)

// publicIP 返回生效的公网地址：手动覆盖 > 自动探测（带缓存）
func (sv *Server) publicIP() string {
	sv.state.mu.RLock()
	override := strings.TrimSpace(sv.state.PublicIP)
	sv.state.mu.RUnlock()
	if override != "" {
		return override
	}

	sv.ipMu.Lock()
	defer sv.ipMu.Unlock()
	ttl := 10 * time.Minute
	if sv.ipCache == "" {
		ttl = time.Minute // 探测失败时缩短重试间隔
	}
	if time.Since(sv.ipCacheAt) < ttl {
		return sv.ipCache
	}
	client := &http.Client{Timeout: 2 * time.Second}
	for _, u := range ipLookupURLs {
		resp, err := client.Get(u)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			sv.ipCache = ip
			sv.ipCacheAt = time.Now()
			return ip
		}
	}
	sv.ipCache = ""
	sv.ipCacheAt = time.Now()
	return ""
}

// validPublicAddr 允许 IPv4/IPv6 或域名（DDNS 场景）
func validPublicAddr(v string) bool {
	return net.ParseIP(v) != nil || domainRe.MatchString(v)
}
