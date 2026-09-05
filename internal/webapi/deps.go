package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
)

// Deps 是业务 API 的依赖注入面（04 §2 API 清单；webapi 只依赖消费方最小
// 接口与 config/domain，不触碰 platform——01 §2.2）。
type Deps struct {
	// Settings：settings 中心快照读/字段级合并写（system/forwarding/logging/ai）。
	Settings SettingsControl
	// Engine：转发引擎控制（system pause/resume + status 的 paused 字段）。
	Engine EngineControl
	// RequestRestart：WebUI restart → exit 75（01 §1.4；app.Production 提供）。
	RequestRestart func()
	// SetLogLevel：动态日志级别（PUT /system/log-level；根 handler LevelVar）。
	SetLogLevel func(level string) error
	// Audit：审计查询（GET /system/audit-logs；写侧经 WithAuditSink）。
	Audit AuditReader
	// Rules：转发规则管理（04 §2 forwarding CRUD+enable/disable）。
	Rules RuleAdmin
	// Channels：频道管理（04 §2 channels CRUD；channel_settings 属 P1/P2）。
	Channels ChannelAdmin
	// Stats：转发统计读取（GET /forwarding/stats）。
	Stats StatsReader
}

// StatsReader 是统计读取最小面（mysql.ForwardedRepo 结构满足）。
type StatsReader interface {
	Stats(ctx context.Context, ruleID int64, days int) ([]domain.ForwardingStat, error)
}

// SettingsControl 是 settings 中心的 API 侧最小面（app 层适配
// *config.SettingsCenter；快照为 JSON 兼容 map 形态）。
type SettingsControl interface {
	// Snapshot 返回 scope 快照；未知 scope 返回错误。
	Snapshot(scope string) (map[string]any, error)
	// Update 字段级合并：校验失败或写库失败返回错误且不改快照。
	Update(ctx context.Context, scope string, partial map[string]any) error
	// Scopes 返回合法 scope 列表。
	Scopes() []string
}

// EngineControl 是引擎的 API 侧最小面。
type EngineControl interface {
	Paused() bool
	Pause()
	Resume()
	// Backfill 回溯补发（03 §3.7；POST /forwarding/rules/{id}/backfill）。
	Backfill(ctx context.Context, ruleID int64, limit int) (forwarding.BackfillResult, error)
	// RefreshRules 规则 CRUD 后热装载（03 §3.1）。
	RefreshRules(ctx context.Context) error
}

// AuditReader 是审计查询最小面。
type AuditReader interface {
	List(ctx context.Context, limit int) ([]domain.AuditLogEntry, error)
}

// settingsSecretKeys 是 GET 时脱敏的 secret 字段（04 §2：••• + 尾 4）。
var settingsSecretKeys = map[string][]string{
	"ai": {"api_key"},
}

// WithDeps 注入业务依赖并注册 system/settings 路由（转发/channels/向导/WS
// 由后续子提交扩展同一 Deps）。nil = 仅骨架（T5.1/5.2 形态）。
func WithDeps(d Deps) ServerOption {
	return func(s *Server) {
		s.deps = &d
		s.registerSystemRoutes()
	}
}

// ApplyDeps 是 WithDeps 的构造后形态（装配层先构造 Server 再接线依赖；
// 须在 Run 前调用，路由注册同样仅 Run 前有效）。
func (s *Server) ApplyDeps(d Deps) { WithDeps(d)(s) }

// registerSystemRoutes 注册 system 与 settings 路由（须在 Run 前）。
func (s *Server) registerSystemRoutes() {
	s.Handle("GET", "/api/system/status", s.handleSystemStatus)
	s.Handle("POST", "/api/system/pause", s.handleSystemPause)
	s.Handle("POST", "/api/system/resume", s.handleSystemResume)
	s.Handle("POST", "/api/system/restart", s.handleSystemRestart)
	s.Handle("PUT", "/api/system/log-level", s.handleLogLevel)
	s.Handle("GET", "/api/system/audit-logs", s.handleAuditLogs)
	s.Handle("GET", "/api/settings/{scope}", s.handleSettingsGet)
	s.Handle("PUT", "/api/settings/{scope}", s.handleSettingsPut)
	s.registerForwardingChannelsRoutes()
}

