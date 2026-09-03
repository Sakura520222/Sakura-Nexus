package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// ---------- ChannelRepo ----------

// ChannelRepo 是 channels / channel_settings 表的仓库。
type ChannelRepo struct{ db *Database }

func NewChannelRepo(db *sqlx.DB) *ChannelRepo {
	return &ChannelRepo{db: WrapDatabase(db, nil)}
}

// Upsert 以 tg_id 为身份写入（username/title/discussion 快照更新）。
func (r *ChannelRepo) Upsert(ctx context.Context, ch domain.Channel) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO channels (tg_id, username, title, discussion_chat_id, is_verified)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE username = VALUES(username), title = VALUES(title),
			discussion_chat_id = VALUES(discussion_chat_id), is_verified = VALUES(is_verified)`,
		ch.TgID, nullStr(ch.Username), ch.Title, nullInt64(ch.DiscussionChatID), ch.IsVerified)
	if err != nil {
		return fmt.Errorf("upsert channel: %w", err)
	}
	return nil
}

func (r *ChannelRepo) List(ctx context.Context) ([]domain.Channel, error) {
	var rows []struct {
		TgID             int64   `db:"tg_id"`
		Username         *string `db:"username"`
		Title            *string `db:"title"`
		DiscussionChatID *int64  `db:"discussion_chat_id"`
		IsVerified       bool    `db:"is_verified"`
	}
	if err := r.db.SelectContext(ctx, &rows, `
		SELECT tg_id, username, title, discussion_chat_id, is_verified FROM channels ORDER BY tg_id`); err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	out := make([]domain.Channel, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.Channel{
			TgID: row.TgID, Username: derefStr(row.Username), Title: derefStr(row.Title),
			DiscussionChatID: derefInt64(row.DiscussionChatID), IsVerified: row.IsVerified,
		})
	}
	return out, nil
}

func (r *ChannelRepo) GetByTgID(ctx context.Context, tgID int64) (domain.Channel, bool, error) {
	var row struct {
		TgID             int64   `db:"tg_id"`
		Username         *string `db:"username"`
		Title            *string `db:"title"`
		DiscussionChatID *int64  `db:"discussion_chat_id"`
		IsVerified       bool    `db:"is_verified"`
	}
	err := r.db.GetContext(ctx, &row,
		"SELECT tg_id, username, title, discussion_chat_id, is_verified FROM channels WHERE tg_id = ?", tgID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Channel{}, false, nil
	}
	if err != nil {
		return domain.Channel{}, false, fmt.Errorf("get channel: %w", err)
	}
	return domain.Channel{
		TgID: row.TgID, Username: derefStr(row.Username), Title: derefStr(row.Title),
		DiscussionChatID: derefInt64(row.DiscussionChatID), IsVerified: row.IsVerified,
	}, true, nil
}

func (r *ChannelRepo) Delete(ctx context.Context, tgID int64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM channel_settings WHERE channel_id = ?", tgID); err != nil {
		return fmt.Errorf("delete channel_settings: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, "DELETE FROM channels WHERE tg_id = ?", tgID); err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return nil
}

// ---------- ForwardRuleRepo ----------

// ForwardRuleRepo 是 forward_rules 表的仓库（规则 CRUD 与 contiguous cursor）。
type ForwardRuleRepo struct{ db *Database }

func NewForwardRuleRepo(db *sqlx.DB) *ForwardRuleRepo {
	return &ForwardRuleRepo{db: WrapDatabase(db, nil)}
}

type ruleRow struct {
	ID                  int64     `db:"id"`
	Name                *string   `db:"name"`
	SourceChatType      string    `db:"source_chat_type"`
	SourceChatID        *int64    `db:"source_chat_id"`
	SourceUsername      *string   `db:"source_username"`
	TargetChatType      string    `db:"target_chat_type"`
	TargetChatID        *int64    `db:"target_chat_id"`
	TargetUsername      *string   `db:"target_username"`
	Enabled             bool      `db:"enabled"`
	Keywords            []byte    `db:"keywords"`
	Blacklist           []byte    `db:"blacklist"`
	Patterns            []byte    `db:"patterns"`
	BlacklistPatterns   []byte    `db:"blacklist_patterns"`
	MediaTypes          []byte    `db:"media_types"`
	ForwardOriginalOnly bool      `db:"forward_original_only"`
	CopyMode            string    `db:"copy_mode"`
	AIEnabled           bool      `db:"ai_enabled"`
	AIPrompt            *string   `db:"ai_prompt"`
	CustomFooter        *string   `db:"custom_footer"`
	DelayMinSec         float64   `db:"delay_min_sec"`
	DelayMaxSec         float64   `db:"delay_max_sec"`
	LastMessageID       *int64    `db:"last_message_id"`
	CreatedAt           time.Time `db:"created_at"`
	UpdatedAt           time.Time `db:"updated_at"`
}

func (r ruleRow) toDomain() domain.ForwardRule {
	rule := domain.ForwardRule{
		ID:                  r.ID,
		Name:                derefStr(r.Name),
		Source:              domain.NewChatRef(kindOf(r.SourceChatType), derefInt64(r.SourceChatID)),
		SourceUsername:      derefStr(r.SourceUsername),
		Target:              domain.NewChatRef(kindOf(r.TargetChatType), derefInt64(r.TargetChatID)),
		TargetUsername:      derefStr(r.TargetUsername),
		Enabled:             r.Enabled,
		ForwardOriginalOnly: r.ForwardOriginalOnly,
		CopyMode:            r.CopyMode,
		AIEnabled:           r.AIEnabled,
		AIPrompt:            derefStr(r.AIPrompt),
		CustomFooter:        derefStr(r.CustomFooter),
		DelayMinSec:         r.DelayMinSec,
		DelayMaxSec:         r.DelayMaxSec,
		LastMessageID:       derefInt64(r.LastMessageID),
	}
	_ = json.Unmarshal(r.Keywords, &rule.Keywords)
	_ = json.Unmarshal(r.Blacklist, &rule.Blacklist)
	_ = json.Unmarshal(r.Patterns, &rule.Patterns)
	_ = json.Unmarshal(r.BlacklistPatterns, &rule.BlacklistPatterns)
	_ = json.Unmarshal(r.MediaTypes, &rule.MediaTypes)
	return rule
}

// Create 写入规则并返回 ID。
func (r *ForwardRuleRepo) Create(ctx context.Context, rule domain.ForwardRule) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO forward_rules
			(name, source_chat_type, source_chat_id, source_username,
			 target_chat_type, target_chat_id, target_username,
			 enabled, keywords, blacklist, patterns, blacklist_patterns, media_types,
			 forward_original_only, copy_mode, ai_enabled, ai_prompt, custom_footer,
			 delay_min_sec, delay_max_sec)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullStr(rule.Name), rule.Source.Kind.String(), nullInt64(rule.Source.ID), nullStr(rule.SourceUsername),
		rule.Target.Kind.String(), nullInt64(rule.Target.ID), nullStr(rule.TargetUsername),
		rule.Enabled, jsonList(rule.Keywords), jsonList(rule.Blacklist), jsonList(rule.Patterns),
		jsonList(rule.BlacklistPatterns), jsonList(rule.MediaTypes),
		rule.ForwardOriginalOnly, copyModeOrDefault(rule.CopyMode), rule.AIEnabled,
		nullStr(rule.AIPrompt), nullStr(rule.CustomFooter),
		rule.DelayMinSec, rule.DelayMaxSec)
	if err != nil {
		return 0, fmt.Errorf("create rule: %w", err)
	}
	return res.LastInsertId()
}

// Update 全量更新（除 cursor——cursor 只经 AdvanceCursor 前进）。
func (r *ForwardRuleRepo) Update(ctx context.Context, rule domain.ForwardRule) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE forward_rules SET
			name = ?, source_chat_type = ?, source_chat_id = ?, source_username = ?,
			target_chat_type = ?, target_chat_id = ?, target_username = ?,
			enabled = ?, keywords = ?, blacklist = ?, patterns = ?, blacklist_patterns = ?,
			media_types = ?, forward_original_only = ?, copy_mode = ?,
			ai_enabled = ?, ai_prompt = ?, custom_footer = ?,
			delay_min_sec = ?, delay_max_sec = ?
		WHERE id = ?`,
		nullStr(rule.Name), rule.Source.Kind.String(), nullInt64(rule.Source.ID), nullStr(rule.SourceUsername),
		rule.Target.Kind.String(), nullInt64(rule.Target.ID), nullStr(rule.TargetUsername),
		rule.Enabled, jsonList(rule.Keywords), jsonList(rule.Blacklist), jsonList(rule.Patterns),
		jsonList(rule.BlacklistPatterns), jsonList(rule.MediaTypes),
		rule.ForwardOriginalOnly, copyModeOrDefault(rule.CopyMode),
		rule.AIEnabled, nullStr(rule.AIPrompt), nullStr(rule.CustomFooter),
		rule.DelayMinSec, rule.DelayMaxSec, rule.ID)
	if err != nil {
		return fmt.Errorf("update rule: %w", err)
	}
	return nil
}

