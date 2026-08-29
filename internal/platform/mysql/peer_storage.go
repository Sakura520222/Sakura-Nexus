package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/jmoiron/sqlx"
)

// PeerStorage 实现 contrib storage.PeerStorage（Add/Find/Assign/Resolve/Iterate），
// 持久化为 telegram_peers / telegram_peer_aliases（02 §2.1：PK(account, peer_type,
// peer_id)，storage.Peer 序列化入 data；重启后 Resolve/Assign 语义完整）。
type PeerStorage struct {
	db      *sqlx.DB
	account string
}

func NewPeerStorage(db *sqlx.DB, account string) *PeerStorage {
	return &PeerStorage{db: db, account: account}
}

func peerType(kind dialogs.PeerKind) string {
	switch kind {
	case dialogs.User:
		return "user"
	case dialogs.Chat:
		return "chat"
	case dialogs.Channel:
		return "channel"
	default:
		return "unknown"
	}
}

// Add 新增或更新 peer（data 为 storage.Peer JSON；username/title 为展示快照）。
func (p *PeerStorage) Add(ctx context.Context, value storage.Peer) error {
	key := storage.KeyFromPeer(value)
	data, err := value.MarshalJSON()
	if err != nil {
		return fmt.Errorf("序列化 peer: %w", err)
	}
	username, title := peerSnapshots(value)
	_, err = p.db.ExecContext(ctx, `
		INSERT INTO telegram_peers (account, peer_type, peer_id, data, username, title)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE data = VALUES(data), username = VALUES(username), title = VALUES(title)`,
		p.account, peerType(key.Kind), key.ID, data, username, title)
	if err != nil {
		return fmt.Errorf("写入 peer: %w", err)
	}
	return nil
}

// Find 按 (kind, id) 查找 peer。
func (p *PeerStorage) Find(ctx context.Context, key storage.PeerKey) (storage.Peer, error) {
	var data []byte
	err := p.db.GetContext(ctx, &data,
		"SELECT data FROM telegram_peers WHERE account = ? AND peer_type = ? AND peer_id = ?",
		p.account, peerType(key.Kind), key.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Peer{}, storage.ErrPeerNotFound
	}
	if err != nil {
		return storage.Peer{}, fmt.Errorf("查询 peer: %w", err)
	}
	var peer storage.Peer
	if err := peer.UnmarshalJSON(data); err != nil {
		return storage.Peer{}, fmt.Errorf("反序列化 peer: %w", err)
	}
	return peer, nil
}

// normalizeAlias 归一化 contrib 的关联键（"@username"/"username"/phone）到
// (alias_type, alias_value)：以 + 开头或纯数字视为 phone，否则 username（去 @、转小写）。
func normalizeAlias(key string) (typ, value string) {
	k := strings.TrimPrefix(strings.TrimSpace(key), "@")
	if strings.HasPrefix(k, "+") || isDigits(k) {
		if k != "" {
			return "phone", k
		}
	}
	return "username", strings.ToLower(k)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Assign 保存 peer 并关联别名（username/phone → peer），upsert 替换旧绑定。
func (p *PeerStorage) Assign(ctx context.Context, key string, value storage.Peer) error {
	if err := p.Add(ctx, value); err != nil {
		return err
	}
	pk := storage.KeyFromPeer(value)
	typ, val := normalizeAlias(key)
	if val == "" {
		return nil
	}
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO telegram_peer_aliases (account, alias_type, alias_value, peer_type, peer_id)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE peer_type = VALUES(peer_type), peer_id = VALUES(peer_id)`,
		p.account, typ, val, peerType(pk.Kind), pk.ID)
	if err != nil {
		return fmt.Errorf("写入 peer alias: %w", err)
	}
	return nil
}

// Resolve 按别名查找 peer。
func (p *PeerStorage) Resolve(ctx context.Context, key string) (storage.Peer, error) {
	typ, val := normalizeAlias(key)
	if val == "" {
		return storage.Peer{}, storage.ErrPeerNotFound
	}
	var row struct {
		PeerType string `db:"peer_type"`
		PeerID   int64  `db:"peer_id"`
	}
	err := p.db.GetContext(ctx, &row,
		"SELECT peer_type, peer_id FROM telegram_peer_aliases WHERE account = ? AND alias_type = ? AND alias_value = ?",
		p.account, typ, val)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.Peer{}, storage.ErrPeerNotFound
	}
	if err != nil {
		return storage.Peer{}, fmt.Errorf("查询 peer alias: %w", err)
	}
	return p.Find(ctx, storage.PeerKey{Kind: kindFromString(row.PeerType), ID: row.PeerID})
}

func kindFromString(s string) dialogs.PeerKind {
	switch s {
	case "user":
		return dialogs.User
	case "chat":
		return dialogs.Chat
	case "channel":
		return dialogs.Channel
	default:
		return dialogs.User
	}
}

// Iterate 按主键序遍历全部 peer。
func (p *PeerStorage) Iterate(ctx context.Context) (storage.PeerIterator, error) {
	rows, err := p.db.QueryxContext(ctx,
		"SELECT data FROM telegram_peers WHERE account = ? ORDER BY peer_type, peer_id", p.account)
	if err != nil {
		return nil, fmt.Errorf("遍历 peers: %w", err)
	}
	return &peerIterator{rows: rows}, nil
}

type peerIterator struct {
	rows *sqlx.Rows
	peer storage.Peer
	err  error
}

func (it *peerIterator) Next(_ context.Context) bool {
	if it.err != nil || !it.rows.Next() {
		return false
	}
	var data []byte
	if err := it.rows.Scan(&data); err != nil {
		it.err = err
		return false
	}
	var peer storage.Peer
	if err := peer.UnmarshalJSON(data); err != nil {
		it.err = err
		return false
	}
	it.peer = peer
	return true
}

func (it *peerIterator) Err() error {
	if it.err != nil {
		return it.err
	}
	return it.rows.Err()
}

func (it *peerIterator) Value() storage.Peer { return it.peer }

func (it *peerIterator) Close() error { return it.rows.Close() }

// peerSnapshots 提取展示用 username/title。
func peerSnapshots(p storage.Peer) (username, title string) {
	switch {
	case p.User != nil:
		return p.User.Username, p.User.FirstName
	case p.Channel != nil:
		return p.Channel.Username, p.Channel.Title
	case p.Chat != nil:
		return "", p.Chat.Title
	}
	return "", ""
}
