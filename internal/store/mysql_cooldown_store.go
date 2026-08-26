package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var _ cliproxyauth.CooldownStateStoreProvider = (*MySQLStore)(nil)
var _ cliproxyauth.CooldownStateStore = (*mysqlCooldownStateStore)(nil)

type mysqlCooldownStateKey struct {
	authID string
	model  string
}

type mysqlCooldownStateRecord struct {
	key       mysqlCooldownStateKey
	content   []byte
	updatedAt time.Time
}

type mysqlCooldownStateVersion struct {
	updatedAt time.Time
}

type mysqlCooldownStateStore struct {
	store    *MySQLStore
	mu       sync.Mutex
	previous map[mysqlCooldownStateKey]mysqlCooldownStateVersion
}

// CooldownStateStore returns the MySQL-backed runtime cooldown store.
func (s *MySQLStore) CooldownStateStore() cliproxyauth.CooldownStateStore {
	if s == nil {
		return nil
	}
	return s.cooldownStore
}

func (s *mysqlCooldownStateStore) Load(ctx context.Context) (records []cliproxyauth.CooldownStateRecord, err error) {
	if s == nil || s.store == nil || s.store.db == nil {
		return nil, fmt.Errorf("mysql cooldown store: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	table := s.store.fullTableName(s.store.cfg.CooldownTable)
	query := fmt.Sprintf("SELECT content, updated_at FROM %s WHERE deleted = FALSE", table)
	rows, errQuery := s.store.db.QueryContext(ctx, query)
	if errQuery != nil {
		return nil, fmt.Errorf("mysql cooldown store: load state: %w", errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("mysql cooldown store: close state rows: %w", errClose))
		}
	}()

	records = make([]cliproxyauth.CooldownStateRecord, 0)
	previous := make(map[mysqlCooldownStateKey]mysqlCooldownStateVersion)
	for rows.Next() {
		var content []byte
		var updatedAt time.Time
		if errScan := rows.Scan(&content, &updatedAt); errScan != nil {
			return nil, fmt.Errorf("mysql cooldown store: scan state: %w", errScan)
		}
		var record cliproxyauth.CooldownStateRecord
		if errUnmarshal := json.Unmarshal(content, &record); errUnmarshal != nil {
			return nil, fmt.Errorf("mysql cooldown store: decode state: %w", errUnmarshal)
		}
		key := mysqlCooldownStateKeyFromRecord(record)
		if key.authID == "" {
			return nil, fmt.Errorf("mysql cooldown store: decoded state has empty auth ID")
		}
		records = append(records, record)
		previous[key] = mysqlCooldownStateVersion{updatedAt: updatedAt}
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("mysql cooldown store: iterate state: %w", errRows)
	}
	s.previous = previous
	return records, nil
}

func (s *mysqlCooldownStateStore) Save(ctx context.Context, records []cliproxyauth.CooldownStateRecord) error {
	if s == nil || s.store == nil || s.store.db == nil {
		return fmt.Errorf("mysql cooldown store: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := normalizeMySQLCooldownTime(time.Now(), time.Time{})
	current := make(map[mysqlCooldownStateKey]mysqlCooldownStateVersion, len(records))
	encoded := make([]mysqlCooldownStateRecord, 0, len(records))
	for i := range records {
		record := records[i]
		key := mysqlCooldownStateKeyFromRecord(record)
		if key.authID == "" {
			return fmt.Errorf("mysql cooldown store: state has empty auth ID")
		}
		record.UpdatedAt = normalizeMySQLCooldownTime(record.UpdatedAt, now)
		content, errMarshal := json.Marshal(record)
		if errMarshal != nil {
			return fmt.Errorf("mysql cooldown store: encode state for %q: %w", key.authID, errMarshal)
		}
		current[key] = mysqlCooldownStateVersion{updatedAt: record.UpdatedAt}
		encoded = append(encoded, mysqlCooldownStateRecord{key: key, content: content, updatedAt: record.UpdatedAt})
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, errBegin := s.store.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("mysql cooldown store: begin save: %w", errBegin)
	}
	table := s.store.fullTableName(s.store.cfg.CooldownTable)
	upsertQuery := fmt.Sprintf(`
		INSERT INTO %s (auth_id, model, content, deleted, created_at, updated_at)
		VALUES (?, ?, ?, FALSE, NOW(6), ?)
		ON DUPLICATE KEY UPDATE
			content = IF(updated_at <= VALUES(updated_at), VALUES(content), content),
			deleted = IF(updated_at <= VALUES(updated_at), FALSE, deleted),
			updated_at = IF(updated_at <= VALUES(updated_at), VALUES(updated_at), updated_at)
	`, table)
	for i := range encoded {
		record := encoded[i]
		if _, errExec := tx.ExecContext(ctx, upsertQuery, record.key.authID, record.key.model, record.content, record.updatedAt); errExec != nil {
			return rollbackMySQLCooldownTransaction(tx, fmt.Errorf("mysql cooldown store: save state for %q: %w", record.key.authID, errExec))
		}
	}
	deleteQuery := fmt.Sprintf(`
		INSERT INTO %s (auth_id, model, content, deleted, created_at, updated_at)
		VALUES (?, ?, ?, TRUE, NOW(6), ?)
		ON DUPLICATE KEY UPDATE
			content = IF(NOT deleted AND updated_at <= ?, VALUES(content), content),
			deleted = IF(NOT deleted AND updated_at <= ?, TRUE, deleted),
			updated_at = IF(NOT deleted AND updated_at <= ?, VALUES(updated_at), updated_at)
	`, table)
	for key, previous := range s.previous {
		if _, ok := current[key]; ok {
			continue
		}
		deletedAt := now
		if !deletedAt.After(previous.updatedAt) {
			deletedAt = previous.updatedAt.Add(time.Microsecond)
		}
		if _, errExec := tx.ExecContext(ctx, deleteQuery, key.authID, key.model, []byte(`{}`), deletedAt, previous.updatedAt, previous.updatedAt, previous.updatedAt); errExec != nil {
			return rollbackMySQLCooldownTransaction(tx, fmt.Errorf("mysql cooldown store: clear state for %q: %w", key.authID, errExec))
		}
	}
	if errCommit := tx.Commit(); errCommit != nil {
		return fmt.Errorf("mysql cooldown store: commit save: %w", errCommit)
	}
	s.previous = current
	return nil
}

func mysqlCooldownStateKeyFromRecord(record cliproxyauth.CooldownStateRecord) mysqlCooldownStateKey {
	return mysqlCooldownStateKey{
		authID: strings.TrimSpace(record.AuthID),
		model:  strings.TrimSpace(record.Model),
	}
}

func normalizeMySQLCooldownTime(value, fallback time.Time) time.Time {
	if value.IsZero() {
		value = fallback
	}
	return value.UTC().Truncate(time.Microsecond)
}

func rollbackMySQLCooldownTransaction(tx *sql.Tx, operationErr error) error {
	if errRollback := tx.Rollback(); errRollback != nil && !errors.Is(errRollback, sql.ErrTxDone) {
		return errors.Join(operationErr, fmt.Errorf("mysql cooldown store: rollback save: %w", errRollback))
	}
	return operationErr
}
