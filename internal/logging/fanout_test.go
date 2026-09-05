package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestFanoutBroadcasts(t *testing.T) {
	var buf bytes.Buffer
	ring := NewRing(8)
	lg := slog.New(NewFanout(slog.NewTextHandler(&buf, nil), ring))

	lg.Info("双路日志", "component", "app", "k", "v")

	if !strings.Contains(buf.String(), "双路日志") {
		t.Errorf("主输出未收到: %s", buf.String())
	}
	snap := ring.Snapshot()
	if len(snap) != 1 || snap[0].Component != "app" {
		t.Errorf("环形缓冲未收到或 component 丢失: %+v", snap)
	}
}
