package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var mysqlCooldownTestDriverID uint64

func newMySQLCooldownTestStore(t *testing.T, state *cooldownTestState) (*MySQLStore, *mysqlCooldownStateStore) {
	t.Helper()
	driverName := fmt.Sprintf("cliproxy_mysql_cooldown_test_%d", mysqlCooldownTestDriverID+1)
	mysqlCooldownTestDriverID++
	sql.Register(driverName, &cooldownTestDriver{state: state})
	db, errOpen := sql.Open(driverName, "")
	if errOpen != nil {
		t.Fatalf("sql.Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		if errClose := db.Close(); errClose != nil {
			t.Errorf("db.Close() error = %v", errClose)
		}
	})

	mysqlStore := &MySQLStore{
		db: db,
		cfg: MySQLStoreConfig{
			ConfigTable:   defaultConfigTable,
			AuthTable:     defaultAuthTable,
			CooldownTable: defaultCooldownTable,
		},
	}
	cooldownStore := &mysqlCooldownStateStore{store: mysqlStore}
	mysqlStore.cooldownStore = cooldownStore
	return mysqlStore, cooldownStore
}

func TestMySQLCooldownStateStore_SaveLoad(t *testing.T) {
	state := &cooldownTestState{rows: make(map[string]cooldownTestRow)}
	mysqlStore, cooldownStore := newMySQLCooldownTestStore(t, state)

	if errSchema := mysqlStore.EnsureSchema(context.Background()); errSchema != nil {
		t.Fatalf("EnsureSchema() error = %v", errSchema)
	}
	if got := mysqlStore.CooldownStateStore(); got != cooldownStore {
		t.Fatalf("CooldownStateStore() = %T, want configured MySQL store", got)
	}

	nextRetry := time.Date(2026, time.March, 15, 12, 0, 0, 0, time.UTC)
	records := []cliproxyauth.CooldownStateRecord{
		{
			Provider:       "codex",
			AuthID:         "account-1",
			Model:          "gpt-test",
			Status:         string(cliproxyauth.StatusError),
			NextRetryAfter: nextRetry,
			Reason:         "rate limited",
			UpdatedAt:      nextRetry.Add(-time.Minute),
		},
	}
	if errSave := cooldownStore.Save(context.Background(), records); errSave != nil {
		t.Fatalf("Save() error = %v", errSave)
	}
	loaded, errLoad := cooldownStore.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() error = %v", errLoad)
	}
	if !reflect.DeepEqual(loaded, records) {
		t.Fatalf("Load() = %#v, want %#v", loaded, records)
	}

	zeroTimeRecord := cliproxyauth.CooldownStateRecord{AuthID: "account-2", Model: "gpt-test"}
	if errSave := cooldownStore.Save(context.Background(), []cliproxyauth.CooldownStateRecord{zeroTimeRecord}); errSave != nil {
		t.Fatalf("Save() with zero UpdatedAt error = %v", errSave)
	}
	loaded, errLoad = cooldownStore.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() after zero UpdatedAt error = %v", errLoad)
	}
	if len(loaded) != 1 || loaded[0].UpdatedAt.IsZero() {
		t.Fatalf("Load() did not persist a normalized UpdatedAt: %#v", loaded)
	}

	if errSave := cooldownStore.Save(context.Background(), nil); errSave != nil {
		t.Fatalf("Save(nil) error = %v", errSave)
	}
	loaded, errLoad = cooldownStore.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("Load() after Save(nil) error = %v", errLoad)
	}
	if len(loaded) != 0 {
		t.Fatalf("Load() after Save(nil) returned %d records, want 0", len(loaded))
	}

	state.mu.Lock()
	queries := strings.Join(state.queries, "\n")
	state.mu.Unlock()
	if !strings.Contains(queries, "CREATE TABLE IF NOT EXISTS `cooldown_store`") {
		t.Fatalf("EnsureSchema() did not create cooldown table; queries:\n%s", queries)
	}
}

func TestMySQLCooldownStateStore_MergesConcurrentInstances(t *testing.T) {
	state := &cooldownTestState{rows: make(map[string]cooldownTestRow)}
	_, cooldownStore := newMySQLCooldownTestStore(t, state)
	mysqlStore := cooldownStore.store
	storeA := &mysqlCooldownStateStore{store: mysqlStore}
	storeB := &mysqlCooldownStateStore{store: mysqlStore}
	staleStore := &mysqlCooldownStateStore{store: mysqlStore}

	for _, store := range []*mysqlCooldownStateStore{storeA, storeB} {
		if _, errLoad := store.Load(context.Background()); errLoad != nil {
			t.Fatalf("initial Load() error = %v", errLoad)
		}
	}
	updatedAt := time.Now().UTC().Add(-time.Minute)
	recordA := cliproxyauth.CooldownStateRecord{AuthID: "account-a", Model: "model-a", UpdatedAt: updatedAt}
	recordB := cliproxyauth.CooldownStateRecord{AuthID: "account-b", Model: "model-b", UpdatedAt: updatedAt}
	if errSave := storeA.Save(context.Background(), []cliproxyauth.CooldownStateRecord{recordA}); errSave != nil {
		t.Fatalf("storeA.Save() error = %v", errSave)
	}
	if errSave := storeB.Save(context.Background(), []cliproxyauth.CooldownStateRecord{recordB}); errSave != nil {
		t.Fatalf("storeB.Save() error = %v", errSave)
	}
	staleRecords, errLoad := staleStore.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("staleStore.Load() error = %v", errLoad)
	}
	if len(staleRecords) != 2 {
		t.Fatalf("merged Load() returned %d records, want 2", len(staleRecords))
	}

	newerRecordA := recordA
	newerRecordA.UpdatedAt = updatedAt.Add(time.Hour)
	if errSave := storeA.Save(context.Background(), []cliproxyauth.CooldownStateRecord{newerRecordA}); errSave != nil {
		t.Fatalf("storeA.Save(newer) error = %v", errSave)
	}
	if errSave := staleStore.Save(context.Background(), []cliproxyauth.CooldownStateRecord{recordB}); errSave != nil {
		t.Fatalf("staleStore.Save(without newer record) error = %v", errSave)
	}
	resurrectStore := &mysqlCooldownStateStore{store: mysqlStore}
	activeRecords, errLoad := resurrectStore.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("resurrectStore.Load() error = %v", errLoad)
	}
	if len(activeRecords) != 2 {
		t.Fatalf("Load() after stale delete returned %d records, want 2", len(activeRecords))
	}

	if errSave := storeA.Save(context.Background(), nil); errSave != nil {
		t.Fatalf("storeA.Save(nil) error = %v", errSave)
	}
	if errSave := resurrectStore.Save(context.Background(), activeRecords); errSave != nil {
		t.Fatalf("resurrectStore.Save() error = %v", errSave)
	}
	reader := &mysqlCooldownStateStore{store: mysqlStore}
	loaded, errLoad := reader.Load(context.Background())
	if errLoad != nil {
		t.Fatalf("reader.Load() error = %v", errLoad)
	}
	if len(loaded) != 1 || loaded[0].AuthID != recordB.AuthID {
		t.Fatalf("Load() after stale save = %#v, want only account-b", loaded)
	}
}
