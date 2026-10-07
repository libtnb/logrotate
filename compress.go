package logrotate

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
)

// tmpSuffix marks in-progress compression output. Completed archives are
// renamed into place so a crash never leaves a truncated archive under the
// final name.
const tmpSuffix = ".tmp"

// Compressor turns a plain backup into an archive. The Writer calls Compress
// once per backup, serially on the maintenance goroutine, with dst being a
// staging file that is synced and renamed into place when Compress returns
// nil, after which the plain backup is deleted; Compress must therefore
// return an error whenever dst is incomplete. It should return promptly once
// ctx is done, with an error wrapping context.Cause(ctx): such an error ends
// the maintenance pass quietly, whereas any other error is retained as a
// compression failure and reported to the error handler. In both cases the
// plain backup is kept and retried on a later pass. A Compressor that ignores
// ctx makes a Shutdown deadline abandon the wait while the maintenance
// goroutine keeps running until Compress returns.
type Compressor interface {
	// Compress writes the complete archive of src to dst, honoring ctx as
	// described for the interface.
	Compress(ctx context.Context, dst io.Writer, src io.Reader) error
	// Extension is the suffix appended to archive names, for example ".gz".
	// It must start with a dot, be at least two characters long, differ from
	// ".tmp" and contain no path separator; New rejects other values. It must
	// not change during the life of the Writer, which uses it both to name
	// new archives and to recognise existing ones.
	Extension() string
}

// GzipCompressor is the Compressor behind WithCompress, built on
// compress/gzip. The zero value compresses at gzip.DefaultCompression; set
// Level to trade ratio for speed, for example gzip.BestSpeed on a busy
// service.
type GzipCompressor struct {
	// Level is a compress/gzip compression level. The zero value selects
	// gzip.DefaultCompression, so gzip.NoCompression (0) cannot be requested;
	// a level outside the gzip range makes Compress fail for every backup.
	Level int
}

// Compress gzips src into dst at Level. It checks ctx around every read and
// returns context.Cause(ctx) once ctx is done, leaving dst incomplete, so a
// Shutdown deadline interrupts a large archive promptly. Other errors come
// from gzip, for an invalid Level, or from the underlying reads and writes.
func (g GzipCompressor) Compress(ctx context.Context, dst io.Writer, src io.Reader) error {
	level := g.Level
	if level == 0 {
		level = gzip.DefaultCompression
	}
	zw, err := gzip.NewWriterLevel(dst, level)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, contextReader{ctx: ctx, reader: src}); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// Extension returns ".gz".
func (GzipCompressor) Extension() string { return ".gz" }

// contextReader fails a read once ctx is done, so that an io.Copy driven by a
// compressor stops between chunks instead of running to the end of src.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, context.Cause(r.ctx)
	}
	return n, err
}

// compressFile compresses src into dst and removes src, returning the size of
// dst. The archive is staged at dst+".tmp", synced, and renamed into place. A
// non-empty dst means another pass already published it, possibly from a
// second process sharing the log during a handoff; src is then simply removed,
// and a src that is already gone counts as done too.
func (w *Writer) compressFile(ctx context.Context, src, dst string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
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
	if err := w.cfg.compressor.Compress(ctx, out, in); err != nil {
		return discard(fmt.Errorf("logrotate: compress backup: %w", err))
	}
	if err := out.Sync(); err != nil {
		return discard(fmt.Errorf("logrotate: sync archive: %w", err))
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("logrotate: close archive: %w", err)
	}
	if err := chown(tmp, srcInfo); err != nil && !os.IsNotExist(err) {
		w.reportError(fmt.Errorf("logrotate: preserve archive owner: %w", err))
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		// another process sharing the log may have published this archive first
		if info, statErr := os.Stat(dst); !os.IsNotExist(err) || statErr != nil || info.Size() == 0 {
			return 0, fmt.Errorf("logrotate: publish archive: %w", err)
		}
	}

	_ = in.Close() // Windows cannot remove an open file
	if err := os.Remove(src); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("logrotate: remove backup after compression: %w", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, nil // archive is in place; size is best-effort
	}
	return info.Size(), nil
}
