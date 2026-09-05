package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
)

// UserbotControl 是 userbot 向导/状态/退出的 API 侧最小面
// （telegram.WizardService + 主 user 客户端组合满足；原生类型签名避免
// platform↔webapi 互相触碰，01 §2.2）。
type UserbotControl interface {
	Status(ctx context.Context) (authorized bool, username string, tgID int64, err error)
	LoginStart(ctx context.Context, phone string) (requestID string, err error)
	// LoginCode 返回 passwordRequired=true 时继续 LoginPassword（04 §2 可选 2FA 步）。
	LoginCode(ctx context.Context, requestID, code string) (passwordRequired bool, err error)
	LoginPassword(ctx context.Context, requestID, password string) error
	Logout(ctx context.Context) error
	// Join 加入公开频道（03 §3.8 规则保存预检的运行期形态）。
	Join(ctx context.Context, chat string) error
}

// registerUserbotRoutes 注册 userbot 路由（04 §2；仅依赖注入后）。
func (s *Server) registerUserbotRoutes() {
	s.Handle("GET", "/api/userbot/status", s.handleUserbotStatus)
	s.Handle("POST", "/api/userbot/login/start", s.handleUserbotLoginStart)
	s.Handle("POST", "/api/userbot/login/code", s.handleUserbotLoginCode)
	s.Handle("POST", "/api/userbot/login/password", s.handleUserbotLoginPassword)
	s.Handle("POST", "/api/userbot/logout", s.handleUserbotLogout)
	s.Handle("POST", "/api/userbot/join", s.handleUserbotJoin)
}

func (s *Server) requireUserbot(w http.ResponseWriter) UserbotControl {
	if s.deps == nil || s.deps.Userbot == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "userbot 未接线")
		return nil
	}
	return s.deps.Userbot
}

func (s *Server) handleUserbotStatus(w http.ResponseWriter, r *http.Request) {
	ub := s.requireUserbot(w)
	if ub == nil {
		return
	}
	authorized, username, tgID, err := ub.Status(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"authorized": false, "detail": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"authorized": authorized, "username": username, "id": itoa(tgID),
	})
}

func (s *Server) handleUserbotLoginStart(w http.ResponseWriter, r *http.Request) {
	ub := s.requireUserbot(w)
	if ub == nil {
		return
	}
	var body struct {
		Phone string `json:"phone"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Phone == "" {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "需要 phone（国际格式）")
		return
	}
	requestID, err := ub.LoginStart(r.Context(), body.Phone)
	if err != nil {
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "LOGIN_START_FAILED", "message": err.Error()},
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"requestId": requestID})
}

func (s *Server) handleUserbotLoginCode(w http.ResponseWriter, r *http.Request) {
	ub := s.requireUserbot(w)
	if ub == nil {
		return
	}
	var body struct {
		RequestID string `json:"requestId"`
		Code      string `json:"code"`
	}
	if err := decodeSmall(w, r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return
	}
	passwordRequired, err := ub.LoginCode(r.Context(), body.RequestID, body.Code)
	if err != nil {
		s.writeError(w, statusOf(err), "LOGIN_CODE_FAILED", err.Error())
		return
	}
	if passwordRequired {
		s.writeJSON(w, http.StatusOK, map[string]any{"status": "password_required"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "authorized"})
}

func (s *Server) handleUserbotLoginPassword(w http.ResponseWriter, r *http.Request) {
	ub := s.requireUserbot(w)
	if ub == nil {
		return
	}
	var body struct {
		RequestID string `json:"requestId"`
		Password  string `json:"password"`
	}
	if err := decodeSmall(w, r, &body); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return
	}
	if err := ub.LoginPassword(r.Context(), body.RequestID, body.Password); err != nil {
		s.writeError(w, statusOf(err), "LOGIN_PASSWORD_FAILED", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "authorized"})
}

func (s *Server) handleUserbotLogout(w http.ResponseWriter, r *http.Request) {
	ub := s.requireUserbot(w)
	if ub == nil {
		return
	}
	if err := ub.Logout(r.Context()); err != nil {
		s.writeError(w, statusOf(err), "LOGOUT_FAILED", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"loggedOut": true})
}

func (s *Server) handleUserbotJoin(w http.ResponseWriter, r *http.Request) {
	ub := s.requireUserbot(w)
	if ub == nil {
		return
	}
	var body struct {
		Chat string `json:"chat"`
	}
	if err := decodeSmall(w, r, &body); err != nil || body.Chat == "" {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "需要 chat")
		return
	}
	if err := ub.Join(r.Context(), body.Chat); err != nil {
		s.writeError(w, statusOf(err), "JOIN_FAILED", err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"joined": true})
}

// ---------- 工具 ----------

func decodeSmall(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	return json.NewDecoder(r.Body).Decode(v)
}

// statusOf 预留错误→状态码映射（当前统一 422 用户可修正错误）。
func statusOf(err error) int {
	_ = err
	return http.StatusUnprocessableEntity
}

// itoa 是 strconv.FormatInt 的语义别名（DTO 字符串 ID 约定）。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
