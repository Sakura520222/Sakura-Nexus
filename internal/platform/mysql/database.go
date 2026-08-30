package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Database 在 sqlx.DB 之上提供项目统一的事务与重试语义（03 §1.4）：
//   - 连接池负责重连；仅 repository 明确判定幂等的操作可 retry 一次
//   - 事务提交状态未知时不得自动重放（防重复 revision/统计）——上层状态机收敛
//   - WithTx：Commit 前 fn 返回错误或 panic 均回滚
type Database struct {
	*sqlx.DB
	lg *slog.Logger
}

func WrapDatabase(db *sqlx.DB, lg *slog.Logger) *Database {
	if lg == nil {
		lg = slog.Default()
	}
	return &Database{DB: db, lg: lg}
}

// WithTx 在事务中执行 fn：fn 返回错误或 panic → Rollback；成功 → Commit。
// Commit 自身失败时提交状态未知（网络断裂），错误原样返回，**不重放**。
func (d *Database) WithTx(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	tx, err := d.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() // 已 Commit 或尚未开始时为 no-op
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务（状态未知，不重放）: %w", err)
	}
	committed = true
	return nil
}

// isTransient 判定连接级瞬时错误（连接断开/服务器离开）——池重连后可重试。
func isTransient(err error) bool {
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 2006 CR_SERVER_GONE_ERROR / 2013 CR_SERVER_LOST
		return myErr.Number == 2006 || myErr.Number == 2013
	}
	return errors.Is(err, mysql.ErrInvalidConn) || errors.Is(err, driver.ErrBadConn)
}

// RetryIdempotent 重试**幂等**操作一次（读、按唯一键 upsert、显式声明幂等的写入）。
// 事务整体不适用（提交状态未知时无法安全重放）——03 §1.4 矩阵的代码化。
func (d *Database) RetryIdempotent(ctx context.Context, op string, fn func(ctx context.Context) error) error {
	err := fn(ctx)
	if err == nil || !isTransient(err) {
		return err
	}
	d.lg.Warn("幂等操作遇连接级瞬时错误，重试一次", "op", op, "err", err)
	if rerr := ctx.Err(); rerr != nil {
		return err // ctx 已取消则不再重试
	}
	return fn(ctx)
}
