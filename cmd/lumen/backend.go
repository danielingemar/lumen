package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/config"
	"github.com/danielingemar/lumen/internal/docstore"
)

// openBackend returns the document backend for users, keys and dashboards: Elasticsearch when
// LUMEN_ELASTICSEARCH_URL is set (waiting up to 2 minutes for it to come up), otherwise a JSON file.
func openBackend(cfg config.Config, log *slog.Logger) (docstore.Backend, time.Duration, error) {
	if cfg.ESURL == "" {
		f, err := docstore.OpenFile(cfg.DataDir)
		return f, 0, err
	}
	es := docstore.NewES(docstore.ESConfig{URL: cfg.ESURL, User: cfg.ESUser, Password: cfg.ESPassword, APIKey: cfg.ESAPIKey, Prefix: cfg.ESPrefix})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := es.Ping(ctx)
		if err == nil {
			err = es.EnsureIndices(ctx)
		}
		cancel()
		if err == nil {
			return es, 5 * time.Second, nil
		}
		if time.Now().After(deadline) {
			return nil, 0, fmt.Errorf("elasticsearch not usable after 2 minutes: %w", err)
		}
		if log != nil {
			log.Warn("waiting for elasticsearch", "err", err.Error())
		}
		time.Sleep(3 * time.Second)
	}
}

// openStore opens users/keys storage and imports an old auth.json if one is lying around.
func openStore(cfg config.Config, log *slog.Logger) (*auth.Store, docstore.Backend, error) {
	b, ttl, err := openBackend(cfg, log)
	if err != nil {
		return nil, nil, err
	}
	s, err := auth.Open(b, ttl)
	if err != nil {
		return nil, nil, err
	}
	if n, err := auth.ImportLegacy(s, cfg.DataDir); err != nil {
		return nil, nil, fmt.Errorf("importing old auth.json: %w", err)
	} else if n > 0 && log != nil {
		log.Info("imported users and keys from the old auth.json", "records", n)
	}
	return s, b, nil
}
