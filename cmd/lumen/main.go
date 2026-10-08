package main

import (
	"context"
	"github.com/danielingemar/lumen/internal/health"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielingemar/lumen/internal/alerts"
	"github.com/danielingemar/lumen/internal/audit"
	"github.com/danielingemar/lumen/internal/auth"
	"github.com/danielingemar/lumen/internal/backup"
	"github.com/danielingemar/lumen/internal/branding"
	"github.com/danielingemar/lumen/internal/buildinfo"
	"github.com/danielingemar/lumen/internal/config"
	"github.com/danielingemar/lumen/internal/dashboards"
	"github.com/danielingemar/lumen/internal/edition"
	"github.com/danielingemar/lumen/internal/install"
	"github.com/danielingemar/lumen/internal/license"
	"github.com/danielingemar/lumen/internal/metering"
	"github.com/danielingemar/lumen/internal/perm"
	"github.com/danielingemar/lumen/internal/registry"
	"github.com/danielingemar/lumen/internal/secretbox"
	"github.com/danielingemar/lumen/internal/server"
	"github.com/danielingemar/lumen/internal/store"
	"github.com/danielingemar/lumen/internal/tenants"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "users" || os.Args[1] == "keys") {
		os.Exit(runCLI(os.Args[1:]))
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg := config.Load()

	if err := install.ValidatePublicURL(cfg.PublicURL); err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}
	ch := store.NewClickHouse(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := ch.Migrate(ctx); err != nil {
		log.Error("clickhouse migration failed", "err", err)
		os.Exit(1)
	}
	store, backend, err := openStore(cfg, log)
	if err != nil {
		log.Error("cannot open the user/key/dashboard store", "err", err)
		os.Exit(1)
	}
	log.Info("document store ready", "backend", backend.Name())
	if cfg.AdminUser != "" && cfg.AdminPassword != "" {
		if _, exists := store.GetUser(cfg.AdminUser); !exists {
			if err := store.CreateUser(cfg.AdminUser, cfg.AdminTenant, cfg.AdminPassword); err != nil {
				log.Error("could not create the admin user", "err", err)
				os.Exit(1)
			}
			log.Info("created admin user", "user", cfg.AdminUser, "tenant", cfg.AdminTenant)
		}
	}
	switch {
	case cfg.DevMode:
		log.Warn("LUMEN_DEV_MODE=true: NO AUTHENTICATION, single tenant 'default'. Never use this on a network you do not control.")
	case !store.HasCredentials() && len(cfg.APIKeys) == 0:
		log.Warn("no users or API keys exist: nobody can log in. Set LUMEN_ADMIN_USER/LUMEN_ADMIN_PASSWORD, or run: lumen users add NAME --tenant TENANT")
	}
	authn := auth.New(store, cfg.APIKeys, cfg.DevMode)
	secret := cfg.SecretKey
	if secret == "" {
		secret = string(store.Secret())
		log.Warn("LUMEN_SECRET_KEY is not set: stored Nextcloud credentials are encrypted with the session secret instead. Set LUMEN_SECRET_KEY (deploy/gen-env.sh does).")
	}
	box, err := secretbox.New(secret)
	if err != nil {
		log.Error("secret key", "err", err)
		os.Exit(1)
	}

	app := server.New(ch, authn, log).WithAuth(authn).WithDashboards(dashboards.New(backend)).WithRegistry(registry.New(backend, box)).WithBranding(branding.New(backend)).WithInstall(cfg.PublicURL, cfg.DistDir)
	bg, stopBg := context.WithCancel(context.Background())
	defer stopBg()
	// tenants: a record for every tenant that exists (so an installation from before this feature shows up in the console),
	// counting of what each one sends, and the record of who did what
	tsvc := tenants.New(backend)
	known := append(tenants.Discover(backend), cfg.AdminTenant, perm.OperatorTenant)
	for _, t := range cfg.APIKeys {
		known = append(known, t)
	}
	if n := tsvc.Ensure(known); n > 0 {
		log.Info("tenant records made for existing tenants", "count", n)
	}
	ten := &server.Tenancy{Tenants: tsvc, Meter: metering.New(backend), Limiter: metering.NewLimiter(), Audit: audit.New(backend),
		Off: &tenants.Offboarder{Svc: tsvc, Purger: ch, BackupDir: cfg.BackupDir, Log: log}}
	app.WithTenancy(ten)
	go ten.Run(bg, log)
	for _, t := range tsvc.List() { // a removal that was interrupted by a restart carries on
		if t.Status == tenants.Offboarding && t.Offboard != nil && t.Offboard.State == "purging" {
			log.Info("resuming the removal of a tenant", "tenant", t.ID)
			go ten.Off.Run(bg, t.ID)
		}
	}
	// Lumen looks at itself: the disk it writes to (the file system under the data and backup folders is the one under Docker's
	// volumes), Elasticsearch if it is used, and ClickHouse
	src := health.Sources{Paths: map[string]string{"data": cfg.DataDir}, CH: ch}
	if cfg.BackupDir != "" {
		src.Paths["backups"] = cfg.BackupDir
	}
	if es, ok := backend.(interface {
		Health(context.Context) (health.ES, error)
	}); ok {
		src.ES = es
	}
	app.WithHealth(health.New(src, cfg.DiskWarn, cfg.DiskCrit))
	lic := license.NewManager(backend, license.EmbeddedKeys(), cfg.LicenseFile)
	lic.Load()
	app.WithLicense(lic).WithOwnerTenant(cfg.AdminTenant)
	go func() { // a renewal that is dropped in as a file is picked up within a minute
		for t := time.NewTicker(time.Minute); ; {
			select {
			case <-bg.Done():
				return
			case <-t.C:
				lic.Load()
			}
		}
	}()
	log.Info("licence", "state", string(lic.State()), "trusted_keys", len(license.EmbeddedKeys()))
	if cfg.Alerts {
		eng := &alerts.Engine{License: lic, DB: backend, Box: box, Eval: &alerts.Evaluator{Q: ch, St: app, G: app, H: app}, Guard: alerts.Guard{AllowPrivate: cfg.AlertAllowPrivate},
			PublicURL: cfg.PublicURL, Log: log, GroupWait: cfg.AlertGroupWait, RepeatEvery: cfg.AlertRepeat}
		app.WithAlerts(eng)
		go eng.Run(bg)
		log.Info("alerting enabled", "group_wait", cfg.AlertGroupWait.String(), "repeat", cfg.AlertRepeat.String(), "allow_private", cfg.AlertAllowPrivate)
	}
	if cfg.BackupDir != "" {
		bk := &backup.Manager{Dir: cfg.BackupDir, Src: ch, Docs: backend, DataDays: cfg.RetentionDays, KeepDays: cfg.BackupRetentionDays, Log: log}
		app.WithBackups(bk, server.BackupInfo{DataDays: cfg.RetentionDays, KeepDays: cfg.BackupRetentionDays})
		go bk.Run(bg)
		log.Info("daily backup of expiring data enabled", "dir", cfg.BackupDir, "live_days", cfg.RetentionDays, "backup_days", cfg.BackupRetentionDays)
	} else {
		log.Warn("LUMEN_BACKUP_DIR is empty: data is deleted after the retention period without a backup", "retention_days", cfg.RetentionDays)
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("lumen started", "addr", cfg.Addr, "edition", edition.Name, "version", buildinfo.Version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopBg()
	shutdown, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	srv.Shutdown(shutdown)
}
