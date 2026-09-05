package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// 转发规则与频道业务 API（04 §2）。DTO 约定（04 §3）：Telegram ID 以字符串
// 传输——domain JSON 保持数值形态（internal/domain 冻结决策），转换在此层。

// RuleAdmin 是规则管理最小面（mysql.ForwardRuleRepo 结构满足）。
type RuleAdmin interface {
	List(ctx context.Context) ([]domain.ForwardRule, error)
	Get(ctx context.Context, id int64) (domain.ForwardRule, bool, error)
	Create(ctx context.Context, rule domain.ForwardRule) (int64, error)
	Update(ctx context.Context, rule domain.ForwardRule) error
	Delete(ctx context.Context, id int64) error
	SetEnabled(ctx context.Context, id int64, enabled bool) error
}

// ChannelAdmin 是频道管理最小面（mysql.ChannelRepo 结构满足）。
// channel_settings（summary/poll/welcome）属 P1/P2 能力，P0 不暴露
// （范围锁定：P0 不建 Summary 等业务）。
type ChannelAdmin interface {
	List(ctx context.Context) ([]domain.Channel, error)
	Upsert(ctx context.Context, ch domain.Channel) error
	Get(ctx context.Context, tgID int64) (domain.Channel, bool, error)
	Delete(ctx context.Context, tgID int64) error
}

// ---------- DTO ----------

type chatRefDTO struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (c chatRefDTO) toDomain() (domain.ChatRef, error) {
	id, err := strconv.ParseInt(c.ID, 10, 64)
	if err != nil {
		return domain.ChatRef{}, errors.New("id 须为十进制数字字符串: " + c.ID)
	}
	kind, ok := domain.PeerKindFromString(c.Kind)
	if !ok {
		return domain.ChatRef{}, errors.New("kind 须为 user|chat|channel: " + c.Kind)
	}
	return domain.NewChatRef(kind, id), nil
}

func chatRefFrom(ref domain.ChatRef) chatRefDTO {
	return chatRefDTO{Kind: ref.Kind.String(), ID: strconv.FormatInt(ref.ID, 10)}
}

// ruleDTO 是 ForwardRule 的 API 形态（ID/源/目标为字符串；lastMessageId 只读
// ——contiguous cursor 由引擎维护，PUT 忽略入参值）。
type ruleDTO struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name,omitempty"`
	Source              chatRefDTO `json:"source"`
	SourceUsername      string     `json:"sourceUsername,omitempty"`
	Target              chatRefDTO `json:"target"`
	TargetUsername      string     `json:"targetUsername,omitempty"`
	Enabled             bool       `json:"enabled"`
	Keywords            []string   `json:"keywords,omitempty"`
	Blacklist           []string   `json:"blacklist,omitempty"`
	Patterns            []string   `json:"patterns,omitempty"`
	BlacklistPatterns   []string   `json:"blacklistPatterns,omitempty"`
	MediaTypes          []string   `json:"mediaTypes,omitempty"`
	ForwardOriginalOnly bool       `json:"forwardOriginalOnly"`
	CopyMode            string     `json:"copyMode"`
	AIEnabled           bool       `json:"aiEnabled"`
	AIPrompt            string     `json:"aiPrompt,omitempty"`
	CustomFooter        string     `json:"customFooter,omitempty"`
	DelayMinSec         float64    `json:"delayMinSec"`
	DelayMaxSec         float64    `json:"delayMaxSec"`
	LastMessageID       string     `json:"lastMessageId"`
}

