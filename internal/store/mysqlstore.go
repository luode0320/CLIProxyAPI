package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// MySQLStoreConfig captures configuration required to initialize a MySQL-backed store.
type MySQLStoreConfig struct {
	DSN           string
	ConfigTable   string
	AuthTable     string
	CooldownTable string
	SpoolDir      string
}

// MySQLStore persists configuration and authentication metadata using MySQL as backend
// while mirroring data to a local workspace so existing file-based workflows continue to operate.
type MySQLStore struct {
	db            *sql.DB
	cfg           MySQLStoreConfig
	spoolRoot     string
	configPath    string
	authDir       string
	cooldownStore *mysqlCooldownStateStore
	mu            sync.Mutex
}

// NewMySQLStore establishes a connection to MySQL and prepares the local workspace.
func NewMySQLStore(ctx context.Context, cfg MySQLStoreConfig) (*MySQLStore, error) {
	trimmedDSN := strings.TrimSpace(cfg.DSN)
	if trimmedDSN == "" {
		return nil, fmt.Errorf("mysql store: DSN is required")
	}
	cfg.DSN = normalizeMySQLDSN(trimmedDSN)
	if cfg.ConfigTable == "" {
		cfg.ConfigTable = defaultConfigTable
	}
	if cfg.AuthTable == "" {
		cfg.AuthTable = defaultAuthTable
	}
	if cfg.CooldownTable == "" {
		cfg.CooldownTable = defaultCooldownTable
	}

	spoolRoot := strings.TrimSpace(cfg.SpoolDir)
	if spoolRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			spoolRoot = filepath.Join(cwd, "mysqlstore")
		} else {
			spoolRoot = filepath.Join(os.TempDir(), "mysqlstore")
		}
	}
	absSpool, err := filepath.Abs(spoolRoot)
	if err != nil {
		return nil, fmt.Errorf("mysql store: resolve spool directory: %w", err)
	}
	configDir := filepath.Join(absSpool, "config")
	authDir := filepath.Join(absSpool, "auths")
	if err = os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("mysql store: create config directory: %w", err)
	}
	if err = os.MkdirAll(authDir, 0o700); err != nil {
		return nil, fmt.Errorf("mysql store: create auth directory: %w", err)
	}

	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("mysql store: open database connection: %w", err)
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql store: ping database: %w", err)
	}

	store := &MySQLStore{
		db:         db,
		cfg:        cfg,
		spoolRoot:  absSpool,
		configPath: filepath.Join(configDir, "config.yaml"),
		authDir:    authDir,
	}
	store.cooldownStore = &mysqlCooldownStateStore{store: store}
	return store, nil
}

// normalizeMySQLDSN rewrites the DSN through the driver parser so that
// parseTime=true is always enabled; without it DATETIME columns cannot be
// scanned into time.Time. Unparseable DSNs are returned unchanged and fail
// later at sql.Open/Ping with the driver's own error.
func normalizeMySQLDSN(dsn string) string {
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil || parsed == nil {
		return dsn
	}
	if !parsed.ParseTime {
		parsed.ParseTime = true
		return parsed.FormatDSN()
	}
	return dsn
}

