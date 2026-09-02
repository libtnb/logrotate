// Package logrotate provides a rotating log file writer: an io.WriteCloser
// that appends to one file at a fixed path and moves it aside as a
// timestamped backup when a size or time limit is reached. Compression and
// retention of backups run on a maintenance goroutine so that writes never
// wait for them; their failures are retained and returned by Close or
// Shutdown.
//
// # Rotation triggers
//
// Rotation happens inside Write, under the same lock as the write itself.
// With WithMaxSize (100 MB by default) a record that would push the file
// past the limit goes into a fresh file, and a record that fills the file is
// followed by an immediate rotation, so the file never lingers over the cap.
// A record is never split across files; one larger than the limit is written
// in full and rotated out right after it.
//
// WithRotateEvery and WithRotateAt add wall-clock boundaries in the
// WithLocation zone, UTC by default. Interval boundaries are anchored at
// midnight and the last interval of a day is cut at the next midnight, so
// daily rotation stays on the calendar day across daylight-saving changes.
// Boundaries are evaluated on writes only: the first write at or after a
// boundary rotates before it is written, an idle period produces no empty
// backup, and the backup is stamped with the boundary that ended its period
// rather than with the time of the write. New applies the same rule to an
// existing file whose modification time falls in a period that has already
// ended.
//
// Rotate forces a rotation regardless of size or schedule, for example on
// SIGHUP. Reopen only reopens the path, for deployments where an external
// tool such as logrotate(8) moves the file.
//
// # Backup naming and retention
//
// The backup of /var/log/app.log rotated at 10:30:00 UTC on 2026-03-14 is
// /var/log/app-2026-03-14T10-30-00.000.log; WithBackupTimeFormat changes the
// timestamp layout. When that timestamp is already in use, because the layout
// is coarser than the rotation cadence or two rotations fell into one tick, a
// sequence suffix is inserted before the extension: app-<stamp>.1.log,
// app-<stamp>.2.log and so on. Sequences within a timestamp only grow, one
// past the highest found on disk, even after retention has removed earlier
// ones, so the newest backup always sorts last and no existing backup is ever
// overwritten. Files in the directory that do not match this pattern are
// never removed.
//
// Each maintenance pass, requested by New and by every rotation that produced
// a backup, orders the backups newest first by timestamp and then by
// sequence and applies the limits in this order: WithMaxBackups and
// WithMaxAge, which is measured from the backup timestamp against the Clock,
// then compression, then WithMaxTotalSize, which therefore counts archives at
// their compressed size. Zero disables a limit; the retention limits all
// default to zero, so by default every backup is kept.
//
// # Compression
//
// WithCompress selects gzip and WithCompressor any Compressor implementation.
// Backups are compressed one at a time on the maintenance goroutine. An
// archive is staged under a ".tmp" suffix, synced and renamed into place
// before the plain backup is removed, so an interruption leaves the plain
// file, the finished archive or both, never a truncated archive under its
// final name; the next pass completes the job and removes orphaned temporary
// files. A backup whose compression fails stays in place uncompressed, counts
// at full size against WithMaxTotalSize and is retried on the next pass.
//
// # Shutdown and errors
//
// Close and Shutdown close the active file, after which Write, Sync, Rotate
// and Reopen return ErrClosed, and wait for the maintenance goroutine to
// finish its pending pass. Shutdown stops waiting when its context ends and
// cancels the compression in flight. Both return the file close error joined
// with a bounded record of the maintenance errors reported during the
// Writer's life: the first one, the most recent ones and the number omitted
// in between. WithErrorHandler receives the same errors as they happen, on a
// dedicated goroutine, for logging or metrics; it is advisory, and the result
// of Close or Shutdown remains authoritative.
//
// # Concurrency
//
// A Writer is safe for concurrent use by multiple goroutines; writes are
// serialised and each call to Write is one record. Maintenance never holds
// the write lock, so compression and retention do not stall logging. A
// Writer assumes it is the only process owning the active path: it takes no
// file lock, so two processes rotating the same path can overwrite each
// other's backups.
package logrotate
