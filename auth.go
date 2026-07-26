package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"time"
)

const sessionCookie = "gspanel_session"
const sessionTTL = 7 * 24 * time.Hour

func (s *State) newSession() string {
	token := randomToken(32)
	s.mu.Lock()
	s.Sessions[token] = time.Now().Add(sessionTTL)
	// 顺带清理过期会话
	for t, exp := range s.Sessions {
		if time.Now().After(exp) {
			delete(s.Sessions, t)
		}
	}
	_ = s.saveLocked() // 持久化：面板重启后会话仍然有效
	s.mu.Unlock()
	return token
}

func (s *State) validSession(token string) bool {
	s.mu.RLock()
	exp, ok := s.Sessions[token]
	s.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

func (s *State) dropSession(token string) {
	s.mu.Lock()
	delete(s.Sessions, token)
	_ = s.saveLocked()
	s.mu.Unlock()
}

// allowLogin 简单限流：10 分钟内失败 5 次锁定 10 分钟
func (s *State) allowLogin(ip string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	fails := s.loginFail[ip]
	cutoff := time.Now().Add(-10 * time.Minute)
	keep := fails[:0]
	for _, t := range fails {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	s.loginFail[ip] = keep
	return len(keep) < 5
}

func (s *State) recordLoginFail(ip string) {
	s.loginMu.Lock()
	s.loginFail[ip] = append(s.loginFail[ip], time.Now())
	s.loginMu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (sv *Server) tokenFrom(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	// 兼容 curl: Authorization: Bearer <token>
	const p = "Bearer "
	if h := r.Header.Get("Authorization"); len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

// auth 中间件：保护 /api/*（除登录）
func (sv *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := sv.tokenFrom(r)
		if tok == "" || !sv.state.validSession(tok) {
			jsonError(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		next(w, r)
	}
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// subtleCompare 防时序攻击比较（登录路径使用）
func subtleCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