// Close releases the underlying database connection.
func (s *MySQLStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// EnsureSchema creates the required tables.
func (s *MySQLStore) EnsureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("mysql store: not initialized")
	}
	configTable := s.fullTableName(s.cfg.ConfigTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id VARCHAR(255) NOT NULL COMMENT 'config key',
			content LONGTEXT NOT NULL COMMENT 'config file content',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT 'creation time',
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6) COMMENT 'last update time',
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='config store'
	`, configTable)); err != nil {
		return fmt.Errorf("mysql store: create config table: %w", err)
	}
	authTable := s.fullTableName(s.cfg.AuthTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			id VARCHAR(255) NOT NULL COMMENT 'auth record id',
			content JSON NOT NULL COMMENT 'auth metadata json',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT 'creation time',
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6) COMMENT 'last update time',
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='auth store'
	`, authTable)); err != nil {
		return fmt.Errorf("mysql store: create auth table: %w", err)
	}
	cooldownTable := s.fullTableName(s.cfg.CooldownTable)
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			auth_id VARCHAR(255) NOT NULL COMMENT 'auth id',
			model VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'model name',
			content JSON NOT NULL COMMENT 'cooldown state json',
			deleted TINYINT(1) NOT NULL DEFAULT 0 COMMENT 'logical delete flag',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT 'creation time',
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6) COMMENT 'last update time',
			PRIMARY KEY (auth_id, model)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='cooldown state store'
	`, cooldownTable)); err != nil {
		return fmt.Errorf("mysql store: create cooldown table: %w", err)
	}
	return nil
}

// Bootstrap synchronizes configuration and auth records between MySQL and the local workspace.
func (s *MySQLStore) Bootstrap(ctx context.Context, exampleConfigPath string) error {
	if err := s.EnsureSchema(ctx); err != nil {
		return err
	}
	if err := s.syncConfigFromDatabase(ctx, exampleConfigPath); err != nil {
		return err
	}
	if err := s.syncAuthFromDatabase(ctx); err != nil {
		return err
	}
	return nil
}

// ConfigPath returns the managed configuration file path inside the spool directory.
func (s *MySQLStore) ConfigPath() string {
	if s == nil {
		return ""
	}
	return s.configPath
}

// AuthDir returns the local directory containing mirrored auth files.
func (s *MySQLStore) AuthDir() string {
	if s == nil {
		return ""
	}
	return s.authDir
}

// WorkDir exposes the root spool directory used for mirroring.
func (s *MySQLStore) WorkDir() string {
	if s == nil {
		return ""
	}
	return s.spoolRoot
}

// SetBaseDir implements the optional interface used by authenticators; it is a no-op because
// the MySQL-backed store controls its own workspace.
func (s *MySQLStore) SetBaseDir(string) {}

// Save persists authentication metadata to disk and MySQL.
func (s *MySQLStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("mysql store: auth is nil")
	}
	if errWeight := cliproxyauth.ValidateAuthWeight(auth); errWeight != nil {
		return "", fmt.Errorf("mysql store: %w", errWeight)
	}

	path, err := s.resolveAuthPath(auth)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", fmt.Errorf("mysql store: missing file path attribute for %s", auth.ID)
	}

	if auth.Disabled {
		if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
			return "", nil
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mysql store: create auth directory: %w", err)
	}

	switch {
	case auth.Storage != nil:
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		auth.Metadata["disabled"] = auth.Disabled
		if setter, ok := auth.Storage.(interface{ SetMetadata(map[string]any) }); ok {
			setter.SetMetadata(auth.Metadata)
		}
		if err = auth.Storage.SaveTokenToFile(path); err != nil {
			return "", err
		}
	case auth.Metadata != nil:
		auth.Metadata["disabled"] = auth.Disabled
		raw, errMarshal := json.Marshal(auth.Metadata)
		if errMarshal != nil {
			return "", fmt.Errorf("mysql store: marshal metadata: %w", errMarshal)
		}
		if existing, errRead := os.ReadFile(path); errRead == nil {
			if jsonEqual(existing, raw) {
				return path, nil
			}
		} else if errRead != nil && !errors.Is(errRead, fs.ErrNotExist) {
			return "", fmt.Errorf("mysql store: read existing metadata: %w", errRead)
		}
		tmp := path + ".tmp"
		if errWrite := os.WriteFile(tmp, raw, 0o600); errWrite != nil {
			return "", fmt.Errorf("mysql store: write temp auth file: %w", errWrite)
		}
		if errRename := os.Rename(tmp, path); errRename != nil {
			return "", fmt.Errorf("mysql store: rename auth file: %w", errRename)
		}
	default:
		return "", fmt.Errorf("mysql store: nothing to persist for %s", auth.ID)
	}

	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[cliproxyauth.AttributePath] = path
	auth.Attributes[cliproxyauth.AttributeSourceBackend] = cliproxyauth.AuthSourceMySQL

	if strings.TrimSpace(auth.FileName) == "" {
		auth.FileName = auth.ID
	}

	relID, err := s.relativeAuthID(path)
	if err != nil {
		return "", err
	}
	if err = s.upsertAuthRecord(ctx, relID, path); err != nil {
		return "", err
	}
	return path, nil
}

// List enumerates all auth records stored in MySQL.
func (s *MySQLStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	query := fmt.Sprintf("SELECT id, content, created_at, updated_at FROM %s ORDER BY id", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("mysql store: list auth: %w", err)
	}
	defer rows.Close()

	auths := make([]*cliproxyauth.Auth, 0, 32)
	for rows.Next() {
		var (
			id        string
			payload   string
			createdAt time.Time
			updatedAt time.Time
		)
		if err = rows.Scan(&id, &payload, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("mysql store: scan auth row: %w", err)
		}
		path, errPath := s.absoluteAuthPath(id)
		if errPath != nil {
			log.WithError(errPath).Warnf("mysql store: skipping auth %s outside spool", id)
			continue
		}
		metadata := make(map[string]any)
		if err = json.Unmarshal([]byte(payload), &metadata); err != nil {
			log.WithError(err).Warnf("mysql store: skipping auth %s with invalid json", id)
			continue
		}
		if errWeight := cliproxyauth.ValidateAuthWeight(&cliproxyauth.Auth{Metadata: metadata}); errWeight != nil {
			log.WithError(errWeight).Warnf("mysql store: skipping auth %s with invalid weight", id)
			continue
		}
		provider := strings.TrimSpace(valueAsString(metadata["type"]))
		if provider == "" {
			provider = "unknown"
		}
		attr := map[string]string{
			cliproxyauth.AttributePath:          path,
			cliproxyauth.AttributeSourceBackend: cliproxyauth.AuthSourceMySQL,
		}
		if email := strings.TrimSpace(valueAsString(metadata["email"])); email != "" {
			attr["email"] = email
		}
		auth := &cliproxyauth.Auth{
			ID:               normalizeAuthID(id),
			Provider:         provider,
			FileName:         normalizeAuthID(id),
			Label:            labelFor(metadata),
			Status:           cliproxyauth.StatusActive,
			Attributes:       attr,
			Metadata:         metadata,
			CreatedAt:        createdAt,
			UpdatedAt:        updatedAt,
			LastRefreshedAt:  time.Time{},
			NextRefreshAfter: time.Time{},
		}
		cliproxyauth.ApplyCustomHeadersFromMetadata(auth)
		if disabled, ok := metadata["disabled"].(bool); ok && disabled {
			auth.Disabled = true
			auth.Status = cliproxyauth.StatusDisabled
		}
		auths = append(auths, auth)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql store: iterate auth rows: %w", err)
	}
	return auths, nil
}

// Delete removes an auth file and the corresponding database record.
func (s *MySQLStore) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("mysql store: id is empty")
	}
	path, err := s.resolveDeletePath(id)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mysql store: delete auth file: %w", err)
	}
	relID, err := s.relativeAuthID(path)
	if err != nil {
		return err
	}
	return s.deleteAuthRecord(ctx, relID)
}

// PersistAuthFiles stores the provided auth file changes in MySQL.
func (s *MySQLStore) PersistAuthFiles(ctx context.Context, _ string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		relID, err := s.relativeAuthID(trimmed)
		if err != nil {
			// Attempt to resolve absolute path under authDir.
			abs := trimmed
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(s.authDir, trimmed)
			}
			relID, err = s.relativeAuthID(abs)
			if err != nil {
				log.WithError(err).Warnf("mysql store: ignoring auth path %s", trimmed)
				continue
			}
			trimmed = abs
		}
		if err = s.syncAuthFile(ctx, relID, trimmed); err != nil {
			return err
		}
	}
	return nil
}

// PersistConfig mirrors the local configuration file to MySQL.
func (s *MySQLStore) PersistConfig(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteConfigRecord(ctx)
		}
		return fmt.Errorf("mysql store: read config file: %w", err)
	}
	return s.persistConfig(ctx, data)
}

// syncConfigFromDatabase writes the database-stored config to disk or seeds the database from template.
func (s *MySQLStore) syncConfigFromDatabase(ctx context.Context, exampleConfigPath string) error {
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = ?", s.fullTableName(s.cfg.ConfigTable))
	var content string
	err := s.db.QueryRowContext(ctx, query, defaultConfigKey).Scan(&content)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, errStat := os.Stat(s.configPath); errors.Is(errStat, fs.ErrNotExist) {
			if exampleConfigPath != "" {
				if errCopy := misc.CopyConfigTemplate(exampleConfigPath, s.configPath); errCopy != nil {
					return fmt.Errorf("mysql store: copy example config: %w", errCopy)
				}
			} else {
				if errCreate := os.MkdirAll(filepath.Dir(s.configPath), 0o700); errCreate != nil {
					return fmt.Errorf("mysql store: prepare config directory: %w", errCreate)
				}
				if errWrite := os.WriteFile(s.configPath, []byte{}, 0o600); errWrite != nil {
					return fmt.Errorf("mysql store: create empty config: %w", errWrite)
				}
			}
		}
		data, errRead := os.ReadFile(s.configPath)
		if errRead != nil {
			return fmt.Errorf("mysql store: read local config: %w", errRead)
		}
		if errPersist := s.persistConfig(ctx, data); errPersist != nil {
			return errPersist
		}
	case err != nil:
		return fmt.Errorf("mysql store: load config from database: %w", err)
	default:
		if err = os.MkdirAll(filepath.Dir(s.configPath), 0o700); err != nil {
			return fmt.Errorf("mysql store: prepare config directory: %w", err)
		}
		normalized := normalizeLineEndings(content)
		if err = os.WriteFile(s.configPath, []byte(normalized), 0o600); err != nil {
			return fmt.Errorf("mysql store: write config to spool: %w", err)
		}
	}
	return nil
}

// syncAuthFromDatabase populates the local auth directory from MySQL data.
func (s *MySQLStore) syncAuthFromDatabase(ctx context.Context) error {
	query := fmt.Sprintf("SELECT id, content FROM %s", s.fullTableName(s.cfg.AuthTable))
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("mysql store: load auth from database: %w", err)
	}
	defer rows.Close()

	if err = os.RemoveAll(s.authDir); err != nil {
		return fmt.Errorf("mysql store: reset auth directory: %w", err)
	}
	if err = os.MkdirAll(s.authDir, 0o700); err != nil {
		return fmt.Errorf("mysql store: recreate auth directory: %w", err)
	}

	for rows.Next() {
		var (
			id      string
			payload string
		)
		if err = rows.Scan(&id, &payload); err != nil {
			return fmt.Errorf("mysql store: scan auth row: %w", err)
		}
		path, errPath := s.absoluteAuthPath(id)
		if errPath != nil {
			log.WithError(errPath).Warnf("mysql store: skipping auth %s outside spool", id)
			continue
		}
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("mysql store: create auth subdir: %w", err)
		}
		if err = os.WriteFile(path, []byte(payload), 0o600); err != nil {
			return fmt.Errorf("mysql store: write auth file: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("mysql store: iterate auth rows: %w", err)
	}
	return nil
}

func (s *MySQLStore) syncAuthFile(ctx context.Context, relID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s.deleteAuthRecord(ctx, relID)
		}
		return fmt.Errorf("mysql store: read auth file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteAuthRecord(ctx, relID)
	}
	return s.persistAuth(ctx, relID, data)
}

func (s *MySQLStore) upsertAuthRecord(ctx context.Context, relID, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mysql store: read auth file: %w", err)
	}
	if len(data) == 0 {
		return s.deleteAuthRecord(ctx, relID)
	}
	return s.persistAuth(ctx, relID, data)
}

func (s *MySQLStore) persistAuth(ctx context.Context, relID string, data []byte) error {
	jsonPayload := json.RawMessage(data)
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, created_at, updated_at)
		VALUES (?, ?, NOW(6), NOW(6))
		ON DUPLICATE KEY UPDATE content = VALUES(content), updated_at = NOW(6)
	`, s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID, jsonPayload); err != nil {
		return fmt.Errorf("mysql store: upsert auth record: %w", err)
	}
	return nil
}