func ruleToDTO(r domain.ForwardRule) ruleDTO {
	return ruleDTO{
		ID: strconv.FormatInt(r.ID, 10), Name: r.Name,
		Source: chatRefFrom(r.Source), SourceUsername: r.SourceUsername,
		Target: chatRefFrom(r.Target), TargetUsername: r.TargetUsername,
		Enabled:  r.Enabled,
		Keywords: r.Keywords, Blacklist: r.Blacklist, Patterns: r.Patterns,
		BlacklistPatterns: r.BlacklistPatterns, MediaTypes: r.MediaTypes,
		ForwardOriginalOnly: r.ForwardOriginalOnly,
		CopyMode:            r.CopyMode,
		AIEnabled:           r.AIEnabled, AIPrompt: r.AIPrompt,
		CustomFooter: r.CustomFooter,
		DelayMinSec:  r.DelayMinSec, DelayMaxSec: r.DelayMaxSec,
		LastMessageID: strconv.FormatInt(r.LastMessageID, 10),
	}
}

func (d ruleDTO) toDomain() (domain.ForwardRule, error) {
	src, err := d.Source.toDomain()
	if err != nil {
		return domain.ForwardRule{}, errors.New("source: " + err.Error())
	}
	tgt, err := d.Target.toDomain()
	if err != nil {
		return domain.ForwardRule{}, errors.New("target: " + err.Error())
	}
	id := int64(0)
	if d.ID != "" {
		if id, err = strconv.ParseInt(d.ID, 10, 64); err != nil {
			return domain.ForwardRule{}, errors.New("id 须为数字字符串")
		}
	}
	copyMode := d.CopyMode
	if copyMode == "" {
		copyMode = "copy"
	}
	if copyMode != "copy" && copyMode != "forward" {
		return domain.ForwardRule{}, errors.New("copyMode 须为 copy|forward")
	}
	return domain.ForwardRule{
		ID: id, Name: d.Name,
		Source: src, SourceUsername: d.SourceUsername,
		Target: tgt, TargetUsername: d.TargetUsername,
		Enabled:  d.Enabled,
		Keywords: d.Keywords, Blacklist: d.Blacklist,
		Patterns: d.Patterns, BlacklistPatterns: d.BlacklistPatterns,
		MediaTypes:          d.MediaTypes,
		ForwardOriginalOnly: d.ForwardOriginalOnly,
		CopyMode:            copyMode,
		AIEnabled:           d.AIEnabled, AIPrompt: d.AIPrompt,
		CustomFooter: d.CustomFooter,
		DelayMinSec:  d.DelayMinSec, DelayMaxSec: d.DelayMaxSec,
	}, nil
}

// ---------- 注册 ----------

// registerForwardingChannelsRoutes 注册转发规则/统计/频道路由（04 §2）。
func (s *Server) registerForwardingChannelsRoutes() {
	s.Handle("GET", "/api/forwarding/rules", s.handleRulesList)
	s.Handle("POST", "/api/forwarding/rules", s.handleRuleCreate)
	s.Handle("GET", "/api/forwarding/rules/{id}", s.handleRuleGet)
	s.Handle("PUT", "/api/forwarding/rules/{id}", s.handleRuleUpdate)
	s.Handle("DELETE", "/api/forwarding/rules/{id}", s.handleRuleDelete)
	s.Handle("POST", "/api/forwarding/rules/{id}/enable", s.handleRuleEnable(true))
	s.Handle("POST", "/api/forwarding/rules/{id}/disable", s.handleRuleEnable(false))
	s.Handle("POST", "/api/forwarding/rules/{id}/backfill", s.handleRuleBackfill)
	s.Handle("GET", "/api/forwarding/stats", s.handleStats)
	s.Handle("GET", "/api/channels", s.handleChannelsList)
	s.Handle("POST", "/api/channels", s.handleChannelUpsert)
	s.Handle("GET", "/api/channels/{id}", s.handleChannelGet)
	s.Handle("PUT", "/api/channels/{id}", s.handleChannelUpsert)
	s.Handle("DELETE", "/api/channels/{id}", s.handleChannelDelete)
}

// ---------- 处理器 ----------

