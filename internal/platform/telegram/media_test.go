package telegram

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

func TestLimitedWriterUnderAndExactCap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lw := &limitedWriter{w: f, remaining: 10}
	if n, err := lw.Write([]byte("12345")); err != nil || n != 5 {
		t.Fatalf("写入 5 字节应成功: n=%d err=%v", n, err)
	}
	if n, err := lw.Write([]byte("67890")); err != nil || n != 5 {
		t.Fatalf("恰好到上限应成功: n=%d err=%v", n, err)
	}
	if lw.remaining != 0 {
		t.Errorf("剩余配额应为 0: %d", lw.remaining)
	}
}

func TestLimitedWriterOverCapErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	f, _ := os.Create(p)
	defer func() { _ = f.Close() }()
	lw := &limitedWriter{w: f, remaining: 4}
	if _, err := lw.Write([]byte("123")); err != nil {
		t.Fatalf("前 3 字节应成功: %v", err)
	}
	_, err := lw.Write([]byte("456"))
	if !errors.Is(err, domain.ErrMediaTooLarge) {
		t.Fatalf("越限应返回 domain.ErrMediaTooLarge: %v", err)
	}
}

func testPhoto() *tg.Photo {
	return &tg.Photo{
		ID:            1001,
		AccessHash:    0xabc,
		FileReference: []byte("ref-v1"),
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "i", W: 80, H: 80, Size: 4096},
			&tg.PhotoSize{Type: "x", W: 800, H: 600, Size: 102400},
			&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 960, Sizes: []int{20480, 307200}},
		},
	}
}

func TestFreshMediaRefPhotoRealSize(t *testing.T) {
	ref, ok := freshMediaRef(&tg.MessageMediaPhoto{Photo: testPhoto()})
	if !ok {
		t.Fatal("photo 应产出 MediaRef")
	}
	if ref.Type != "photo" {
		t.Errorf("类型应为 photo: %s", ref.Type)
	}
	if ref.Size != 307200 {
		t.Errorf("photo 尺寸应取最大 PhotoSizeProgressive 末端（非 file_reference 占位）: %d", ref.Size)
	}
	if ref.Width != 1280 || ref.Height != 960 {
		t.Errorf("尺寸元数据应来自最大档: %dx%d", ref.Width, ref.Height)
	}
}

func TestFreshMediaRefDocumentFullMeta(t *testing.T) {
	doc := &tg.Document{
		ID:            2002,
		AccessHash:    0xdef,
		FileReference: []byte("ref-d1"),
		MimeType:      "video/mp4",
		Size:          999999,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeVideo{W: 1920, H: 1080, Duration: 62},
			&tg.DocumentAttributeFilename{FileName: "cat.mp4"},
		},
	}
	ref, ok := freshMediaRef(&tg.MessageMediaDocument{Document: doc})
	if !ok {
		t.Fatal("document 应产出 MediaRef")
	}
	if ref.Type != "video" || ref.MimeType != "video/mp4" || ref.Size != 999999 {
		t.Errorf("基础元数据不符: %+v", ref)
	}
	if ref.FileName != "cat.mp4" {
		t.Errorf("文件名应从 attributes 捕获: %q", ref.FileName)
	}
	if ref.Width != 1920 || ref.Height != 1080 || ref.Duration != 62 {
		t.Errorf("尺寸/时长不符: %+v", ref)
	}
}

func TestDownloadLocationPhotoAndDocument(t *testing.T) {
	ph := testPhoto()
	_, loc, ok := mediaRefAndLocation(&tg.MessageMediaPhoto{Photo: ph})
	if !ok {
		t.Fatal("photo 应产出 location")
	}
	pl, isPhoto := loc.(*tg.InputPhotoFileLocation)
	if !isPhoto {
		t.Fatalf("photo location 类型不符: %T", loc)
	}
	if pl.ID != 1001 || pl.AccessHash != 0xabc || string(pl.FileReference) != "ref-v1" {
		t.Errorf("photo location 字段不符: %+v", pl)
	}
	if pl.ThumbSize != "y" {
		t.Errorf("应选最大档 thumb size: %q", pl.ThumbSize)
	}

	doc := &tg.Document{ID: 2002, AccessHash: 0xdef, FileReference: []byte("ref-d1"), MimeType: "video/mp4", Size: 5}
	_, loc2, ok2 := mediaRefAndLocation(&tg.MessageMediaDocument{Document: doc})
	if !ok2 {
		t.Fatal("document 应产出 location")
	}
	dl, isDoc := loc2.(*tg.InputDocumentFileLocation)
	if !isDoc {
		t.Fatalf("document location 类型不符: %T", loc2)
	}
	if dl.ID != 2002 || dl.AccessHash != 0xdef {
		t.Errorf("document location 字段不符: %+v", dl)
	}
}

func TestExtractMessageFromMessagesClass(t *testing.T) {
	msgs := []tg.MessageClass{
		&tg.Message{ID: 1, Message: "a"},
		&tg.Message{ID: 2, Message: "b"},
	}
	res, ok := extractMessage(&tg.MessagesMessagesSlice{Messages: msgs, Count: 2}, 2)
	if !ok || res.ID != 2 {
		t.Fatalf("应从 MessagesSlice 提取 id=2: ok=%v id=%v", ok, res.ID)
	}
	res2, ok2 := extractMessage(&tg.MessagesChannelMessages{Messages: msgs, Count: 2}, 1)
	if !ok2 || res2.ID != 1 {
		t.Fatalf("应从 ChannelMessages 提取 id=1: ok=%v", ok2)
	}
	if _, ok3 := extractMessage(&tg.MessagesMessagesNotModified{}, 1); ok3 {
		t.Error("NotModified 应提取失败")
	}
	if _, ok4 := extractMessage(&tg.MessagesMessagesSlice{Messages: msgs, Count: 2}, 9); ok4 {
		t.Error("不存在的 id 应提取失败")
	}
}
