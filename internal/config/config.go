// Package config loads runtime configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Addr           string // listen address, default :4318 (OTLP/HTTP standard port)
	ClickHouseURL  string // e.g. http://localhost:8123
	ClickHouseDB   string // default "lumen"
	ClickHouseUser string
	ClickHousePass string
	RetentionDays  int               // TTL applied to all tables
	APIKeys        map[string]string // api key -> tenant. Empty = dev mode (single tenant, no auth)
	PublicURL      string            // externally reachable base URL, baked into agent install scripts
	DistDir        string            // directory holding agent binaries served at /download
	DataDir        string            // where users, API keys and the session secret are stored (auth.json)
	DevMode        bool              // LUMEN_DEV_MODE=true: no authentication at all, single tenant "default"
	AdminUser      string            // optional bootstrap admin, created at startup if it does not exist
	AdminPassword  string
	AdminTenant    string
	// Elasticsearch (or OpenSearch) holds users, API keys and dashboards. Empty URL = JSON file in DataDir.
	SecretKey                                     string // encrypts stored instance credentials; falls back to the session secret when empty
	BackupDir                                     string // where daily exports of expiring data go ("" disables backups)
	BackupRetentionDays                           int    // how long backups are kept; 0 = forever
	ESURL, ESUser, ESPassword, ESAPIKey, ESPrefix string
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func Load() Config {
	days, err := strconv.Atoi(env("LUMEN_RETENTION_DAYS", "30"))
	if err != nil || days < 1 {
		days = 30
	}
	bdays, err := strconv.Atoi(env("LUMEN_BACKUP_RETENTION_DAYS", "365"))
	if err != nil || bdays < 0 {
		bdays = 365
	}
	c := Config{
		BackupDir:           env("LUMEN_BACKUP_DIR", "/backup"),
		BackupRetentionDays: bdays,
		Addr:                env("LUMEN_ADDR", ":4318"),
		ClickHouseURL:       env("LUMEN_CLICKHOUSE_URL", "http://localhost:8123"),
		ClickHouseDB:        env("LUMEN_CLICKHOUSE_DB", "lumen"),
		ClickHouseUser:      env("LUMEN_CLICKHOUSE_USER", "default"),
		ClickHousePass:      os.Getenv("LUMEN_CLICKHOUSE_PASSWORD"),
		RetentionDays:       days,
		APIKeys:             map[string]string{},
		PublicURL:           strings.TrimRight(os.Getenv("LUMEN_PUBLIC_URL"), "/"),
		DistDir:             env("LUMEN_DIST_DIR", "/dist"),
		DataDir:             env("LUMEN_DATA_DIR", "/data"),
		DevMode:             os.Getenv("LUMEN_DEV_MODE") == "true",
		AdminUser:           os.Getenv("LUMEN_ADMIN_USER"),
		AdminPassword:       os.Getenv("LUMEN_ADMIN_PASSWORD"),
		AdminTenant:         env("LUMEN_ADMIN_TENANT", "main"),
		SecretKey:           os.Getenv("LUMEN_SECRET_KEY"),
		ESURL:               os.Getenv("LUMEN_ELASTICSEARCH_URL"),
		ESUser:              os.Getenv("LUMEN_ELASTICSEARCH_USER"),
		ESPassword:          os.Getenv("LUMEN_ELASTICSEARCH_PASSWORD"),
		ESAPIKey:            os.Getenv("LUMEN_ELASTICSEARCH_API_KEY"),
		ESPrefix:            env("LUMEN_ELASTICSEARCH_PREFIX", "lumen"),
	}
	// LUMEN_API_KEYS="key1:tenantA,key2:tenantB"
	for _, pair := range strings.Split(os.Getenv("LUMEN_API_KEYS"), ",") {
		k, t, ok := strings.Cut(strings.TrimSpace(pair), ":")
		if ok && k != "" && t != "" {
			c.APIKeys[k] = t
		}
	}
	return c
}
