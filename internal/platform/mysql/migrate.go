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
func MigrateUp(ctx context.Context, db *sql.DB) error {
	provider, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider 构造失败: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("goose up 失败: %w", err)
	}
	return nil
}

// MigrateDownTo 回退到指定版本（0 = 全部撤销）。仅测试使用（TestMigrateFullCycle
// 的 fresh-schema 保证）；生产升级纪律是只加不改（06 §2），永不 Down。
func MigrateDownTo(ctx context.Context, db *sql.DB, version int64) error {
	provider, err := goose.NewProvider(goose.DialectMySQL, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("goose provider 构造失败: %w", err)
	}
	if _, err := provider.DownTo(ctx, version); err != nil {
		return fmt.Errorf("goose downTo(%d) 失败: %w", version, err)
	}
	return nil
}
