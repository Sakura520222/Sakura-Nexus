package config

import "testing"

func TestForwardingSettingsMediaMaxSizeDefaults(t *testing.T) {
	f := defaultForwarding()
	if f.MediaMaxSizeMB != 2048 {
		t.Errorf("media_max_size_mb 默认应为 2048（2GB），实际 %d", f.MediaMaxSizeMB)
	}
	if err := f.Validate(); err != nil {
		t.Errorf("默认值应通过校验: %v", err)
	}
	f.MediaMaxSizeMB = -1
	if err := f.Validate(); err == nil {
		t.Error("负值应被拒绝")
	}
	f.MediaMaxSizeMB = 0
	if err := f.Validate(); err == nil {
		t.Error("0 应被拒绝（必须为正，防止意外无限制）")
	}
}
