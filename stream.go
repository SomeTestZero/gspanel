package main

import (
	"fmt"
	"net/http"
	"time"
)

// handleTaskStream SSE 推送任务日志：先发存量，再推增量，任务结束发 done 事件
func (sv *Server) handleTaskStream(w http.ResponseWriter, r *http.Request) {
	t := sv.tasks.Get(r.PathValue("id"))
	if t == nil {
		jsonError(w, http.StatusNotFound, "任务不存在")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, http.StatusInternalServerError, "不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	writeSSE(w, t.Log())
	flusher.Flush()

	t.mu.Lock()
	finished := t.Status != "running"
	t.mu.Unlock()
	if finished {
		fmt.Fprintf(w, "event: done\ndata: %s\n\n", t.Status)
		flusher.Flush()
		return
	}

	ch := t.subscribe()
	defer t.unsubscribe(ch)
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case data, ok := <-ch:
			if !ok { // 任务结束，通道关闭
				fmt.Fprintf(w, "event: done\ndata: %s\n\n", t.Status)
				flusher.Flush()
				return
			}
			writeSSE(w, data)
			flusher.Flush()
		}
	}
}