func (r *ForwardRuleRepo) Delete(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM forward_rules WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

func (r *ForwardRuleRepo) SetEnabled(ctx context.Context, id int64, enabled bool) error {
	if _, err := r.db.ExecContext(ctx, "UPDATE forward_rules SET enabled = ? WHERE id = ?", enabled, id); err != nil {
		return fmt.Errorf("set rule enabled: %w", err)
	}
	return nil
}

func (r *ForwardRuleRepo) Get(ctx context.Context, id int64) (domain.ForwardRule, bool, error) {
	var row ruleRow
	err := r.db.GetContext(ctx, &row, "SELECT * FROM forward_rules WHERE id = ?", id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ForwardRule{}, false, nil
	}
	if err != nil {
		return domain.ForwardRule{}, false, fmt.Errorf("get rule: %w", err)
	}
	return row.toDomain(), true, nil
}

func (r *ForwardRuleRepo) List(ctx context.Context) ([]domain.ForwardRule, error) {
	return r.listWhere(ctx, "1=1")
}

// ListEnabled 返回全部启用规则（引擎热路径——app 层可自行缓存并经 settings
// 订阅失效；本方法始终直查，保证重启/规则变更后一致）。
func (r *ForwardRuleRepo) ListEnabled(ctx context.Context) ([]domain.ForwardRule, error) {
	return r.listWhere(ctx, "enabled = 1")
}

func (r *ForwardRuleRepo) listWhere(ctx context.Context, where string) ([]domain.ForwardRule, error) {
	var rows []ruleRow
	if err := r.db.SelectContext(ctx, &rows, "SELECT * FROM forward_rules WHERE "+where+" ORDER BY id"); err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	out := make([]domain.ForwardRule, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.toDomain())
	}
	return out, nil
}

