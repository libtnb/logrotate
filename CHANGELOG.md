# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Release notes for v0.1.4 and earlier live on [GitHub Releases](https://github.com/libtnb/logrotate/releases).

## [Unreleased]

## [v0.1.5] - 2026-09-02

### Fixed

- Backup sequence suffixes are now monotonic within a timestamp (one past the highest sequence on disk, found with a single directory read per new timestamp), so retention keeps the newest backups even when the backup time layout is coarser than the rotation cadence, and a rotation no longer probes every sequence with `Lstat` under the write lock.

[Unreleased]: https://github.com/libtnb/logrotate/compare/v0.1.5...HEAD
[v0.1.5]: https://github.com/libtnb/logrotate/compare/v0.1.4...v0.1.5
