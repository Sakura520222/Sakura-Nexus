package webapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Sakura520222/Sakura-Nexus/internal/logging"
)

// WS 日志流（04 §5）：同源 Cookie 鉴权（requireSession）+ Origin 校验，
// 连接即回放环形缓冲快照，随后实时推送；客户端可发 subscribe 更新过滤
//（levels/components/keyword，过滤在服务端做）与 ping。
// 仅实时流走 WS；CRUD/配置/统计一律 REST（ADR-003 边界）。

// WithLogRing 注入环形缓冲并注册 GET /api/ws（04 §5）。nil = 不注册。
func WithLogRing(ring *logging.Ring) ServerOption {
	return func(s *Server) {
		s.ring = ring
		s.Handle("GET", "/api/ws", s.handleWS)
	}
}

// wsFilter 是 subscribe 消息的服务端过滤条件。
type wsFilter struct {
	Levels     []string `json:"levels,omitempty"`
	Components []string `json:"components,omitempty"`
	Keyword    string   `json:"keyword,omitempty"`
}

func (f *wsFilter) match(rec logging.Record) bool {
	if len(f.Levels) > 0 {
		matched := false
		for _, l := range f.Levels {
			if strings.EqualFold(l, levelName(rec.Level)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(f.Components) > 0 {
		matched := false
		for _, c := range f.Components {
			if strings.EqualFold(c, rec.Component) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if f.Keyword != "" && !strings.Contains(rec.Message, f.Keyword) {
		return false
	}
	return true
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// handleWS 升级连接并驱动读/写两个泵（ctx 取消即关闭）。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.ring == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "日志流未接线")
		return
	}
	// Origin 校验：拒绝跨域连接（04 §5；不传 token，同源 Cookie 已验）。
	if origin := r.Header.Get("Origin"); origin != "" && !strings.HasPrefix(origin, "http://"+r.Host) && !strings.HasPrefix(origin, "https://"+r.Host) {
		s.writeError(w, http.StatusForbidden, "FORBIDDEN", "跨域连接被拒绝")
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 写泵：快照回放 + 订阅实时流（过滤器可被读泵更新；局部互斥保护）。
	var filterMu sync.Mutex
	var filter wsFilter
	subs := s.ring.Subscribe()
	writeOne := func(rec logging.Record) bool {
		filterMu.Lock()
		match := filter.match(rec)
		filterMu.Unlock()
		if !match {
			return true
		}
		payload, _ := json.Marshal(map[string]any{
			"type": "log", "ts": rec.Time.UTC().Format(time.RFC3339Nano),
			"level": levelName(rec.Level), "component": rec.Component, "msg": rec.Message,
		})
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, payload) == nil
	}
	for _, rec := range s.ring.Snapshot() {
		if !writeOne(rec) {
			return
		}
	}

	// 读泵：subscribe/ping。
	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg struct {
				Type string `json:"type"`
				wsFilter
			}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			switch msg.Type {
			case "subscribe":
				filterMu.Lock()
				filter = wsFilter{Levels: msg.Levels, Components: msg.Components, Keyword: msg.Keyword}
				filterMu.Unlock()
			case "ping":
				pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
				_ = conn.Write(pctx, websocket.MessageText, []byte(`{"type":"ping"}`))
				pcancel()
			}
		}
	}()

	// 30s 心跳（04 §5）。
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
			if conn.Write(hctx, websocket.MessageText, []byte(`{"type":"ping"}`)) != nil {
				hcancel()
				return
			}
			hcancel()
		case rec, ok := <-subs:
			if !ok {
				return
			}
			if !writeOne(rec) {
				return
			}
		}
	}
}