// AdvanceCursor 前进式更新 contiguous cursor（P0 Plan §6：cursor 只进不退）。
func (r *ForwardRuleRepo) AdvanceCursor(ctx context.Context, id int64, cursor int64) error {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE forward_rules SET last_message_id = GREATEST(COALESCE(last_message_id, 0), ?)
		WHERE id = ?`, cursor, id); err != nil {
		return fmt.Errorf("advance cursor: %w", err)
	}
	return nil
}

// ---------- ForwardedRepo / Stats ----------

// ForwardedRepo 是 forwarded_messages / forwarding_stats 的仓库。
type ForwardedRepo struct{ db *Database }

func NewForwardedRepo(db *sqlx.DB) *ForwardedRepo {
	return &ForwardedRepo{db: WrapDatabase(db, nil)}
}

// Exists 查五列完整 ChatRef 去重键。
func (r *ForwardedRepo) Exists(ctx context.Context, src domain.MessageRef, target domain.ChatRef) (bool, error) {
	var n int
	err := r.db.GetContext(ctx, &n, `
		SELECT COUNT(*) FROM forwarded_messages
		WHERE source_chat_type = ? AND source_chat_id = ? AND source_message_id = ?
			AND target_chat_type = ? AND target_chat_id = ?`,
		src.Chat.Kind.String(), src.Chat.ID, src.MessageID, target.Kind.String(), target.ID)
	if err != nil {
		return false, fmt.Errorf("exists forwarded: %w", err)
	}
	return n > 0, nil
}

// ExistsByContent 按 content_hash 查同源同目标的重复内容（content_dedup 开启时
// 的防删帖重发比对，03 §3.5；走 idx_fwd_hash）。
func (r *ForwardedRepo) ExistsByContent(ctx context.Context, source, target domain.ChatRef,
	contentHash string,
) (bool, error) {
	var n int
	err := r.db.GetContext(ctx, &n, `
		SELECT COUNT(*) FROM forwarded_messages
		WHERE content_hash = ? AND source_chat_type = ? AND source_chat_id = ?
			AND target_chat_type = ? AND target_chat_id = ?`,
		contentHash, source.Kind.String(), source.ID, target.Kind.String(), target.ID)
	if err != nil {
		return false, fmt.Errorf("exists by content: %w", err)
	}
	return n > 0, nil
}

// Record 写入去重记录（INSERT IGNORE：相册全成员补记/重放天然幂等）。
func (r *ForwardedRepo) Record(ctx context.Context, src domain.MessageRef, target domain.ChatRef,
	ruleID int64, targetMessageID int64, contentHash string,
) error {
	if _, err := r.db.ExecContext(ctx, `
		INSERT IGNORE INTO forwarded_messages
			(source_chat_type, source_chat_id, source_message_id, target_chat_type, target_chat_id,
			 rule_id, target_message_id, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		src.Chat.Kind.String(), src.Chat.ID, src.MessageID, target.Kind.String(), target.ID,
		nullInt64(ruleID), nullInt64(targetMessageID), nullStr(contentHash)); err != nil {
		return fmt.Errorf("record forwarded: %w", err)
	}
	return nil
}

