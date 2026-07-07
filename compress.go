package logrotate

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
)

// tmpSuffix marks in-progress compression output. Completed archives are
// renamed into place so a crash never leaves a truncated archive under the
// final name.
const tmpSuffix = ".tmp"

// Compressor compresses rotated backups. Implementations are invoked from a
// single background goroutine, one file at a time.
//
// The interface is deliberately stream-based so third-party algorithms plug
// in without this package depending on them; see the package example for a
// zstd adapter.
type Compressor interface {
	// Compress reads src to EOF and writes the compressed form to dst.
	Compress(dst io.Writer, src io.Reader) error
	// Extension is the suffix appended to compressed backups, e.g. ".gz".
	// It must start with a dot.
	Extension() string
}

// GzipCompressor implements Compressor with compress/gzip.
type GzipCompressor struct {
	// Level is a compress/gzip compression level. The zero value selects
	// gzip.DefaultCompression.
	Level int
}

func (g GzipCompressor) Compress(dst io.Writer, src io.Reader) error {
	level := g.Level
	if level == 0 {
		level = gzip.DefaultCompression
	}
	zw, err := gzip.NewWriterLevel(dst, level)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, src); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func (GzipCompressor) Extension() string { return ".gz" }

// compressFile compresses src into dst and removes src, returning the size of
// dst. The archive is staged at dst+".tmp", synced, and renamed into place. A
// pre-existing non-empty dst means an earlier pass was interrupted after the
// rename; src is then simply removed.
func (w *Writer) compressFile(src, dst string) (int64, error) {
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		if err := os.Remove(src); err != nil {
			return 0, fmt.Errorf("logrotate: remove backup after compression: %w", err)
		}
		return info.Size(), nil
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return 0, fmt.Errorf("logrotate: stat backup: %w", err)
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, fmt.Errorf("logrotate: open backup: %w", err)
	}
	defer func() { _ = in.Close() }()

	tmp := dst + tmpSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, srcInfo.Mode().Perm())
	if err != nil {
		return 0, fmt.Errorf("logrotate: create archive: %w", err)
	}
	discard := func(err error) (int64, error) {
		_ = out.Close()
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := out.Chmod(srcInfo.Mode().Perm()); err != nil {
		return discard(fmt.Errorf("logrotate: set archive mode: %w", err))
	}
	if err := w.cfg.compressor.Compress(out, in); err != nil {
		return discard(fmt.Errorf("logrotate: compress backup: %w", err))
	}
	if err := out.Sync(); err != nil {
		return discard(fmt.Errorf("logrotate: sync archive: %w", err))
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logrotate: close archive: %w", err)
	}
	if err := chown(tmp, srcInfo); err != nil {
		w.cfg.reportError(fmt.Errorf("logrotate: preserve archive owner: %w", err))
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logrotate: publish archive: %w", err)
	}

	_ = in.Close() // Windows cannot remove an open file
	if err := os.Remove(src); err != nil {
		return 0, fmt.Errorf("logrotate: remove backup after compression: %w", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, nil // archive is in place; size is best-effort
	}
	return info.Size(), nil
}
