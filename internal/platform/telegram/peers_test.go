package telegram

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// fakePeerStorage 是 contrib storage.PeerStorage 的内存实现（表驱动测试用）。
type fakePeerStorage struct {
	peers map[storage.PeerKey]storage.Peer
}

func newFakePeerStorage() *fakePeerStorage {
	return &fakePeerStorage{peers: map[storage.PeerKey]storage.Peer{}}
}

func (f *fakePeerStorage) Add(_ context.Context, value storage.Peer) error {
	key := storage.KeyFromPeer(value)
	f.peers[key] = value
	return nil
}

func (f *fakePeerStorage) Find(_ context.Context, key storage.PeerKey) (storage.Peer, error) {
	p, ok := f.peers[key]
	if !ok {
		return storage.Peer{}, storage.ErrPeerNotFound
	}
	return p, nil
}

func (f *fakePeerStorage) Assign(_ context.Context, _ string, value storage.Peer) error {
	return f.Add(context.Background(), value)
}

func (f *fakePeerStorage) Resolve(_ context.Context, _ string) (storage.Peer, error) {
	return storage.Peer{}, storage.ErrPeerNotFound
}

func (f *fakePeerStorage) Iterate(_ context.Context) (storage.PeerIterator, error) {
	return nil, errors.New("not implemented")
}

// PeerBook：Bot 账号 PeerResolver，经 telegram_peers 表解析 access_hash。
func TestPeerBookResolvesFromStorage(t *testing.T) {
	fs := newFakePeerStorage()
	_ = fs.Add(context.Background(), storage.Peer{Key: dialogs.DialogKey{Kind: dialogs.User, ID: 7}, User: &tg.User{ID: 7, AccessHash: 111}})
	_ = fs.Add(context.Background(), storage.Peer{Key: dialogs.DialogKey{Kind: dialogs.Chat, ID: 8}, Chat: &tg.Chat{ID: 8}})
	_ = fs.Add(context.Background(), storage.Peer{Key: dialogs.DialogKey{Kind: dialogs.Channel, ID: 9}, Channel: &tg.Channel{ID: 9, AccessHash: 222}})

	book := NewPeerBook(fs, nil)
	cases := []struct {
		ref  domain.ChatRef
		want tg.InputPeerClass
	}{
		{domain.NewChatRef(domain.PeerUser, 7), &tg.InputPeerUser{UserID: 7, AccessHash: 111}},
		{domain.NewChatRef(domain.PeerChat, 8), &tg.InputPeerChat{ChatID: 8}},
		{domain.NewChatRef(domain.PeerChannel, 9), &tg.InputPeerChannel{ChannelID: 9, AccessHash: 222}},
	}
	for _, tc := range cases {
		got, err := book.InputPeer(context.Background(), tc.ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if got.(interface{ String() string }) == nil {
			t.Fatalf("%s: 空 peer", tc.ref)
		}
		switch want := tc.want.(type) {
		case *tg.InputPeerUser:
			if u, ok := got.(*tg.InputPeerUser); !ok || u.UserID != want.UserID || u.AccessHash != want.AccessHash {
				t.Errorf("%s: got %#v", tc.ref, got)
			}
		case *tg.InputPeerChat:
			if c, ok := got.(*tg.InputPeerChat); !ok || c.ChatID != want.ChatID {
				t.Errorf("%s: got %#v", tc.ref, got)
			}
		case *tg.InputPeerChannel:
			if c, ok := got.(*tg.InputPeerChannel); !ok || c.ChannelID != want.ChannelID || c.AccessHash != want.AccessHash {
				t.Errorf("%s: got %#v", tc.ref, got)
			}
		}
	}
}

func TestPeerBookMissErrors(t *testing.T) {
	book := NewPeerBook(newFakePeerStorage(), nil)
	if _, err := book.InputPeer(context.Background(), domain.NewChatRef(domain.PeerChannel, 42)); err == nil {
		t.Fatal("未知 peer 应报错（调用方处置，不静默零 hash）")
	}
}

// IsPermanentSendError：完整 tgerr→permanent 映射（T5.1 备忘；冒烟侧最小版扩展）。
func TestIsPermanentSendError(t *testing.T) {
	permanent := []string{
		"CHAT_WRITE_FORBIDDEN", "USER_BANNED_IN_CHANNEL", "CHAT_ADMIN_REQUIRED",
		"CHANNEL_PRIVATE", "CHANNEL_INVALID", "CHAT_ID_INVALID", "PEER_ID_INVALID",
		"MESSAGE_ID_INVALID", "MESSAGE_EMPTY", "MEDIA_INVALID", "MEDIA_EMPTY",
		"PHOTO_INVALID", "DOCUMENT_INVALID", "USER_DEACTIVATED_BAN", "INPUT_USER_DEACTIVATED",
	}
	for _, code := range permanent {
		err := &tgerr.Error{Code: 400, Type: code, Message: code}
		if !IsPermanentSendError(err) {
			t.Errorf("%s 应判 permanent", code)
		}
		wrapped := errors.Join(errors.New("发送段 1/2"), err)
		if !IsPermanentSendError(wrapped) {
			t.Errorf("%s（wrapped）应判 permanent", code)
		}
	}
	transient := []error{
		errors.New("dial tcp: connection refused"),
		tgerr.New(420, "FLOOD_WAIT_30"),
		&tgerr.Error{Code: 500, Type: "INTERNAL", Message: "internal"},
		nil,
	}
	for _, err := range transient {
		if IsPermanentSendError(err) {
			t.Errorf("%v 应判 transient", err)
		}
	}
}
