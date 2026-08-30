//go:build integration

package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// TestStateStorageContract：T1.3 验证项——gotd StateStorage 全接口往返
// （GetState found/not found、SetState upsert、部分更新要求行存在、channel PTS、ForEach）。
func TestStateStorageContract(t *testing.T) {
	db, ctx := testMigratedDB(t)
	const uid int64 = 900001
	st := NewStateStorage(db, "itest")
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM telegram_update_states WHERE account='itest'")
		_, _ = db.ExecContext(ctx, "DELETE FROM telegram_channel_states WHERE account='itest'")
	})

	// 未找到
	if _, found, err := st.GetState(ctx, uid); err != nil || found {
		t.Fatalf("初始 GetState: found=%v err=%v", found, err)
	}
	// 部分更新在行不存在时报错（接口语义）
	if err := st.SetPts(ctx, uid, 5); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("无行 SetPts 应报 ErrStateNotFound，得到 %v", err)
	}
	// SetState → GetState
	if err := st.SetState(ctx, uid, updates.State{Pts: 10, Qts: 20, Date: 100, Seq: 2}); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.GetState(ctx, uid)
	if err != nil || !found {
		t.Fatalf("GetState: found=%v err=%v", found, err)
	}
	if got != (updates.State{Pts: 10, Qts: 20, Date: 100, Seq: 2}) {
		t.Errorf("GetState = %+v", got)
	}
	// 部分更新
	if err := st.SetPts(ctx, uid, 11); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDateSeq(ctx, uid, 200, 3); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.GetState(ctx, uid)
	if got.Pts != 11 || got.Date != 200 || got.Seq != 3 || got.Qts != 20 {
		t.Errorf("部分更新后 = %+v", got)
	}
	// channel PTS
	if _, found, _ := st.GetChannelPts(ctx, uid, 777); found {
		t.Error("初始 channel pts 不应 found")
	}
	if err := st.SetChannelPts(ctx, uid, 777, 42); err != nil {
		t.Fatal(err)
	}
	if err := st.SetChannelPts(ctx, uid, 777, 43); err != nil { // upsert
		t.Fatal(err)
	}
	pts, found, _ := st.GetChannelPts(ctx, uid, 777)
	if !found || pts != 43 {
		t.Errorf("channel pts = %d found=%v", pts, found)
	}
	// ForEach
	n := 0
	err = st.ForEachChannels(ctx, uid, func(_ context.Context, channelID int64, p int) error {
		n++
		if channelID != 777 || p != 43 {
			t.Errorf("ForEach: channel=%d pts=%d", channelID, p)
		}
		return nil
	})
	if err != nil || n != 1 {
		t.Errorf("ForEach: n=%d err=%v", n, err)
	}
}

// TestPeerStorageContract：T1.3 验证项——contrib PeerStorage 接口往返
// （Add/Find、Assign/Resolve 归一化、Iterate、not found）。
func TestPeerStorageContract(t *testing.T) {
	db, ctx := testMigratedDB(t)
	ps := NewPeerStorage(db, "itest")
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM telegram_peers WHERE account='itest'")
		_, _ = db.ExecContext(ctx, "DELETE FROM telegram_peer_aliases WHERE account='itest'")
	})

	ch := storage.Peer{
		Version: storage.LatestVersion,
		Key:     dialogs.DialogKey{Kind: dialogs.Channel, ID: 100200},
		Channel: &tg.Channel{
			ID: 100200, AccessHash: 987654, Username: "FooChan", Title: "Foo 频道",
			Photo: &tg.ChatPhotoEmpty{}, // contrib 序列化要求 photo 非空
		},
	}
	if err := ps.Add(ctx, ch); err != nil {
		t.Fatal(err)
	}
	got, err := ps.Find(ctx, storage.PeerKey{Kind: dialogs.Channel, ID: 100200})
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel == nil || got.Channel.AccessHash != 987654 || got.Channel.Title != "Foo 频道" {
		t.Errorf("Find 往返失真: %+v", got)
	}
	if _, err := ps.Find(ctx, storage.PeerKey{Kind: dialogs.User, ID: 1}); !errors.Is(err, storage.ErrPeerNotFound) {
		t.Errorf("未找到应报 ErrPeerNotFound: %v", err)
	}

	// Assign + Resolve（@username 归一化为小写）
	if err := ps.Assign(ctx, "@FooChan", ch); err != nil {
		t.Fatal(err)
	}
	r, err := ps.Resolve(ctx, "foochan")
	if err != nil {
		t.Fatal(err)
	}
	if r.Key.ID != 100200 {
		t.Errorf("Resolve 得 id=%d", r.Key.ID)
	}
	if _, err := ps.Resolve(ctx, "nobody"); !errors.Is(err, storage.ErrPeerNotFound) {
		t.Errorf("Resolve 未找到应报 ErrPeerNotFound: %v", err)
	}

	// Iterate
	iter, err := ps.Iterate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = iter.Close() }()
	n := 0
	for iter.Next(ctx) {
		n++
	}
	if err := iter.Err(); err != nil || n != 1 {
		t.Errorf("Iterate n=%d err=%v", n, err)
	}
}

