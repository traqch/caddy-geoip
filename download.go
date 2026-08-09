package caddygeoip

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
	"go.uber.org/zap"
)

const (
	downloadTimeout = 5 * time.Minute
	// tar headers carry "ustar" at this offset; enough to tell a tarball from
	// a bare .mmdb without buffering the whole body.
	tarMagicOffset = 257
	tarPeekSize    = 512
)

// download fetches the configured URL and atomically replaces the database
// file. MaxMind serves a gzipped tarball, but a plain .mmdb or a bare .gz is
// accepted too so the file can also come from an internal mirror.
func (db *database) download(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, db.cfg.DownloadURL, nil)
	if err != nil {
		return err
	}
	if db.cfg.AccountID != "" || db.cfg.LicenseKey != "" {
		req.SetBasicAuth(db.cfg.AccountID, db.cfg.LicenseKey)
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body may carry a MaxMind error document; a truncated peek is
		// enough to tell "bad license key" from "rate limited".
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("download returned %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	dir := filepath.Dir(db.cfg.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	// Same directory as the target so the rename below stays on one filesystem
	// and is therefore atomic.
	tmp, err := os.CreateTemp(dir, ".geoip-download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename succeeded
	}()

	src, err := unwrap(resp.Body)
	if err != nil {
		return err
	}

	written, err := io.Copy(tmp, src)
	if err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Verify before replacing: swapping in a truncated or HTML-error file
	// would take geo resolution down until the next successful download.
	if err := verify(tmpName); err != nil {
		return err
	}

	if err := os.Rename(tmpName, db.cfg.Path); err != nil {
		return fmt.Errorf("replace %s: %w", db.cfg.Path, err)
	}

	db.log.Info("geoip database downloaded",
		zap.String("url", redactURL(db.cfg.DownloadURL)),
		zap.String("db_path", db.cfg.Path),
		zap.Int64("bytes", written),
		zap.Duration("took", time.Since(start)),
	)
	return nil
}

// safeQueryParams are the only query parameters allowed into a log line. An
// allowlist rather than a denylist on purpose: MaxMind's account-ID-less
// endpoint carries the license key as `license_key`, and any download URL may
// carry a signed token, so anything unrecognised is treated as a secret.
var safeQueryParams = map[string]bool{
	"edition_id": true,
	"suffix":     true,
	"date":       true,
	"version":    true,
	"format":     true,
}

// redactURL strips credentials from a URL before it is logged. Caddy's logs
// are shipped off the node; a license key in them is a leaked secret.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparsable url]"
	}

	u.User = nil

	if q := u.Query(); len(q) > 0 {
		for key := range q {
			if !safeQueryParams[strings.ToLower(key)] {
				q.Set(key, "REDACTED")
			}
		}
		u.RawQuery = q.Encode()
	}

	return u.String()
}

// unwrap peels gzip and tar layers off the response body and returns a reader
// positioned at the raw .mmdb bytes.
func unwrap(body io.Reader) (io.Reader, error) {
	br := bufio.NewReaderSize(body, tarPeekSize*2)

	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("gunzip: %w", err)
		}
		br = bufio.NewReaderSize(gr, tarPeekSize*2)
	}

	head, err := br.Peek(tarPeekSize)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if !isTar(head) {
		return br, nil
	}

	tr := tar.NewReader(br)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no .mmdb file in downloaded archive")
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && strings.HasSuffix(hdr.Name, ".mmdb") {
			return tr, nil
		}
	}
}

func isTar(head []byte) bool {
	if len(head) < tarMagicOffset+5 {
		return false
	}
	return string(head[tarMagicOffset:tarMagicOffset+5]) == "ustar"
}

// verify opens the candidate file and checks its internal consistency claim,
// so a corrupt download is rejected before it replaces a working database.
func verify(path string) error {
	reader, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("downloaded file is not a valid mmdb: %w", err)
	}
	defer reader.Close()

	if !strings.Contains(reader.Metadata.DatabaseType, "City") {
		return fmt.Errorf("downloaded database is %q, expected a City database", reader.Metadata.DatabaseType)
	}
	return nil
}
