//go:build integration

package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

func seedChannel(t *testing.T, ctx context.Context, db *Database) {
	t.Helper()
	_, _ = db.ExecContext(ctx, "DELETE FROM channel_settings WHERE channel_id = 777001")
	_, _ = db.ExecContext(ctx, "DELETE FROM channels WHERE tg_id = 777001")
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM channel_settings WHERE channel_id = 777001")
		_, _ = db.ExecContext(context.Background(), "DELETE FROM channels WHERE tg_id = 777001")
	})
}

// TestChannelRepoUpsertList：tg_id 身份 upsert、快照列更新、查询往返。
func TestChannelRepoUpsertList(t *testing.T) {
	db, ctx := testMigratedDB(t)
	d := WrapDatabase(db, nil)
	repo := NewChannelRepo(db)
	seedChannel(t, ctx, d)

	if err := repo.Upsert(ctx, domain.Channel{
		TgID: 777001, Username: "First", Title: "旧标题",
	}); err != nil {
		t.Fatal(err)
	}
	// 改名再 upsert：同 tg_id 更新而非新行
	if err := repo.Upsert(ctx, domain.Channel{
		TgID: 777001, Username: "Second", Title: "新标题", DiscussionChatID: 888, IsVerified: true,
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := repo.GetByTgID(ctx, 777001)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Username != "Second" || got.Title != "新标题" || got.DiscussionChatID != 888 || !got.IsVerified {
		t.Errorf("upsert 后字段不符: %+v", got)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range list {
		if c.TgID == 777001 && c.Username == "Second" {
			found = true
		}
	}
	if !found {
		t.Error("List 应包含 upsert 后的频道")
	}

	if err := repo.Delete(ctx, 777001); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := repo.GetByTgID(ctx, 777001); ok {
		t.Error("删除后不应存在")
	}
}

// TestRuleRepoCrudAndCursor：规则 CRUD 往返（JSON 列、五列 ChatRef）+
// contiguous cursor 只进不退（P0 Plan §6 的 repo 层保证）。
func TestRuleRepoCrudAndCursor(t *testing.T) {
	db, ctx := testMigratedDB(t)
	repo := NewForwardRuleRepo(db)
	seedRule := domain.ForwardRule{
		Name: "测试规则", Source: domain.NewChatRef(domain.PeerChannel, 111),
		SourceUsername: "src", Target: domain.NewChatRef(domain.PeerChannel, 222),
		Enabled: true, Keywords: []string{"流萤", "爆料"}, Patterns: []string{"v4\\.[0-9]"},
		MediaTypes: []string{"photo", "video"}, ForwardOriginalOnly: true, CopyMode: "copy",
		AIEnabled: false, CustomFooter: "[来自 {source_title}]", DelayMinSec: 0.5, DelayMaxSec: 2,
	}
	id, err := repo.Create(ctx, seedRule)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Delete(context.Background(), id) })

	got, ok, err := repo.Get(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Source != seedRule.Source || got.Target != seedRule.Target {
		t.Errorf("ChatRef 往返不符: %+v", got)
	}
	if len(got.Keywords) != 2 || got.Keywords[1] != "爆料" || len(got.Patterns) != 1 {
		t.Errorf("JSON 列往返不符: %+v", got)
	}
	if got.ForwardOriginalOnly != true || got.CustomFooter != seedRule.CustomFooter {
		t.Errorf("其余字段不符: %+v", got)
	}

	// cursor 前进语义
	if err := repo.AdvanceCursor(ctx, id, 500); err != nil {
		t.Fatal(err)
	}
	if err := repo.AdvanceCursor(ctx, id, 300); err != nil { // 后退不生效
		t.Fatal(err)
	}
	got, _, _ = repo.Get(ctx, id)
	if got.LastMessageID != 500 {
		t.Errorf("cursor 应只进不退: %d", got.LastMessageID)
	}

	// 启停过滤
	if err := repo.SetEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := repo.ListEnabled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range enabled {
		if r.ID == id {
			t.Error("禁用规则不应出现在 ListEnabled")
		}
	}
}

// TestForwardedRepoDedupAndStats：五列去重键、INSERT IGNORE 幂等、stats 累加。
func TestForwardedRepoDedupAndStats(t *testing.T) {
	db, ctx := testMigratedDB(t)
	d := WrapDatabase(db, nil)
	repo := NewForwardedRepo(db)
	src := domain.MessageRef{Chat: domain.NewChatRef(domain.PeerChannel, 111), MessageID: 42}
	target := domain.NewChatRef(domain.PeerChannel, 222)

	if exists, err := repo.Exists(ctx, src, target); err != nil || exists {
		t.Fatalf("初始不应存在: exists=%v err=%v", exists, err)
	}
	if err := repo.Record(ctx, src, target, 1, 9001, "hash1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Record(ctx, src, target, 1, 9001, "hash1"); err != nil { // 幂等
		t.Fatal(err)
	}
	if exists, _ := repo.Exists(ctx, src, target); !exists {
		t.Error("记录后应存在")
	}
	// 同源消息不同目标 → 不去重
	target2 := domain.NewChatRef(domain.PeerChannel, 333)
	if exists, _ := repo.Exists(ctx, src, target2); exists {
		t.Error("不同目标不应被去重")
	}

	if err := repo.IncrStats(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.IncrStats(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.IncrStats(ctx, 1, false); err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Forwarded int `db:"forwarded_count"`
		Failed    int `db:"failed_count"`
	}
	if err := d.GetContext(ctx, &stats,
		"SELECT forwarded_count, failed_count FROM forwarding_stats WHERE rule_id = 1"); err != nil {
		t.Fatal(err)
	}
	if stats.Forwarded != 2 || stats.Failed != 1 {
		t.Errorf("stats 累加不符: %+v", stats)
	}

	// 清理：立即过期
	if n, err := repo.CleanupBefore(ctx, -time.Hour); err != nil || n < 1 {
		t.Errorf("CleanupBefore: n=%d err=%v", n, err)
	}

	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), "DELETE FROM forwarding_stats WHERE rule_id = 1")
		_, _ = d.ExecContext(context.Background(),
			"DELETE FROM forwarded_messages WHERE source_chat_id IN (111)")
	})
}

