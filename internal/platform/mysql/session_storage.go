package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/gotd/td/session"
)

// SessionStorage 实现 gotd session.Storage：opaque blob 落 gotd_sessions 表
// （02 §2.1：不解析、不版本化；upsert 用 INSERT … ON DUPLICATE KEY UPDATE）。
type SessionStorage struct {
	db      *sqlx.DB
	account string // "user" / "bot" 逻辑槽
}

func NewSessionStorage(db *sqlx.DB, account string) *SessionStorage {
	return &SessionStorage{db: db, account: account}
}

// LoadSession 返回持久化的 session 字节；未登录时返回 session.ErrNotFound。
func (s *SessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	var data []byte
	err := s.db.GetContext(ctx, &data, "SELECT data FROM gotd_sessions WHERE account = ?", s.account)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, session.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("加载 session(%s): %w", s.account, err)
	}
	return data, nil
}

// StoreSession 以 upsert 持久化 session 字节（02 §2.1 写语义）。
func (s *SessionStorage) StoreSession(ctx context.Context, data []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO gotd_sessions (account, data) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE data = VALUES(data)`,
		s.account, data)
	if err != nil {
		return fmt.Errorf("保存 session(%s): %w", s.account, err)
	}
	return nil
}
