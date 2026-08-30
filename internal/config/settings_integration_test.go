//go:build integration

package config

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
)

// testCenter 建立「迁移就绪库 + 已 Load 的 SettingsCenter」。
// config 包自带 fixture（SAKURA_TEST_MYSQL_* 约定与 mysql 包一致）。
func testCenter(t *testing.T) (*SettingsCenter, context.Context) {
	t.Helper()
	if os.Getenv("SAKURA_TEST_MYSQL_HOST") == "" {
		t.Skip("SAKURA_TEST_MYSQL_HOST 未设置")
	}
	ctx := context.Background()
	db, err := mysql.Connect(ctx, mysql.Options{
		Host:     os.Getenv("SAKURA_TEST_MYSQL_HOST"),
		Port:     atoi(os.Getenv("SAKURA_TEST_MYSQL_PORT"), 3306),
		User:     os.Getenv("SAKURA_TEST_MYSQL_USER"),
		Password: os.Getenv("SAKURA_TEST_MYSQL_PASSWORD"),
		Database: os.Getenv("SAKURA_TEST_MYSQL_DATABASE"),
	})
	if err != nil {
		t.Fatalf("连接测试 MySQL: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := mysql.MigrateUp(ctx, db.DB); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	c := NewSettingsCenter(db)
	if err := c.Load(ctx); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 测试间隔离：结束后清掉本测试写入的 settings 行
	t.Cleanup(func() {
		_, _ = db.Exec("DELETE FROM settings WHERE scope IN ('system','forwarding','logging','ai')")
	})
	return c, ctx
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func TestSettingsLoadDefaults(t *testing.T) {
	c, _ := testCenter(t)

	if got := c.System(); got.Language != "zh-CN" || !got.NotifyAdminsOnStart {
		t.Errorf("system 默认不符: %+v", got)
	}
	if got := c.Forwarding(); !got.ShowDefaultFooter || got.DedupDays != 30 || got.AlbumQuietMs != 450 {
		t.Errorf("forwarding 默认不符: %+v", got)
	}
	if got := c.Logging(); got.Level != "info" {
		t.Errorf("logging 默认不符: %+v", got)
	}
	if got := c.AI(); got.Temperature != 0.7 || got.TimeoutSeconds != 60 {
		t.Errorf("ai 默认不符: %+v", got)
	}
}

func TestSettingsUpdateMergeAndNotify(t *testing.T) {
	c, ctx := testCenter(t)

	var notified sync.Map
	c.Subscribe("forwarding", func(scope string) { notified.Store(scope, true) })

	// 部分更新：只改两个字段，其余保留默认
	if err := c.Update(ctx, "forwarding", map[string]any{
		"dedup_days":    7,
		"content_dedup": true,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := c.Forwarding()
	if got.DedupDays != 7 || !got.ContentDedup {
		t.Errorf("更新字段未生效: %+v", got)
	}
	if !got.ShowDefaultFooter || got.AlbumQuietMs != 450 {
		t.Errorf("未更新字段应保留默认: %+v", got)
	}
	if v, ok := notified.Load("forwarding"); !ok || v != true {
		t.Error("订阅回调未触发")
	}

	// 持久化：新实例 Load 读回
	if err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.Forwarding(); got.DedupDays != 7 {
		t.Errorf("重载后应持久: %+v", got)
	}
}

func TestSettingsValidationRejects(t *testing.T) {
	c, ctx := testCenter(t)

	cases := []struct {
		scope   string
		partial map[string]any
	}{
		{"forwarding", map[string]any{"dedup_days": -1}},
		{"forwarding", map[string]any{"default_delay_min_sec": 5.0, "default_delay_max_sec": 1.0}}, // min > max
		{"forwarding", map[string]any{"album_hard_deadline_ms": 100}},                              // < quiet
		{"logging", map[string]any{"level": "verbose"}},
		{"ai", map[string]any{"temperature": 9.9}},
		{"nonexistent", map[string]any{"x": 1}},
	}
	for _, tc := range cases {
		if err := c.Update(ctx, tc.scope, tc.partial); err == nil {
			t.Errorf("Update(%s, %v) 应被拒绝", tc.scope, tc.partial)
		}
	}
	// 被拒绝的更新不落库、不改快照
	if got := c.Forwarding(); got.DedupDays != 30 {
		t.Errorf("拒绝后快照被污染: %+v", got)
	}
}
