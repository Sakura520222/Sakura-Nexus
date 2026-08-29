package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/gotd/td/telegram/updates"
)

// ErrStateNotFound 表示部分更新（SetPts 等）时 user state 行不存在——
// gotd StateStorage 接口注释要求的行为。
var ErrStateNotFound = errors.New("user update state 不存在（需先 SetState）")

// StateStorage 实现 gotd updates.StateStorage（02 §2.1）：
// account 逻辑槽 + user_id 状态身份分区（换号自然建新行，旧行由启动清理任务处理）。
type StateStorage struct {
	db      *sqlx.DB
	account string
}

func NewStateStorage(db *sqlx.DB, account string) *StateStorage {
	return &StateStorage{db: db, account: account}
}

// GetState 返回全局 update 恢复状态；未找到时 found=false。
func (s *StateStorage) GetState(ctx context.Context, userID int64) (updates.State, bool, error) {
	var row struct {
		Pts  int `db:"pts"`
		Qts  int `db:"qts"`
		Date int `db:"date"`
		Seq  int `db:"seq"`
	}
	err := s.db.GetContext(ctx, &row,
		"SELECT pts, qts, date, seq FROM telegram_update_states WHERE account = ? AND user_id = ?",
		s.account, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return updates.State{}, false, nil
	}
	if err != nil {
		return updates.State{}, false, fmt.Errorf("读取 state: %w", err)
	}
	return updates.State{Pts: row.Pts, Qts: row.Qts, Date: row.Date, Seq: row.Seq}, true, nil
}

// SetState 整体写入全局状态（upsert）。
func (s *StateStorage) SetState(ctx context.Context, userID int64, state updates.State) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO telegram_update_states (account, user_id, pts, qts, date, seq)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE pts = VALUES(pts), qts = VALUES(qts),
			date = VALUES(date), seq = VALUES(seq)`,
		s.account, userID, state.Pts, state.Qts, state.Date, state.Seq)
	if err != nil {
		return fmt.Errorf("写入 state: %w", err)
	}
	return nil
}

// 部分更新：要求行已存在（接口语义），否则 ErrStateNotFound。
func (s *StateStorage) updateStateField(ctx context.Context, userID int64, sets string, args ...any) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE telegram_update_states SET "+sets+" WHERE account = ? AND user_id = ?",
		append(args, s.account, userID)...)
	if err != nil {
		return fmt.Errorf("更新 state(%s): %w", sets, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrStateNotFound
	}
	return nil
}

func (s *StateStorage) SetPts(ctx context.Context, userID int64, pts int) error {
	return s.updateStateField(ctx, userID, "pts = ?", pts)
}

func (s *StateStorage) SetQts(ctx context.Context, userID int64, qts int) error {
	return s.updateStateField(ctx, userID, "qts = ?", qts)
}

func (s *StateStorage) SetDate(ctx context.Context, userID int64, date int) error {
	return s.updateStateField(ctx, userID, "date = ?", date)
}

func (s *StateStorage) SetSeq(ctx context.Context, userID int64, seq int) error {
	return s.updateStateField(ctx, userID, "seq = ?", seq)
}

func (s *StateStorage) SetDateSeq(ctx context.Context, userID int64, date, seq int) error {
	return s.updateStateField(ctx, userID, "date = ?, seq = ?", date, seq)
}

// GetChannelPts 返回频道级 PTS；未找到时 found=false。
func (s *StateStorage) GetChannelPts(ctx context.Context, userID, channelID int64) (int, bool, error) {
	var pts int
	err := s.db.GetContext(ctx, &pts,
		"SELECT pts FROM telegram_channel_states WHERE account = ? AND user_id = ? AND channel_id = ?",
		s.account, userID, channelID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("读取 channel pts: %w", err)
	}
	return pts, true, nil
}

// SetChannelPts 写入频道级 PTS（upsert）。
func (s *StateStorage) SetChannelPts(ctx context.Context, userID, channelID int64, pts int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO telegram_channel_states (account, user_id, channel_id, pts)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE pts = VALUES(pts)`,
		s.account, userID, channelID, pts)
	if err != nil {
		return fmt.Errorf("写入 channel pts: %w", err)
	}
	return nil
}

// ForEachChannels 遍历该用户的全部频道状态。
func (s *StateStorage) ForEachChannels(ctx context.Context, userID int64,
	f func(ctx context.Context, channelID int64, pts int) error,
) error {
	rows, err := s.db.QueryxContext(ctx,
		"SELECT channel_id, pts FROM telegram_channel_states WHERE account = ? AND user_id = ? ORDER BY channel_id",
		s.account, userID)
	if err != nil {
		return fmt.Errorf("遍历 channel states: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var channelID, pts int64
		if err := rows.Scan(&channelID, &pts); err != nil {
			return fmt.Errorf("扫描 channel state: %w", err)
		}
		if err := f(ctx, channelID, int(pts)); err != nil {
			return err
		}
	}
	return rows.Err()
}
