// Package config：.env 引导配置见 env.go；本文件是 MySQL settings 配置中心
// （01 §6.2/§6.3）：每 scope 一个 typed struct（编译期 schema，写入前校验），
// 加载为内存快照，Update 走「校验→写库→快照原子替换→订阅回调」。
// 全项目所有配置写入只经此处（Invariant 4 的配置实例）。
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jmoiron/sqlx"
)

// ---------- P0 scope 定义（01 §6.2；P1/P2 scope 后续追加于此） ----------

// SystemSettings 是 system scope（01 §6.2）。
type SystemSettings struct {
	Language            string  `json:"language"` // zh-CN / en-US
	NotifyAdminsOnStart bool    `json:"notify_admins_on_start"`
	TelegramAdminIDs    []int64 `json:"telegram_admin_ids"` // Bot 管理命令白名单；空 = 命令全禁
}

func (SystemSettings) Validate() error { return nil }

func defaultSystem() SystemSettings {
	return SystemSettings{Language: "zh-CN", NotifyAdminsOnStart: true}
}

// ForwardingSettings 是 forwarding scope（03 §1.6/§3 的运行时参数）。
type ForwardingSettings struct {
	ShowDefaultFooter   bool    `json:"show_default_footer"`
	DedupDays           int     `json:"dedup_days"`
	ContentDedup        bool    `json:"content_dedup"`
	DefaultDelayMinSec  float64 `json:"default_delay_min_sec"`
	DefaultDelayMaxSec  float64 `json:"default_delay_max_sec"`
	AlbumQuietMs        int     `json:"album_quiet_ms"`         // 相册静默窗口（默认 450）
	AlbumHardDeadlineMs int     `json:"album_hard_deadline_ms"` // 相册硬上限（默认 2000）
}

func (f ForwardingSettings) Validate() error {
	if f.DedupDays < 0 {
		return fmt.Errorf("dedup_days 不能为负: %d", f.DedupDays)
	}
	if f.DefaultDelayMinSec < 0 || f.DefaultDelayMaxSec < f.DefaultDelayMinSec {
		return fmt.Errorf("延迟区间非法: [%v, %v]", f.DefaultDelayMinSec, f.DefaultDelayMaxSec)
	}
	if f.AlbumQuietMs <= 0 || f.AlbumHardDeadlineMs < f.AlbumQuietMs {
		return fmt.Errorf("相册窗口非法: quiet=%d hard=%d", f.AlbumQuietMs, f.AlbumHardDeadlineMs)
	}
	return nil
}

func defaultForwarding() ForwardingSettings {
	return ForwardingSettings{
		ShowDefaultFooter:   true,
		DedupDays:           30,
		DefaultDelayMinSec:  0.5,
		DefaultDelayMaxSec:  2.0,
		AlbumQuietMs:        450,
		AlbumHardDeadlineMs: 2000,
	}
}

// LoggingSettings 是 logging scope（运行时级别，覆盖 .env）。
type LoggingSettings struct {
	Level string `json:"level"` // debug/info/warn/error
}

func (l LoggingSettings) Validate() error {
	switch l.Level {
	case "debug", "info", "warn", "error":
		return nil
	}
	return fmt.Errorf("未知日志级别: %q", l.Level)
}

func defaultLogging() LoggingSettings { return LoggingSettings{Level: "info"} }

// AISettings 是 ai scope（P0 起存在——P0 转发 AI 改写即依赖；字段按期启用，
// APIKey 为 secret：仅可写入，WebUI 回显 •••+尾4，01 §6.4）。
type AISettings struct {
	BaseURL        string  `json:"base_url"`
	APIKey         string  `json:"api_key"`
	RewriteModel   string  `json:"rewrite_model"`
	Temperature    float64 `json:"temperature"`
	TimeoutSeconds int     `json:"timeout_seconds"`
	// P1: SummaryModel/EmbeddingModel/EmbeddingDimension；P2: Classification/Vision
}

func (a AISettings) Validate() error {
	if a.Temperature < 0 || a.Temperature > 2 {
		return fmt.Errorf("temperature 超范围: %v", a.Temperature)
	}
	if a.TimeoutSeconds < 0 {
		return fmt.Errorf("timeout_seconds 不能为负: %d", a.TimeoutSeconds)
	}
	return nil
}

func defaultAI() AISettings {
	return AISettings{Temperature: 0.7, TimeoutSeconds: 60}
}

// scopeRegistry 是 scope → 默认构造器（快照装载与校验入口）。
var scopeRegistry = map[string]func() any{
	"system": func() any { s := defaultSystem(); return &s },
	"forwarding": func() any {
		s := defaultForwarding()
		return &s
	},
	"logging": func() any { s := defaultLogging(); return &s },
	"ai":      func() any { s := defaultAI(); return &s },
}