// CleanupBefore 删除早于保留期的去重记录（维护任务）。
func (r *ForwardedRepo) CleanupBefore(ctx context.Context, retention time.Duration) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM forwarded_messages WHERE created_at < NOW(6) - INTERVAL ? SECOND`,
		int(retention.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("cleanup forwarded: %w", err)
	}
	return res.RowsAffected()
}

// IncrStats 按真实成败计数：首次插入即计 1（不做「先插 0 再 +1」的哑计数）。
func (r *ForwardedRepo) IncrStats(ctx context.Context, ruleID int64, forwarded bool) error {
	f, n := 0, 1
	if forwarded {
		f, n = 1, 0
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO forwarding_stats (rule_id, stat_date, forwarded_count, failed_count)
		VALUES (?, CURRENT_DATE(), ?, ?)
		ON DUPLICATE KEY UPDATE forwarded_count = forwarded_count + ?, failed_count = failed_count + ?`,
		ruleID, f, n, f, n); err != nil {
		return fmt.Errorf("incr stats: %w", err)
	}
	return nil
}

// ---------- AuditRepo ----------

// AuditRepo 是 system_audit_logs 的仓库。
type AuditRepo struct{ db *Database }

func NewAuditRepo(db *sqlx.DB) *AuditRepo {
	return &AuditRepo{db: WrapDatabase(db, nil)}
}

// Append 写审计（detail 可为 nil）。
func (r *AuditRepo) Append(ctx context.Context, e domain.AuditEntry) error {
	var detail any
	if len(e.Detail) > 0 {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("序列化 audit detail: %w", err)
		}
		detail = b
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO system_audit_logs (actor, action, detail) VALUES (?, ?, ?)`,
		e.Actor, e.Action, detail); err != nil {
		return fmt.Errorf("append audit: %w", err)
	}
	return nil
}

// ---------- helpers ----------

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func jsonList(list []string) any {
	if len(list) == 0 {
		return nil
	}
	b, err := json.Marshal(list)
	if err != nil {
		return nil
	}
	return b
}

func copyModeOrDefault(s string) string {
	if s == "" {
		return "copy"
	}
	return s
}

func kindOf(s string) domain.PeerKind {
	k, _ := domain.PeerKindFromString(s)
	return k
}
