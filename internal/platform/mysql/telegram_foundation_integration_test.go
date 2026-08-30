//go:build integration

package mysql

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/gotd/td/session"
	"github.com/jmoiron/sqlx"
)

// testDB 返回连向 SAKURA_TEST_MYSQL_* 的池；未配置时跳过（T0.2 约定）。
// 只负责 raw 连接——不保证 schema 存在。
func testDB(t *testing.T) (*sqlx.DB, context.Context) {
	t.Helper()
	if os.Getenv("SAKURA_TEST_MYSQL_HOST") == "" {
		t.Skip("SAKURA_TEST_MYSQL_HOST 未设置（本地：export .env.test.local 中的变量）")
	}
	ctx := context.Background()
	db, err := Connect(ctx, Options{
		Host:     os.Getenv("SAKURA_TEST_MYSQL_HOST"),
		Port:     atoiDefault(os.Getenv("SAKURA_TEST_MYSQL_PORT"), 3306),
		User:     os.Getenv("SAKURA_TEST_MYSQL_USER"),
		Password: os.Getenv("SAKURA_TEST_MYSQL_PASSWORD"),
		Database: os.Getenv("SAKURA_TEST_MYSQL_DATABASE"),
	})
	if err != nil {
		t.Fatalf("连接测试 MySQL: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, ctx
}

// testMigratedDB 是业务表契约测试的 fixture：连接 + MigrateUp，
// 自带 schema 保证——**任何测试都不得依赖其他测试先跑迁移**
// （2026-08-29 CI 教训：CI 空库上 t13 契约先于迁移测试执行而失败）。
func testMigratedDB(t *testing.T) (*sqlx.DB, context.Context) {
	t.Helper()
	db, ctx := testDB(t)
	if err := MigrateUp(ctx, db.DB); err != nil {
		t.Fatalf("fixture 迁移: %v", err)
	}
	return db, ctx
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// TestMigrateFullCycle：migration runner 专项测试——Down 到 0（清空全部表与
// 版本记录）后 Up×2，在长期存在的库上也能验证「0001 从空库构建成功」+ 幂等。
// 本测试是唯一允许操作 schema 版本的测试，必须用 raw testDB。
func TestMigrateFullCycle(t *testing.T) {
	db, ctx := testDB(t)

	if err := MigrateDownTo(ctx, db.DB, 0); err != nil {
		t.Fatalf("DownTo(0): %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := MigrateUp(ctx, db.DB); err != nil {
			t.Fatalf("第 %d 次 MigrateUp: %v", i, err)
		}
	}

	for _, table := range []string{
		"gotd_sessions", "telegram_update_states", "telegram_channel_states",
		"telegram_peers", "telegram_peer_aliases", "messages", "message_revisions",
	} {
		var n int
		err := db.GetContext(ctx, &n,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", table)
		if err != nil || n != 1 {
			t.Errorf("表 %s 不存在（n=%d err=%v）", table, n, err)
		}
	}
}

// TestSessionStorageRoundtrip：T1.1 验证项——Load 未找到返回 session.ErrNotFound；
// Store 后 Load 返回原字节；重复 Store 为 upsert 覆盖（02 §2.1 写语义）。
func TestSessionStorageRoundtrip(t *testing.T) {
	db, ctx := testMigratedDB(t)
	storage := NewSessionStorage(db, "itest")
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, "DELETE FROM gotd_sessions WHERE account = 'itest'"); err != nil {
			t.Logf("清理 itest session 行失败: %v", err)
		}
	})

	// 初始：未找到
	if _, err := storage.LoadSession(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("初始 Load 应返回 ErrNotFound，得到 %v", err)
	}

	// 首次写入
	if err := storage.StoreSession(ctx, []byte("hello-session")); err != nil {
		t.Fatalf("Store(1): %v", err)
	}
	got, err := storage.LoadSession(ctx)
	if err != nil || string(got) != "hello-session" {
		t.Fatalf("Load(1) = %q err=%v", got, err)
	}

	// 覆盖（upsert，非新增行）
	if err := storage.StoreSession(ctx, []byte("world-session")); err != nil {
		t.Fatalf("Store(2): %v", err)
	}
	got, err = storage.LoadSession(ctx)
	if err != nil || string(got) != "world-session" {
		t.Fatalf("Load(2) = %q err=%v（应覆盖）", got, err)
	}
	var rows int
	if err := db.GetContext(ctx, &rows, "SELECT COUNT(*) FROM gotd_sessions WHERE account='itest'"); err != nil || rows != 1 {
		t.Errorf("upsert 后应恰 1 行（rows=%d err=%v）", rows, err)
	}
}
