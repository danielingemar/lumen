package store

import (
	"context"
	"regexp"
)

type archiveKey struct{}

// WithArchive makes read queries on this context use the *_archive tables (data restored from backups).
func WithArchive(ctx context.Context) context.Context {
	return context.WithValue(ctx, archiveKey{}, true)
}

// InArchive reports whether the context asks for archived data.
func InArchive(ctx context.Context) bool { return isArchive(ctx) }

func isArchive(ctx context.Context) bool { v, _ := ctx.Value(archiveKey{}).(bool); return v }

var tableRe = regexp.MustCompile(`\botel_(spans|logs|metrics)\b`)

// archiveSQL points a query at the archive tables. Table names appear only as FROM targets, never in user
// input (which travels as typed parameters), so a textual rewrite is safe.
func archiveSQL(q string) string { return tableRe.ReplaceAllString(q, "otel_${1}_archive") }
