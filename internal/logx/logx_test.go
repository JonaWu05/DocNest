package logx

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestColorizeLevel(t *testing.T) {
	line := []byte("time=2026-01-02T15:04:05Z level=WARN msg=登入失敗 user=boss\n")
	out := string(colorizeLevel(line, slog.LevelWarn))

	// level 值被黃色包住，其餘內容原樣保留
	if !strings.Contains(out, "level="+levelColors[slog.LevelWarn]+"WARN"+reset) {
		t.Errorf("WARN 應被上色，得到：%q", out)
	}
	if !strings.Contains(out, "msg=登入失敗 user=boss") {
		t.Errorf("其餘欄位應原樣保留，得到：%q", out)
	}
}

func TestColorizeUnknownLevel(t *testing.T) {
	line := []byte("time=x level=INFO+2 msg=y\n")
	if got := colorizeLevel(line, slog.LevelInfo+2); !bytes.Equal(got, line) {
		t.Errorf("未知等級應原樣回傳，得到：%q", got)
	}
}

func TestColorDisabledByNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if colorEnabled(os.Stderr) {
		t.Error("設了 NO_COLOR 時不應上色")
	}
}

// New 在非終端（此處以 bytes.Buffer 無法傳入 *os.File，改用暫存檔驗證）輸出應為原生無色格式。
func TestNewOnFileHasNoColor(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "log-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	logger := slog.New(New(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Warn("測試", "k", "v")

	data, _ := os.ReadFile(f.Name())
	if bytes.Contains(data, []byte("\x1b[")) {
		t.Errorf("寫入檔案（非終端）不應含 ANSI 色碼，得到：%q", string(data))
	}
	if !bytes.Contains(data, []byte("level=WARN")) {
		t.Errorf("應為原生無色格式，得到：%q", string(data))
	}
}