func (s *MySQLStore) deleteAuthRecord(ctx context.Context, relID string) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE id = ?", s.fullTableName(s.cfg.AuthTable))
	if _, err := s.db.ExecContext(ctx, query, relID); err != nil {
		return fmt.Errorf("mysql store: delete auth record: %w", err)
	}
	return nil
}

func (s *MySQLStore) persistConfig(ctx context.Context, data []byte) error {
	query := fmt.Sprintf(`
		INSERT INTO %s (id, content, created_at, updated_at)
		VALUES (?, ?, NOW(6), NOW(6))
		ON DUPLICATE KEY UPDATE content = VALUES(content), updated_at = NOW(6)
	`, s.fullTableName(s.cfg.ConfigTable))
	normalized := normalizeLineEndings(string(data))
	if _, err := s.db.ExecContext(ctx, query, defaultConfigKey, normalized); err != nil {
		return fmt.Errorf("mysql store: upsert config: %w", err)
	}
	return nil
}

func (s *MySQLStore) deleteConfigRecord(ctx context.Context) error {
	query := fmt.Sprintf("DELETE FROM %s WHERE id = ?", s.fullTableName(s.cfg.ConfigTable))
	if _, err := s.db.ExecContext(ctx, query, defaultConfigKey); err != nil {
		return fmt.Errorf("mysql store: delete config: %w", err)
	}
	return nil
}

