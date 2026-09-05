package botapi

import (
	"testing"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// Bot API chat_id 三态编码（internal/domain/chat.go 冻结注释：user→+ID、
// chat→-ID、channel→-(1000000000000+ID)），仅在 platform/botapi 出站时构造。
func TestChatIDThreeStateEncoding(t *testing.T) {
	cases := []struct {
		name string
		ref  domain.ChatRef
		want int64
	}{
		{"user 正数直传", domain.NewChatRef(domain.PeerUser, 6826794184), 6826794184},
		{"basic group 取负", domain.NewChatRef(domain.PeerChat, 1097), -1097},
		{"channel/supergroup 加 -100 前缀", domain.NewChatRef(domain.PeerChannel, 1234567890), -1001234567890},
		{"channel 小 ID 同样补前缀", domain.NewChatRef(domain.PeerChannel, 1), -1000000000001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChatID(tc.ref)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != tc.want {
				t.Errorf("编码不符: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestChatIDRejectsInvalidRefs(t *testing.T) {
	cases := []struct {
		name string
		ref  domain.ChatRef
	}{
		{"裸 ID 必须为正", domain.NewChatRef(domain.PeerUser, 0)},
		{"拒绝已编码 ID 防二次编码", domain.NewChatRef(domain.PeerChannel, -1001234567890)},
		{"未知 PeerKind", domain.NewChatRef(domain.PeerKind(9), 5)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ChatID(tc.ref); err == nil {
				t.Fatalf("应报错: %v", tc.ref)
			}
		})
	}
}
