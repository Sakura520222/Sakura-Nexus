package domain

import "time"

// Channel 是频道注册表条目（02 §2.2 channels 表的领域形态）。
type Channel struct {
	TgID             int64  `json:"tgId"`               // 裸 ID（唯一稳定标识）
	Username         string `json:"username,omitempty"` // 解析辅助（可变，非身份）
	Title            string `json:"title,omitempty"`
	DiscussionChatID int64  `json:"discussionChatId,omitempty"` // 关联讨论群裸 ID
	IsVerified       bool   `json:"isVerified,omitempty"`
}

// ForwardRule 是转发规则（02 §2.2 forward_rules 表的领域形态；
// 源/目标均为完整 ChatRef——R3.1.1，username 为可变辅助列）。
type ForwardRule struct {
	ID                  int64    `json:"id"`
	Name                string   `json:"name,omitempty"`
	Source              ChatRef  `json:"source"`
	SourceUsername      string   `json:"sourceUsername,omitempty"`
	Target              ChatRef  `json:"target"`
	TargetUsername      string   `json:"targetUsername,omitempty"`
	Enabled             bool     `json:"enabled"`
	Keywords            []string `json:"keywords,omitempty"`
	Blacklist           []string `json:"blacklist,omitempty"`
	Patterns            []string `json:"patterns,omitempty"`
	BlacklistPatterns   []string `json:"blacklistPatterns,omitempty"`
	MediaTypes          []string `json:"mediaTypes,omitempty"`
	ForwardOriginalOnly bool     `json:"forwardOriginalOnly"`
	CopyMode            string   `json:"copyMode"` // copy / forward
	AIEnabled           bool     `json:"aiEnabled"`
	AIPrompt            string   `json:"aiPrompt,omitempty"`
	CustomFooter        string   `json:"customFooter,omitempty"`
	DelayMinSec         float64  `json:"delayMinSec"`
	DelayMaxSec         float64  `json:"delayMaxSec"`
	LastMessageID       int64    `json:"lastMessageId"` // contiguous cursor（P0 Plan §6）
}

// AuditEntry 是 WebUI/命令/system 的审计记录（02 §2.8）。
// ForwardingStat 是 forwarding_stats 的读取形态（按规则与日期聚合，04 §2
// GET /api/forwarding/stats 的行）。
type ForwardingStat struct {
	RuleID    int64  `db:"rule_id" json:"ruleId"`
	Date      string `db:"stat_date" json:"date"` // YYYY-MM-DD（DTO 时间约定 ISO 8601）
	Forwarded int64  `db:"forwarded_count" json:"forwarded"`
	Failed    int64  `db:"failed_count" json:"failed"`
}

type AuditEntry struct {
	Actor  string         `json:"actor"` // webui:<username> / tg:<user_id> / system
	Action string         `json:"action"`
	Detail map[string]any `json:"detail,omitempty"`
}

// AuditLogEntry 是审计查询行（04 §2 GET /api/system/audit-logs；写侧用 AuditEntry）。
type AuditLogEntry struct {
	ID        int64          `db:"id" json:"id"`
	Actor     string         `db:"actor" json:"actor"`
	Action    string         `db:"action" json:"action"`
	Detail    map[string]any `db:"detail" json:"detail,omitempty"`
	CreatedAt time.Time      `db:"created_at" json:"createdAt"`
}
