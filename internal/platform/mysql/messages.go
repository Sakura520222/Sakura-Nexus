package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// MessageRepository 是 canonical message 的单一写入入口（02 §2.3 写入协议）：
//   - WriteNew：不存在则 INSERT + revision(create)；已存在 → 幂等吸收（补抓/重复
//     更新被 UNIQUE(chat_type, chat_id, message_id) 吸收——GATE-1 恢复语义）
//   - WriteEdit：current_revision+1 + revision(edit)；未见过的消息按 New 入库
//   - WriteDelete：同一事务内 deleted_at + current_revision+1 +
//     embedding_state='delete_pending' + revision(delete)——事务内不调用任何
//     外部系统（R3.1.1 durability ≠ queue delivery）；重复 Delete 幂等
type MessageRepository struct {
	db *Database
}

func NewMessageRepository(db *sqlx.DB) *MessageRepository {
	return &MessageRepository{db: WrapDatabase(db, nil)}
}

type msgRow struct {
	ID              int64      `db:"id"`
	CurrentRevision int        `db:"current_revision"`
	DeletedAt       *time.Time `db:"deleted_at"`
}

func (r *MessageRepository) findByRef(ctx context.Context, ref domain.MessageRef) (msgRow, bool, error) {
	var row msgRow
	err := r.db.GetContext(ctx, &row, `
		SELECT id, current_revision, deleted_at FROM messages
		WHERE chat_type = ? AND chat_id = ? AND message_id = ?`,
		ref.Chat.Kind.String(), ref.Chat.ID, ref.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return msgRow{}, false, nil
	}
	if err != nil {
		return msgRow{}, false, fmt.Errorf("查询 canonical message: %w", err)
	}
	return row, true, nil
}

func mediaJSON(media []domain.MediaRef) any {
	if len(media) == 0 {
		return nil
	}
	b, err := json.Marshal(media)
	if err != nil {
		return nil
	}
	return b
}

// WriteNew 幂等写入新消息；返回是否实际新建（false=被唯一键吸收）。
func (r *MessageRepository) WriteNew(ctx context.Context, m domain.ChannelMessage) (created bool, err error) {
	if existing, ok, err := r.findByRef(ctx, m.Ref); err != nil {
		return false, err
	} else if ok {
		_ = existing
		return false, nil // 幂等吸收（重复更新/补抓）
	}

	err = r.db.WithTx(ctx, func(tx *sqlx.Tx) error {
		return r.insertNew(ctx, tx, m)
	})
	if errors.Is(err, errAlreadyExists) {
		return false, nil // 幂等吸收
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// errAlreadyExists 表示唯一键已存在（ON DUPLICATE 吞并），New 被幂等吸收。
var errAlreadyExists = errors.New("canonical message already exists")

func (r *MessageRepository) insertNew(ctx context.Context, tx *sqlx.Tx, m domain.ChannelMessage) error {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO messages
			(chat_type, chat_id, message_id, source_type, thread_top_id,
			 sender_user_id, sender_username, sender_display_name,
			 text, media, published_at, edited_at, current_revision, embedding_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 'pending')
		ON DUPLICATE KEY UPDATE id = id`,
		m.Ref.Chat.Kind.String(), m.Ref.Chat.ID, m.Ref.MessageID,
		sourceTypeOrDefault(m.SourceType), nullInt64(m.ThreadTopID),
		nullInt64(m.SenderUserID), m.SenderUsername, m.SenderDisplayName,
		m.Text, mediaJSON(m.Media), m.PublishedAt.UTC(), timePtrUTC(m.EditedAt))
	if err != nil {
		return fmt.Errorf("插入 canonical message: %w", err)
	}
	id, _ := res.LastInsertId()
	// ON DUPLICATE 吞并并发竞态：影响行数为 0 表示已存在，跳过 revision——
	// 返回哨兵错误让外层区分"已存在吸收"与"新建成功"
	if n, _ := res.RowsAffected(); n == 0 {
		return errAlreadyExists
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_revisions (message_id, revision, event_type, text, media, edited_at)
		VALUES (?, 0, 'create', ?, ?, NULL)`,
		id, m.Text, mediaJSON(m.Media)); err != nil {
		return fmt.Errorf("插入 create revision: %w", err)
	}
	return nil
}

// WriteEdit 记录编辑修订；未见过的消息以当前内容按 New 入库（canonical 完整性）。
func (r *MessageRepository) WriteEdit(ctx context.Context, m domain.ChannelMessage) error {
	row, ok, err := r.findByRef(ctx, m.Ref)
	if err != nil {
		return err
	}
	if !ok {
		_, err := r.WriteNew(ctx, m)
		return err
	}

	return r.db.WithTx(ctx, func(tx *sqlx.Tx) error {
		newRev := row.CurrentRevision + 1
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET text = ?, media = ?, edited_at = ?, current_revision = ?
			WHERE id = ?`,
			m.Text, mediaJSON(m.Media), timePtrUTC(m.EditedAt), newRev, row.ID); err != nil {
			return fmt.Errorf("更新 canonical message: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_revisions (message_id, revision, event_type, text, media, edited_at)
			VALUES (?, ?, 'edit', ?, ?, ?)`,
			row.ID, newRev, m.Text, mediaJSON(m.Media), timePtrUTC(m.EditedAt)); err != nil {
			return fmt.Errorf("插入 edit revision: %w", err)
		}
		return nil
	})
}

// WriteDelete 事务化删除状态机（02 §2.3）：未见过/已删除均幂等返回。
func (r *MessageRepository) WriteDelete(ctx context.Context, ref domain.MessageRef) error {
	row, ok, err := r.findByRef(ctx, ref)
	if err != nil {
		return err
	}
	if !ok || row.DeletedAt != nil {
		return nil
	}

	return r.db.WithTx(ctx, func(tx *sqlx.Tx) error {
		newRev := row.CurrentRevision + 1
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages
			SET deleted_at = NOW(6), current_revision = ?, embedding_state = 'delete_pending'
			WHERE id = ? AND deleted_at IS NULL`,
			newRev, row.ID); err != nil {
			return fmt.Errorf("更新删除状态: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_revisions (message_id, revision, event_type)
			VALUES (?, ?, 'delete')`, row.ID, newRev); err != nil {
			return fmt.Errorf("插入 delete revision: %w", err)
		}
		return nil
	})
}

func sourceTypeOrDefault(s string) string {
	if s == "" {
		return "channel_message"
	}
	return s
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func timePtrUTC(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}