// handleSystemStatus 组件细项与运行状态（01 §1.5：鉴权后）。
// 组件可用性状态（bot/user 连接翻转）在 availability 接线后补全，当前报告
// 进程内确定性字段。
func (s *Server) handleSystemStatus(w http.ResponseWriter, _ *http.Request) {
	d := s.deps
	status := map[string]any{
		"version":           versionString(),
		"uptime_seconds":    time.Since(s.start).Seconds(),
		"paused":            d.Engine != nil && d.Engine.Paused(),
		"restart_available": d.RequestRestart != nil,
	}
	if d.Settings != nil {
		status["settings_scopes"] = d.Settings.Scopes()
	}
	s.writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleSystemPause(w http.ResponseWriter, _ *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	s.deps.Engine.Pause()
	s.writeJSON(w, http.StatusOK, map[string]any{"paused": true})
}

func (s *Server) handleSystemResume(w http.ResponseWriter, _ *http.Request) {
	if !s.requireEngine(w) {
		return
	}
	s.deps.Engine.Resume()
	s.writeJSON(w, http.StatusOK, map[string]any{"paused": false})
}

// handleSystemRestart 先应答再触发 exit 75（01 §1.4：非零码由 systemd/
// docker 拉起新进程；异步触发避免响应连接被关闭流程截断）。
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.RequestRestart == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "重启钩子未接线")
		return
	}
	s.log.Warn("WebUI 请求重启（exit 75）", "remote", clientIP(r))
	s.writeJSON(w, http.StatusOK, map[string]any{"restarting": true})
	go func() {
		time.Sleep(200 * time.Millisecond) // 让响应落到客户端
		s.deps.RequestRestart()
	}()
}

var validLogLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

// handleLogLevel 动态调整根 logger 级别（06 §4 运维面）。
func (s *Server) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.SetLogLevel == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "日志级别钩子未接线")
		return
	}
	var body struct {
		Level string `json:"level"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return
	}
	level := strings.ToLower(strings.TrimSpace(body.Level))
	if !validLogLevels[level] {
		s.writeError(w, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "level 须为 debug|info|warn|error")
		return
	}
	if err := s.deps.SetLogLevel(level); err != nil {
		s.writeError(w, http.StatusInternalServerError, "INTERNAL", "调整失败")
		return
	}
	s.log.Info("日志级别已调整", "level", level, "remote", clientIP(r))
	s.writeJSON(w, http.StatusOK, map[string]any{"level": level})
}

// handleAuditLogs 最近审计查询。
func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Audit == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "审计查询未接线")
		return
	}
	entries, err := s.deps.Audit.List(r.Context(), queryInt(r, "limit", 100))
	if err != nil {
		s.log.Error("审计查询失败", "err", err)
		s.writeError(w, http.StatusInternalServerError, "INTERNAL", "查询失败")
		return
	}
	if entries == nil {
		entries = []domain.AuditLogEntry{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": entries})
}

// ---------- settings ----------

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	scope := r.PathValue("scope")
	snap, err := s.deps.Settings.Snapshot(scope)
	if err != nil {
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	// secret 脱敏（04 §2：••• + 尾 4）。
	for _, key := range settingsSecretKeys[scope] {
		if v, ok := snap[key].(string); ok && v != "" {
			snap[key] = maskSecret(v)
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "settings": snap})
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireSettings(w) {
		return
	}
	scope := r.PathValue("scope")
	if !settingsHasScope(s.deps.Settings, scope) {
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", "未知 scope: "+scope)
		return
	}
	var partial map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&partial); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return
	}
	if err := s.deps.Settings.Update(r.Context(), scope, partial); err != nil {
		// 校验失败 422（04 §3 错误结构）。
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "VALIDATION_ERROR", "message": err.Error()},
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "updated": true})
}

// ---------- 工具 ----------

// requireEngine 防御 Engine 未接线的调用（配置错误，非用户错误）。
func (s *Server) requireEngine(w http.ResponseWriter) bool {
	if s.deps == nil || s.deps.Engine == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "业务依赖未接线")
		return false
	}
	return true
}

func (s *Server) requireSettings(w http.ResponseWriter) bool {
	if s.deps == nil || s.deps.Settings == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "settings 未接线")
		return false
	}
	return true
}

// settingsHasScope 判定 scope 合法性。
func settingsHasScope(c SettingsControl, scope string) bool {
	for _, s := range c.Scopes() {
		if s == scope {
			return true
		}
	}
	return false
}

// maskSecret 脱敏：••• + 尾 4（不足 5 位全遮）。
func maskSecret(s string) string {
	if len(s) <= 4 {
		return "•••"
	}
	return "•••" + s[len(s)-4:]
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n := 0
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
	}
	if n == 0 {
		return def
	}
	return n
}