// TestForwardedRepoContentDedup：content_hash 内容去重（防删帖重发，03 §3.5）——
// 同源同目标同内容命中；不同源/不同目标/不同内容不命中。
func TestForwardedRepoContentDedup(t *testing.T) {
	db, ctx := testMigratedDB(t)
	d := WrapDatabase(db, nil)
	repo := NewForwardedRepo(db)
	source := domain.NewChatRef(domain.PeerChannel, 1111)
	target := domain.NewChatRef(domain.PeerChannel, 2222)

	if exists, err := repo.ExistsByContent(ctx, source, target, "h1"); err != nil || exists {
		t.Fatalf("初始不应命中: exists=%v err=%v", exists, err)
	}
	// 删帖重发：旧消息已记录内容哈希，新 message_id 携带相同内容出现
	if err := repo.Record(ctx, domain.MessageRef{Chat: source, MessageID: 100},
		target, 1, 5001, "h1"); err != nil {
		t.Fatal(err)
	}
	if exists, err := repo.ExistsByContent(ctx, source, target, "h1"); err != nil || !exists {
		t.Fatalf("同源同目标同内容应命中: exists=%v err=%v", exists, err)
	}
	if exists, _ := repo.ExistsByContent(ctx, source, target, "h2"); exists {
		t.Error("不同内容不应命中")
	}
	source2 := domain.NewChatRef(domain.PeerChannel, 3333)
	if err := repo.Record(ctx, domain.MessageRef{Chat: source2, MessageID: 200},
		target, 1, 5002, "h2"); err != nil {
		t.Fatal(err)
	}
	if exists, _ := repo.ExistsByContent(ctx, source2, target, "h1"); exists {
		t.Error("不同源不应命中")
	}
	target2 := domain.NewChatRef(domain.PeerChannel, 4444)
	if exists, _ := repo.ExistsByContent(ctx, source, target2, "h1"); exists {
		t.Error("不同目标不应命中")
	}

	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(),
			"DELETE FROM forwarded_messages WHERE source_chat_id IN (1111, 3333)")
	})
}

// TestAuditRepoAppend：审计写入往返。
func TestAuditRepoAppend(t *testing.T) {
	db, ctx := testMigratedDB(t)
	d := WrapDatabase(db, nil)
	repo := NewAuditRepo(db)

	if err := repo.Append(ctx, domain.AuditEntry{
		Actor: "webui:admin", Action: "rule.create",
		Detail: map[string]any{"ruleId": float64(9)},
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.GetContext(ctx, &n,
		"SELECT COUNT(*) FROM system_audit_logs WHERE actor='webui:admin' AND action='rule.create'"); err != nil || n != 1 {
		t.Fatalf("audit 行: n=%d err=%v", n, err)
	}
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(),
			"DELETE FROM system_audit_logs WHERE actor='webui:admin' AND action='rule.create'")
	})
}