func (s *Server) handleRulesList(w http.ResponseWriter, r *http.Request) {
	rules, err := s.deps.Rules.List(r.Context())
	if err != nil {
		s.repoError(w, err)
		return
	}
	items := make([]ruleDTO, 0, len(rules))
	for _, r := range rules {
		items = append(items, ruleToDTO(r))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleRuleCreate(w http.ResponseWriter, r *http.Request) {
	rule, ok := s.parseRuleBody(w, r)
	if !ok {
		return
	}
	id, err := s.deps.Rules.Create(r.Context(), rule)
	if err != nil {
		s.repoError(w, err)
		return
	}
	s.refreshRules()
	s.writeJSON(w, http.StatusOK, map[string]any{"id": strconv.FormatInt(id, 10)})
}

func (s *Server) handleRuleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathRuleID(w, r)
	if !ok {
		return
	}
	rule, found, err := s.deps.Rules.Get(r.Context(), id)
	if err != nil {
		s.repoError(w, err)
		return
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", "规则不存在")
		return
	}
	s.writeJSON(w, http.StatusOK, ruleToDTO(rule))
}

func (s *Server) handleRuleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathRuleID(w, r)
	if !ok {
		return
	}
	rule, ok := s.parseRuleBody(w, r)
	if !ok {
		return
	}
	// cursor 引擎维护：以存储值覆盖（PUT 忽略入参 lastMessageId）。
	stored, found, err := s.deps.Rules.Get(r.Context(), id)
	if err != nil {
		s.repoError(w, err)
		return
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", "规则不存在")
		return
	}
	rule.ID, rule.LastMessageID = id, stored.LastMessageID
	if err := s.deps.Rules.Update(r.Context(), rule); err != nil {
		s.repoError(w, err)
		return
	}
	s.refreshRules()
	s.writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathRuleID(w, r)
	if !ok {
		return
	}
	if err := s.deps.Rules.Delete(r.Context(), id); err != nil {
		s.repoError(w, err)
		return
	}
	s.refreshRules()
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleRuleEnable(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.pathRuleID(w, r)
		if !ok {
			return
		}
		if err := s.deps.Rules.SetEnabled(r.Context(), id, enabled); err != nil {
			s.repoError(w, err)
			return
		}
		s.refreshRules()
		s.writeJSON(w, http.StatusOK, map[string]any{"id": strconv.FormatInt(id, 10), "enabled": enabled})
	}
}