func testMsg(id int64, text string) domain.ChannelMessage {
	return domain.ChannelMessage{
		Ref: domain.MessageRef{
			Chat:      domain.NewChatRef(domain.PeerChannel, 100200),
			MessageID: id,
		},
		SourceType: "channel_message",
		Text:       text,
		PublishedAt: func() time.Time {
			tt, _ := time.Parse(time.RFC3339, "2026-08-29T10:00:00Z")
			return tt
		}(),
	}
}

// TestMessageRepositoryProtocol：T1.3 验证项——canonical 写入协议
// （New 幂等吸收 / Edit revision / Delete 事务化状态机 / revisions 事件序列）。
func TestMessageRepositoryProtocol(t *testing.T) {
	db, ctx := testMigratedDB(t)
	repo := NewMessageRepository(db)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `
			DELETE mr FROM message_revisions mr
			JOIN messages m ON mr.message_id = m.id
			WHERE m.chat_type='channel' AND m.chat_id=100200`)
		_, _ = db.ExecContext(ctx, "DELETE FROM messages WHERE chat_type='channel' AND chat_id=100200")
	})

	// New → created
	created, err := repo.WriteNew(ctx, testMsg(1, "hello"))
	if err != nil || !created {
		t.Fatalf("WriteNew: created=%v err=%v", created, err)
	}
	// 重复 New → 幂等吸收
	created, err = repo.WriteNew(ctx, testMsg(1, "hello"))
	if err != nil || created {
		t.Fatalf("重复 WriteNew 应吸收: created=%v err=%v", created, err)
	}

	// Edit → revision 1
	edited := testMsg(1, "hello v2")
	et := time.Now().UTC()
	edited.EditedAt = &et
	if err := repo.WriteEdit(ctx, edited); err != nil {
		t.Fatal(err)
	}

	// Delete → 事务化状态机
	ref := testMsg(1, "").Ref
	if err := repo.WriteDelete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	// 重复 Delete → 幂等
	if err := repo.WriteDelete(ctx, ref); err != nil {
		t.Fatal(err)
	}

	var row struct {
		CurrentRevision int        `db:"current_revision"`
		EmbeddingState  string     `db:"embedding_state"`
		DeletedAt       *time.Time `db:"deleted_at"`
		Text            string     `db:"text"`
	}
	if err := db.GetContext(ctx, &row, `
		SELECT current_revision, embedding_state, deleted_at, text FROM messages
		WHERE chat_type='channel' AND chat_id=100200 AND message_id=1`); err != nil {
		t.Fatal(err)
	}
	if row.CurrentRevision != 2 {
		t.Errorf("current_revision = %d（期望 2：create→edit→delete）", row.CurrentRevision)
	}
	if row.EmbeddingState != "delete_pending" {
		t.Errorf("embedding_state = %s（期望 delete_pending）", row.EmbeddingState)
	}
	if row.DeletedAt == nil {
		t.Error("deleted_at 应已置位")
	}
	if row.Text != "hello v2" {
		t.Errorf("text = %s（期望编辑后版本）", row.Text)
	}

	// revisions 事件序列：create(0) → edit(1) → delete(2)
	type revRow struct {
		Revision  int    `db:"revision"`
		EventType string `db:"event_type"`
	}
	var revs []revRow
	if err := db.SelectContext(ctx, &revs, `
		SELECT revision, event_type FROM message_revisions mr
		JOIN messages m ON mr.message_id = m.id
		WHERE m.chat_type='channel' AND m.chat_id=100200 AND m.message_id=1
		ORDER BY revision`); err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 {
		t.Fatalf("revisions 应 3 行，得到 %d", len(revs))
	}
	wantEvents := []string{"create", "edit", "delete"}
	for i, r := range revs {
		if r.Revision != i || r.EventType != wantEvents[i] {
			t.Errorf("rev[%d] = {rev:%d, %s}（期望 {rev:%d, %s}）", i, r.Revision, r.EventType, i, wantEvents[i])
		}
	}

	// 未见过的消息 Edit → 按 New 入库
	if err := repo.WriteEdit(ctx, testMsg(2, "never seen")); err != nil {
		t.Fatalf("未知消息 WriteEdit 应按 New 入库: %v", err)
	}
	// 未见过的消息 Delete → 忽略
	if err := repo.WriteDelete(ctx, testMsg(999, "").Ref); err != nil {
		t.Fatalf("未知消息 WriteDelete 应忽略: %v", err)
	}
}
