// Package logrotate provides a rotating file writer for logs.
//
// It is a pluggable io.WriteCloser sitting at the bottom of a logging stack —
// pass it to log.SetOutput, slog.NewJSONHandler or zapcore.AddSync — that
// rotates the file it writes to by size, by wall-clock schedule, or on
// demand, and prunes old backups by count, age and total disk usage, with
// optional background compression.
//
//	w, err := logrotate.New("/var/log/myapp/app.log",
//		logrotate.WithMaxSize(64*logrotate.MB),
//		logrotate.WithRotateEvery(24*time.Hour),
//		logrotate.WithMaxBackups(14),
//		logrotate.WithMaxTotalSize(1*logrotate.GB),
//		logrotate.WithCompress(),
//	)
//	if err != nil {
//		// configuration and permission errors surface here, not mid-flight
//	}
//	defer w.Close()
//	log.SetOutput(w)
//
// The active file keeps its configured name at all times; rotation renames it
// to "app-<timestamp>.log" (compressed to "app-<timestamp>.log.gz") in the
// same directory. Time-based rotation is evaluated on write rather than by a
// background timer: an idle process creates no empty files, backups are
// stamped with the boundary that ended their period, and after a restart the
// previous period is recovered from the file's modification time.
//
// logrotate assumes a single process writes to a given file. It has no
// third-party dependencies.
package logrotate