// handleRuleBackfill 回溯补发（03 §3.7；单次上限 200 防风暴）。
func (s *Server) handleRuleBackfill(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Engine == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "引擎未接线")
		return
	}
	id, ok := s.pathRuleID(w, r)
	if !ok {
		return
	}
	var body struct {
		Limit int `json:"limit"`
	}
	// 空 body = 默认上限（03 §3.7 单次 ≤200）。
	raw := make([]byte, 1<<10)
	n, _ := r.Body.Read(raw)
	if n > 0 {
		if err := json.Unmarshal(raw[:n], &body); err != nil {
			s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
			return
		}
	}
	res, err := s.deps.Engine.Backfill(r.Context(), id, body.Limit)
	if err != nil {
		// 规则不存在/History 未接入 → 用户可见错误
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "BACKFILL_FAILED", "message": err.Error()},
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"fetched": res.Fetched,
		"cursor":  strconv.FormatInt(res.Cursor, 10),
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if s.deps.Stats == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NOT_READY", "统计未接线")
		return
	}
	ruleID := int64(0)
	if v := r.URL.Query().Get("rule_id"); v != "" {
		var err error
		if ruleID, err = strconv.ParseInt(v, 10, 64); err != nil {
			s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "rule_id 须为数字")
			return
		}
	}
	rows, err := s.deps.Stats.Stats(r.Context(), ruleID, queryInt(r, "days", 30))
	if err != nil {
		s.repoError(w, err)
		return
	}
	if rows == nil {
		rows = []domain.ForwardingStat{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": rows})
}

// ---------- channels ----------

type channelDTO struct {
	TgID             string `json:"tgId"`
	Username         string `json:"username,omitempty"`
	Title            string `json:"title,omitempty"`
	DiscussionChatID string `json:"discussionChatId,omitempty"`
	IsVerified       bool   `json:"isVerified,omitempty"`
}

func channelToDTO(c domain.Channel) channelDTO {
	return channelDTO{
		TgID: strconv.FormatInt(c.TgID, 10), Username: c.Username, Title: c.Title,
		DiscussionChatID: strconv.FormatInt(c.DiscussionChatID, 10), IsVerified: c.IsVerified,
	}
}

func (d channelDTO) toDomain() (domain.Channel, error) {
	id, err := strconv.ParseInt(d.TgID, 10, 64)
	if err != nil {
		return domain.Channel{}, errors.New("tgId 须为数字字符串")
	}
	disc := int64(0)
	if d.DiscussionChatID != "" {
		if disc, err = strconv.ParseInt(d.DiscussionChatID, 10, 64); err != nil {
			return domain.Channel{}, errors.New("discussionChatId 须为数字字符串")
		}
	}
	return domain.Channel{TgID: id, Username: d.Username, Title: d.Title, DiscussionChatID: disc, IsVerified: d.IsVerified}, nil
}

func (s *Server) handleChannelsList(w http.ResponseWriter, r *http.Request) {
	chs, err := s.deps.Channels.List(r.Context())
	if err != nil {
		s.repoError(w, err)
		return
	}
	items := make([]channelDTO, 0, len(chs))
	for _, c := range chs {
		items = append(items, channelToDTO(c))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleChannelUpsert(w http.ResponseWriter, r *http.Request) {
	ch, ok := s.parseChannelBody(w, r)
	if !ok {
		return
	}
	if p := r.PathValue("id"); p != "" {
		pid, err := strconv.ParseInt(p, 10, 64)
		if err != nil || pid != ch.TgID {
			s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "路径 id 与 tgId 不一致")
			return
		}
	}
	if err := s.deps.Channels.Upsert(r.Context(), ch); err != nil {
		s.repoError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"upserted": true})
}

func (s *Server) handleChannelGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathChannelID(w, r)
	if !ok {
		return
	}
	ch, found, err := s.deps.Channels.Get(r.Context(), id)
	if err != nil {
		s.repoError(w, err)
		return
	}
	if !found {
		s.writeError(w, http.StatusNotFound, "NOT_FOUND", "频道不存在")
		return
	}
	s.writeJSON(w, http.StatusOK, channelToDTO(ch))
}

func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathChannelID(w, r)
	if !ok {
		return
	}
	if err := s.deps.Channels.Delete(r.Context(), id); err != nil {
		s.repoError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ---------- 工具 ----------

func (s *Server) parseRuleBody(w http.ResponseWriter, r *http.Request) (domain.ForwardRule, bool) {
	var dto ruleDTO
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return domain.ForwardRule{}, false
	}
	rule, err := dto.toDomain()
	if err != nil {
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "VALIDATION_ERROR", "message": err.Error()},
		})
		return domain.ForwardRule{}, false
	}
	return rule, true
}

func (s *Server) parseChannelBody(w http.ResponseWriter, r *http.Request) (domain.Channel, bool) {
	var dto channelDTO
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return domain.Channel{}, false
	}
	ch, err := dto.toDomain()
	if err != nil {
		s.writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "VALIDATION_ERROR", "message": err.Error()},
		})
		return domain.Channel{}, false
	}
	return ch, true
}

func (s *Server) pathRuleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) pathChannelID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "id 非法")
		return 0, false
	}
	return id, true
}

func (s *Server) repoError(w http.ResponseWriter, err error) {
	s.log.Error("仓库操作失败", "err", err)
	s.writeError(w, http.StatusInternalServerError, "INTERNAL", "操作失败")
}

// refreshRules 规则变更后热装载（03 §3.1；失败仅告警——下次引擎 Run 会再装）。
func (s *Server) refreshRules() {
	if s.deps != nil && s.deps.Engine != nil {
		if err := s.deps.Engine.RefreshRules(context.Background()); err != nil {
			s.log.Warn("规则热装载失败", "err", err)
		}
	}
}
