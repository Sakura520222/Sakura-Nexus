// Package mysql 提供 sqlx 连接池、goose 迁移 runner、repositories 实现与
// gotd Telegram 持久状态存储（session/update state/peers/aliases）。
// 设计：docs/design/02-storage.md §2.1。
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // 注册 mysql 驱动
	"github.com/jmoiron/sqlx"
)

// Options 是 MySQL 连接配置（platform 层不依赖 config 包，保持可测）。
type Options struct {
	Host         string
	Port         int
	User         string
	Password     string
	Database     string
	MaxOpenConns int
}

// DSN 构建驱动连接串。02 §1.2：DATETIME(6) UTC——parseTime + loc=UTC。
func (o Options) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&loc=UTC&charset=utf8mb4&timeout=10s",
		o.User, o.Password, o.Host, o.Port, o.Database)
}

// Connect 建立连接池并 Ping（短暂失败由调用方决定重试；T2.2 完善语义）。
func Connect(ctx context.Context, opts Options) (*sqlx.DB, error) {
	db, err := sqlx.ConnectContext(ctx, "mysql", opts.DSN())
	if err != nil {
		return nil, fmt.Errorf("mysql 连接失败: %w", err)
	}
	maxOpen := opts.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = 5
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxLifetime(time.Hour)
	return db, nil
}

// Ping 留作健康检查入口（06 §1 healthcheck 子命令复用）。
func Ping(ctx context.Context, db *sql.DB) error {
	return db.PingContext(ctx)
}
