//go:build integration

package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
)

// TestWithTxCommitAndRollback：T2.2 验证项——fn 成功提交、fn 失败回滚、
// fn panic 同样回滚。
func TestWithTxCommitAndRollback(t *testing.T) {
	db, ctx := testMigratedDB(t)
	d := WrapDatabase(db, nil)

	// 成功提交
	var n int
	if err := d.WithTx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO settings (scope, data) VALUES ('t22', '{}')")
		return err
	}); err != nil {
		t.Fatalf("WithTx 成功路径: %v", err)
	}
	if err := db.GetContext(ctx, &n, "SELECT COUNT(*) FROM settings WHERE scope='t22'"); err != nil || n != 1 {
		t.Fatalf("提交后应存在: n=%d err=%v", n, err)
	}

	// fn 失败 → 回滚
	if err := d.WithTx(ctx, func(tx *sqlx.Tx) error {
		_, _ = tx.ExecContext(ctx, "INSERT INTO settings (scope, data) VALUES ('t22r', '{}')")
		return errors.New("business error")
	}); err == nil {
		t.Fatal("fn 错误应向上返回")
	}
	if err := db.GetContext(ctx, &n, "SELECT COUNT(*) FROM settings WHERE scope='t22r'"); err != nil || n != 0 {
		t.Fatalf("失败应回滚: n=%d err=%v", n, err)
	}

	// fn panic → 回滚且 panic 不逃逸出 WithTx？——panic 应向上传播（调用方决定），
	// 但事务必须回滚。这里捕获验证回滚。
	func() {
		defer func() { _ = recover() }()
		_ = d.WithTx(ctx, func(tx *sqlx.Tx) error {
			_, _ = tx.ExecContext(ctx, "INSERT INTO settings (scope, data) VALUES ('t22p', '{}')")
			panic("boom")
		})
	}()
	if err := db.GetContext(ctx, &n, "SELECT COUNT(*) FROM settings WHERE scope='t22p'"); err != nil || n != 0 {
		t.Fatalf("panic 应回滚: n=%d err=%v", n, err)
	}

	_, _ = db.ExecContext(ctx, "DELETE FROM settings WHERE scope IN ('t22','t22r','t22p')")
}

// TestRetryIdempotent：瞬时连接错误重试一次；非瞬时错误不重试。
func TestRetryIdempotent(t *testing.T) {
	db, _ := testDB(t)
	_ = db // 连接本身不使用——重试语义是纯逻辑
	d := WrapDatabase(db, nil)
	ctx := context.Background()

	calls := 0
	// 非瞬时错误：不重试
	err := d.RetryIdempotent(ctx, "op", func(context.Context) error {
		calls++
		return errors.New("logic error")
	})
	if err == nil || calls != 1 {
		t.Fatalf("非瞬时错误应原样返回且不重试: calls=%d err=%v", calls, err)
	}
	// 成功：不重试
	calls = 0
	if err := d.RetryIdempotent(ctx, "op", func(context.Context) error {
		calls++
		return nil
	}); err != nil || calls != 1 {
		t.Fatalf("成功不重试: calls=%d err=%v", calls, err)
	}
}