// ---------- Center ----------

// SettingsCenter 是配置中心：快照读、校验写、热更回调。
type SettingsCenter struct {
	db *sqlx.DB

	mu     sync.RWMutex
	loaded map[string]any // scope → 当前快照（*SystemSettings 等 typed 指针）

	subsMu sync.Mutex
	subs   map[string][]func(scope string)
}

func NewSettingsCenter(db *sqlx.DB) *SettingsCenter {
	return &SettingsCenter{
		db:     db,
		loaded: map[string]any{},
		subs:   map[string][]func(scope string){},
	}
}

// Load 全量加载：DB 无记录的 scope 用默认值；非法 JSON 报错（不静默）。
func (c *SettingsCenter) Load(ctx context.Context) error {
	rows, err := c.db.QueryxContext(ctx, "SELECT scope, data FROM settings")
	if err != nil {
		return fmt.Errorf("读取 settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	stored := map[string]json.RawMessage{}
	for rows.Next() {
		var scope string
		var data json.RawMessage
		if err := rows.Scan(&scope, &data); err != nil {
			return fmt.Errorf("扫描 settings 行: %w", err)
		}
		stored[scope] = data
	}
	if err := rows.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.loaded = map[string]any{}
	for scope, ctor := range scopeRegistry {
		snapshot := ctor()
		if raw, ok := stored[scope]; ok {
			if err := json.Unmarshal(raw, snapshot); err != nil {
				return fmt.Errorf("scope %s 存储数据非法: %w", scope, err)
			}
		}
		c.loaded[scope] = snapshot
	}
	return nil
}

// System 返回 system 快照。
func (c *SettingsCenter) System() SystemSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.loaded["system"].(*SystemSettings); ok {
		return *s
	}
	return defaultSystem()
}

// Forwarding 返回 forwarding 快照。
func (c *SettingsCenter) Forwarding() ForwardingSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.loaded["forwarding"].(*ForwardingSettings); ok {
		return *s
	}
	return defaultForwarding()
}

// Logging 返回 logging 快照。
func (c *SettingsCenter) Logging() LoggingSettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.loaded["logging"].(*LoggingSettings); ok {
		return *s
	}
	return defaultLogging()
}

// AI 返回 ai 快照。
func (c *SettingsCenter) AI() AISettings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.loaded["ai"].(*AISettings); ok {
		return *s
	}
	return defaultAI()
}

// Update 以字段级合并更新 scope：partial 反序列化到当前快照副本 → 校验 →
// 写库 → 快照原子替换 → 通知订阅者。校验失败或写库失败不改快照。
func (c *SettingsCenter) Update(ctx context.Context, scope string, partial map[string]any) error {
	ctor, ok := scopeRegistry[scope]
	if !ok {
		return fmt.Errorf("未知 scope: %s", scope)
	}

	c.mu.Lock()
	next := ctor()
	if cur, ok := c.loaded[scope]; ok {
		if raw, err := json.Marshal(cur); err == nil {
			if err := json.Unmarshal(raw, next); err != nil {
				c.mu.Unlock()
				return fmt.Errorf("复制当前快照: %w", err)
			}
		}
	}
	c.mu.Unlock()

	if len(partial) > 0 {
		patch, err := json.Marshal(partial)
		if err != nil {
			return fmt.Errorf("序列化 partial: %w", err)
		}
		if err := json.Unmarshal(patch, next); err != nil {
			return fmt.Errorf("应用 partial 到 %s: %w", scope, err)
		}
	}

	validator, ok := any(next).(interface{ Validate() error })
	if !ok {
		return fmt.Errorf("scope %s 缺少 Validate", scope)
	}
	if err := validator.Validate(); err != nil {
		return fmt.Errorf("校验 %s: %w", scope, err)
	}

	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `
		INSERT INTO settings (scope, data) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE data = VALUES(data)`, scope, data); err != nil {
		return fmt.Errorf("写 settings(%s): %w", scope, err)
	}

	c.mu.Lock()
	c.loaded[scope] = next
	c.mu.Unlock()

	c.notify(scope)
	return nil
}

// Subscribe 注册热更回调（进程内直接调用，无轮询；01 §6.3）。
func (c *SettingsCenter) Subscribe(scope string, fn func(scope string)) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if _, ok := scopeRegistry[scope]; !ok {
		return // 未知 scope 静默忽略（订阅 typo 不应 panic）
	}
	c.subs[scope] = append(c.subs[scope], fn)
}

func (c *SettingsCenter) notify(scope string) {
	c.subsMu.Lock()
	fns := append([]func(string){}, c.subs[scope]...)
	c.subsMu.Unlock()
	for _, fn := range fns {
		fn(scope) // 回调异常由订阅方自行防御；中心不因回调失败中断通知链
	}
}
