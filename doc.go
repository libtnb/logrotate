// Package logrotate provides a concurrent rotating io.WriteCloser. Compression
// and retention run asynchronously; Close and Shutdown return retained errors.
package logrotate
