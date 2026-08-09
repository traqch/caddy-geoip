package caddygeoip

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/oschwald/maxminddb-golang/v2"
	"go.uber.org/zap"
)

// dbPool keeps one *database per file path alive across config reloads. Caddy
// provisions a fresh handler instance on every reload; without the pool each
// reload would re-open (and, worse, re-download) a ~60 MB database.
var dbPool = caddy.NewUsagePool()

type dbConfig struct {
	Path              string
	ReloadFrequency   time.Duration
	DownloadURL       string
	DownloadFrequency time.Duration
	AccountID         string
	LicenseKey        string
}

// database owns the open reader plus the two background loops that keep the
// file current. Its lifetime is the pool's, not the caddy.Context's — a config
// reload must not tear down the refresh loops.
type database struct {
	cfg dbConfig
	log *zap.Logger

	mu      sync.RWMutex
	reader  *maxminddb.Reader
	modTime time.Time
	size    int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// loadDatabase returns the shared database for cfg.Path, creating and starting
// it on first use. Callers must pair this with releaseDatabase.
func loadDatabase(cfg dbConfig, log *zap.Logger) (*database, error) {
	val, loaded, err := dbPool.LoadOrNew(cfg.Path, func() (caddy.Destructor, error) {
		db := &database{cfg: cfg, log: log}
		if err := db.start(); err != nil {
			return nil, err
		}
		return db, nil
	})
	if err != nil {
		return nil, err
	}

	db := val.(*database)
	if loaded && db.cfg != cfg {
		// Two site blocks pointing at the same file with different refresh
		// settings: the first one won. Silently ignoring that would make the
		// effective config unguessable from the Caddyfile.
		log.Warn("geoip database already in use with a different configuration; existing settings kept",
			zap.String("db_path", cfg.Path))
	}
	return db, nil
}

func releaseDatabase(path string) error {
	_, err := dbPool.Delete(path)
	return err
}

func (db *database) start() error {
	if _, err := os.Stat(db.cfg.Path); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", db.cfg.Path, err)
		}
		if db.cfg.DownloadURL == "" {
			return fmt.Errorf("geoip database %s does not exist and no download_url is configured", db.cfg.Path)
		}
		// Deliberately fatal on failure. Starting without geo data means every
		// request resolves as unknown, and downstream logic keyed on geo then
		// applies one wrong answer to everyone — a process that refuses to
		// start is the louder, safer failure.
		if err := db.download(context.Background()); err != nil {
			return fmt.Errorf("initial geoip download: %w", err)
		}
	}

	if err := db.reload(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	db.cancel = cancel

	if db.cfg.ReloadFrequency > 0 {
		db.wg.Add(1)
		go db.loop(ctx, db.cfg.ReloadFrequency, func() {
			if err := db.reload(); err != nil {
				db.log.Error("geoip database reload failed", zap.Error(err), zap.String("db_path", db.cfg.Path))
			}
		})
	}

	if db.cfg.DownloadURL != "" && db.cfg.DownloadFrequency > 0 {
		db.wg.Add(1)
		go db.loop(ctx, db.cfg.DownloadFrequency, func() {
			if err := db.download(ctx); err != nil {
				// The previous database stays loaded and keeps serving.
				db.log.Error("geoip database download failed", zap.Error(err), zap.String("url", redactURL(db.cfg.DownloadURL)))
				return
			}
			if err := db.reload(); err != nil {
				db.log.Error("geoip database reload after download failed", zap.Error(err))
			}
		})
	}

	return nil
}

func (db *database) loop(ctx context.Context, every time.Duration, fn func()) {
	defer db.wg.Done()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

// reload re-opens the file if it changed on disk. Cheap enough to run on a
// short interval: a stat that matches is the common case and costs nothing.
func (db *database) reload() error {
	info, err := os.Stat(db.cfg.Path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", db.cfg.Path, err)
	}

	db.mu.RLock()
	unchanged := db.reader != nil && info.ModTime().Equal(db.modTime) && info.Size() == db.size
	db.mu.RUnlock()
	if unchanged {
		return nil
	}

	reader, err := maxminddb.Open(db.cfg.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", db.cfg.Path, err)
	}

	db.mu.Lock()
	old := db.reader
	db.reader = reader
	db.modTime = info.ModTime()
	db.size = info.Size()
	db.mu.Unlock()

	// Safe to close only once no lookup can still be holding it: the write
	// lock above drained every in-flight reader.
	if old != nil {
		if err := old.Close(); err != nil {
			db.log.Warn("closing previous geoip database failed", zap.Error(err))
		}
	}

	db.log.Info("geoip database loaded",
		zap.String("db_path", db.cfg.Path),
		zap.String("database_type", reader.Metadata.DatabaseType),
		zap.Uint("build_epoch", uint(reader.Metadata.BuildEpoch)),
		zap.Time("mod_time", info.ModTime()),
	)
	return nil
}

// lookup resolves addr. The second return value distinguishes "no data for
// this address" from "resolved", so the caller can emit the empty set instead
// of guessing.
func (db *database) lookup(addr netip.Addr) (*cityRecord, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if db.reader == nil {
		return nil, false
	}

	result := db.reader.Lookup(addr)
	if !result.Found() {
		return nil, false
	}

	var rec cityRecord
	if err := result.Decode(&rec); err != nil {
		db.log.Warn("geoip record decode failed", zap.Error(err), zap.String("ip", addr.String()))
		return nil, false
	}
	return &rec, true
}

// Destruct implements caddy.Destructor; the pool calls it when the last
// handler referencing this path goes away.
func (db *database) Destruct() error {
	if db.cancel != nil {
		db.cancel()
	}
	db.wg.Wait()

	db.mu.Lock()
	defer db.mu.Unlock()
	if db.reader != nil {
		err := db.reader.Close()
		db.reader = nil
		return err
	}
	return nil
}
