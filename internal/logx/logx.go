// Package logx 提供一個「彩色等級」的 slog handler：沿用官方 TextHandler 的 key=value
// 輸出格式（含 quoting），僅把 level 欄位依等級上色以利終端閱讀。
//
// 非終端輸出（被導向檔案 / 管線）或設了 NO_COLOR 環境變數時自動不上色，
// 避免 ANSI 跳脫碼污染日誌檔。刻意零外部相依。
package logx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
)

// ANSI 色碼：僅用於 level 欄位。reset 還原預設色。
const reset = "\x1b[0m"

var levelColors = map[slog.Level]string{
	slog.LevelDebug: "\x1b[90m", // 灰
	slog.LevelInfo:  "\x1b[32m", // 綠
	slog.LevelWarn:  "\x1b[33m", // 黃
	slog.LevelError: "\x1b[31m", // 紅
}

// New 依輸出是否為終端，回傳彩色（TTY）或原生（非 TTY / NO_COLOR）的 TextHandler。
func New(w *os.File, opts *slog.HandlerOptions) slog.Handler {
	if colorEnabled(w) {
		return newColorHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// colorEnabled 判斷是否該上色：未設 NO_COLOR，且輸出為字元裝置（終端機）。
func colorEnabled(w *os.File) bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	fi, err := w.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// colorHandler 包裝一個寫入內部緩衝的 TextHandler；每次 Handle 後把該行的 level 欄位上色再輸出。
// 沿用 TextHandler 產生整行，故格式與 quoting 與原生完全一致，只多了顏色。
type colorHandler struct {
	out   io.Writer
	mu    *sync.Mutex   // 與衍生 handler（WithAttrs/WithGroup）共用，序列化對 buf 與 out 的存取
	buf   *bytes.Buffer // 與衍生 handler 共用的暫存緩衝
	inner slog.Handler  // 寫入 buf 的 TextHandler
}

func newColorHandler(out io.Writer, opts *slog.HandlerOptions) *colorHandler {
	buf := &bytes.Buffer{}
	return &colorHandler{
		out:   out,
		mu:    &sync.Mutex{},
		buf:   buf,
		inner: slog.NewTextHandler(buf, opts),
	}
}

func (h *colorHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *colorHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Reset()
	if err := h.inner.Handle(ctx, r); err != nil {
		return err
	}
	_, err := h.out.Write(colorizeLevel(h.buf.Bytes(), r.Level))
	return err
}

// WithAttrs / WithGroup 回傳共用同一 out/mu/buf 的衍生 handler（inner 帶上新屬性 / 群組）。
func (h *colorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &colorHandler{out: h.out, mu: h.mu, buf: h.buf, inner: h.inner.WithAttrs(attrs)}
}

func (h *colorHandler) WithGroup(name string) slog.Handler {
	return &colorHandler{out: h.out, mu: h.mu, buf: h.buf, inner: h.inner.WithGroup(name)}
}

// colorizeLevel 把行內第一個 "level=<等級>" 的等級值包上顏色；未知等級或找不到則原樣回傳。
func colorizeLevel(line []byte, level slog.Level) []byte {
	color, ok := levelColors[level]
	if !ok {
		return line
	}
	token := []byte("level=" + level.String())
	i := bytes.Index(line, token)
	if i < 0 {
		return line
	}
	var b bytes.Buffer
	b.Grow(len(line) + len(color) + len(reset))
	b.Write(line[:i])
	b.WriteString("level=" + color + level.String() + reset)
	b.Write(line[i+len(token):])
	return b.Bytes()
}
