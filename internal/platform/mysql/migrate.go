package mysql

import (
	"context"
	"database/sql"
	"fmt"

	goose "github.com/pressly/goose/v3"

	"github.com/Sakura520222/Sakura-Nexus/migrations"
)

// MigrateUp 以嵌入的迁移文件执行 goose Up（启动即迁移，01 §1.1）。
// 单一实现：Phase 1（T1.1）与后续全部迁移共用本 runner（P0 Plan R1.1 必改 1）。
// 迁移操作经 MySQL 命名锁全局串行化（2026-08-30 CI 竞态修复：config 与 mysql 两包
// 集成测试并行时，DownTo 与 MigrateUp 并发会读到版本表半途状态——missing zero
// version migration）。
func MigrateUp(ctx context.Context, db *sql.DB) error {
	return withMigrationLock(ctx, db, func(conn *sql.Conn) error {
		provider, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
		if err != nil {
			return fmt.Errorf("goose provider 构造失败: %w", err)
		}
		if _, err := provider.Up(ctx); err != nil {
			return fmt.Errorf("goose up 失败: %w", err)
		}
		return nil
	})
}

const migrationLockName = "sakura_migration_lock"

// withMigrationLock 以 MySQL 命名锁串行化迁移操作（连接级锁：持有期间其他
// 迁移调用阻塞；业务读写不受影响——测试隔离由临时库保证）。
func withMigrationLock(ctx context.Context, db *sql.DB, fn func(*sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取迁移连接: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 30)", migrationLockName).Scan(&locked); err != nil {
		return fmt.Errorf("获取迁移锁: %w", err)
	}
	if !locked.Valid || locked.Int64 != 1 {
		return fmt.Errorf("获取迁移锁超时（30s，另一迁移操作进行中）")
	}
	defer func() { _, _ = conn.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", migrationLockName) }()
	return fn(conn)
}