func (s *MySQLStore) resolveAuthPath(auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", fmt.Errorf("mysql store: auth is nil")
	}
	if auth.Attributes != nil {
		if p := strings.TrimSpace(auth.Attributes["path"]); p != "" {
			return p, nil
		}
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		if filepath.IsAbs(fileName) {
			return fileName, nil
		}
		return filepath.Join(s.authDir, fileName), nil
	}
	if auth.ID == "" {
		return "", fmt.Errorf("mysql store: missing id")
	}
	if filepath.IsAbs(auth.ID) {
		return auth.ID, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(auth.ID)), nil
}

func (s *MySQLStore) resolveDeletePath(id string) (string, error) {
	if strings.ContainsRune(id, os.PathSeparator) || filepath.IsAbs(id) {
		return id, nil
	}
	return filepath.Join(s.authDir, filepath.FromSlash(id)), nil
}

func (s *MySQLStore) relativeAuthID(path string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("mysql store: store not initialized")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.authDir, path)
	}
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(s.authDir, clean)
	if err != nil {
		return "", fmt.Errorf("mysql store: compute relative path: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("mysql store: path %s outside managed directory", path)
	}
	return filepath.ToSlash(rel), nil
}

func (s *MySQLStore) absoluteAuthPath(id string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("mysql store: store not initialized")
	}
	clean := filepath.Clean(filepath.FromSlash(id))
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("mysql store: invalid auth identifier %s", id)
	}
	path := filepath.Join(s.authDir, clean)
	rel, err := filepath.Rel(s.authDir, path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("mysql store: resolved auth path escapes auth directory")
	}
	return path, nil
}

func (s *MySQLStore) fullTableName(name string) string {
	return quoteMySQLIdentifier(name)
}

func quoteMySQLIdentifier(identifier string) string {
	replaced := strings.ReplaceAll(identifier, "`", "``")
	return "`" + replaced + "`"
}
