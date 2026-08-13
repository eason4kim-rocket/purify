package searchindex

import (
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/klauspost/compress/zstd"
)

// leadRunes is the plain-text prefix kept beside the compressed body. Snippets
// are cut from it, so it never needs decompressing on the query path.
const leadRunes = 400

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder
	encoderErr  error

	decoderOnce sync.Once
	decoder     *zstd.Decoder
	decoderErr  error
)

// compressBody stores page text at roughly a third of its size. The index is
// contentless, so this copy is what makes a tokenizer change rebuildable
// without re-crawling.
func compressBody(body string) ([]byte, error) {
	if body == "" {
		return nil, nil
	}
	encoderOnce.Do(func() {
		encoder, encoderErr = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	})
	if encoderErr != nil {
		return nil, fmt.Errorf("searchindex: compressor: %w", encoderErr)
	}
	return encoder.EncodeAll([]byte(body), nil), nil
}

func decompressBody(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	decoderOnce.Do(func() {
		decoder, decoderErr = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	})
	if decoderErr != nil {
		return "", fmt.Errorf("searchindex: decompressor: %w", decoderErr)
	}
	plain, err := decoder.DecodeAll(blob, nil)
	if err != nil {
		return "", fmt.Errorf("searchindex: decompress body: %w", err)
	}
	return string(plain), nil
}

// buildLead collapses whitespace and keeps the opening runes of a page so the
// query path can show a fragment without touching the compressed body.
func buildLead(title, body string) string {
	text := strings.TrimSpace(body)
	if text == "" {
		text = strings.TrimSpace(title)
	}
	if text == "" {
		return ""
	}
	fields := strings.FieldsFunc(text, unicode.IsSpace)
	joined := strings.Join(fields, " ")
	runes := []rune(joined)
	if len(runes) <= leadRunes {
		return joined
	}
	return string(runes[:leadRunes])
}
