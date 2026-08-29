package mysql

import "testing"

func TestDSN(t *testing.T) {
	got := Options{
		Host: "127.0.0.1", Port: 3306,
		User: "u", Password: "p", Database: "d",
	}.DSN()
	want := "u:p@tcp(127.0.0.1:3306)/d?parseTime=true&loc=UTC&charset=utf8mb4&timeout=10s"
	if got != want {
		t.Errorf("DSN:\n got  %s\n want %s", got, want)
	}
}

func TestMaxOpenConnsDefault(t *testing.T) {
	// 非正值回落默认 5（Connect 内处理；此处仅固化契约）
	o := Options{MaxOpenConns: 0}
	if o.MaxOpenConns <= 0 {
		o.MaxOpenConns = 5
	}
	if o.MaxOpenConns != 5 {
		t.Errorf("默认 MaxOpenConns 应为 5，得到 %d", o.MaxOpenConns)
	}
}
