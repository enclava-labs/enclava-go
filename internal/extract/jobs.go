package extract

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path/filepath"
	"strings"

	"enclava-go/internal/jobs"
)

// JobHandler dispatches durable extraction and vision operations through the
// same fenced attempt and authorized inference executor.
func (s *Service) JobHandler() jobs.HandlerFunc {
	return func(ctx context.Context, claim *jobs.Claim, input jobs.InputFunc) (string, error) {
		return s.attempt(ctx, claim, input)
	}
}

func copyUpload(ctx context.Context, dst io.Writer, src io.Reader, filename string) error {
	src = contextReader{ctx: ctx, reader: src}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
		_, err := io.Copy(dst, src)
		return err
	}
	var prefix bytes.Buffer
	cfg, format, err := image.DecodeConfig(io.TeeReader(src, &prefix))
	if err != nil {
		return fmt.Errorf("invalid image: %w", err)
	}
	if (format == "png" && ext != ".png") || (format == "jpeg" && ext != ".jpg" && ext != ".jpeg") {
		return fmt.Errorf("image content does not match %s", ext)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxImagePixels/cfg.Height {
		return ErrDocumentTooLarge
	}
	if _, err := io.Copy(dst, &prefix); err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
