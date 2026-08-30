//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/jmoiron/sqlx"
	goose "github.com/pressly/goose/v3"

	"github.com/Sakura520222/Sakura-Nexus/migrations"
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

// TestMigrateFullCycle：migration runner 专项测试——优先在**独立临时库**上验证
// 「0001 从空库构建成功」+ 幂等（2026-08-30 竞态修复：此前在共享库 DownTo 会与
// config 包并行测试的 MigrateUp 互相踩——missing zero version migration）。
// 无 CREATE DATABASE 权限时退化为共享库幂等验证（不动 Down）。
func TestMigrateFullCycle(t *testing.T) {
	db, ctx := testDB(t)

	cycleDB := fmt.Sprintf("sakura_test_cycle_%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+cycleDB+" CHARACTER SET utf8mb4"); err != nil {
		t.Logf("无 CREATE DATABASE 权限（%v）——退化为共享库 Up×2 幂等验证（跳过 Down）", err)
		for i := 1; i <= 2; i++ {
			if err := MigrateUp(ctx, db.DB); err != nil {
				t.Fatalf("第 %d 次 MigrateUp: %v", i, err)
			}
		}
		assertTables(t, db, ctx)
		return
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+cycleDB)
	})

	// 建临时库连接（权限同主连接；独立 database 与其他测试完全隔离）
	cycle, err := Connect(ctx, Options{
		Host:     os.Getenv("SAKURA_TEST_MYSQL_HOST"),
		Port:     atoiDefault(os.Getenv("SAKURA_TEST_MYSQL_PORT"), 3306),
		User:     os.Getenv("SAKURA_TEST_MYSQL_USER"),
		Password: os.Getenv("SAKURA_TEST_MYSQL_PASSWORD"),
		Database: cycleDB,
	})
	if err != nil {
		t.Fatalf("连接临时库: %v", err)
	}
	t.Cleanup(func() { cycle.Close() })

	if err := migrateDownTo(ctx, cycle.DB, 0); err != nil {
		t.Fatalf("DownTo(0)（空库应无操作）: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := MigrateUp(ctx, cycle.DB); err != nil {
			t.Fatalf("第 %d 次 MigrateUp: %v", i, err)
		}
	}
	assertTables(t, cycle, ctx)
}

func assertTables(t *testing.T, db *sqlx.DB, ctx context.Context) {
	t.Helper()
	// 0001 七张 + 0002 七张（T2.1：空库 Up 自动顺序升级到最新）
	for _, table := range []string{
		"gotd_sessions", "telegram_update_states", "telegram_channel_states",
		"telegram_peers", "telegram_peer_aliases", "messages", "message_revisions",
		"settings", "channels", "channel_settings", "forward_rules",
		"forwarded_messages", "forwarding_stats", "system_audit_logs",
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

// migrateDownTo 回退 schema 到指定版本——仅测试使用（TestMigrateFullCycle 的
// fresh-schema 保证）。生产 API 不暴露回滚入口（06 §2：升级只加不改，永不 Down）。
// 与 MigrateUp 共用同一把命名锁（migrate.go），防止跨包并发竞态。
func migrateDownTo(ctx context.Context, db *sql.DB, version int64) error {
	return withMigrationLock(ctx, db, func(conn *sql.Conn) error {
		provider, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
		if err != nil {
			return fmt.Errorf("goose provider 构造失败: %w", err)
		}
		if _, err := provider.DownTo(ctx, version); err != nil {
			return fmt.Errorf("goose downTo(%d) 失败: %w", version, err)
		}
		return nil
	})
}
