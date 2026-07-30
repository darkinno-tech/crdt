package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/DarkInno/crdt"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresStoreAcceptance exercises PostgreSQL only when a caller
// explicitly supplies CRDT_POSTGRES_DSN. It creates the fixed schema, then
// verifies a real append/replay transaction against the selected server.
func TestPostgresStoreAcceptance(t *testing.T) {
	dsn := os.Getenv("CRDT_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set CRDT_POSTGRES_DSN to run PostgreSQL acceptance")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := New(pool, Config{MaxEvents: 8, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest := testManifest(t)
	change := testChange(t, manifest, "acceptance-postgres", 1, 1)
	if _, err := store.Append(manifest.GroupID, change); err != nil {
		t.Fatal(err)
	}
	events, highWater, err := store.Replay(manifest.GroupID, 0, 8, 1<<20, manifest, crdt.ProtocolPolicy{}, 1<<20, 128)
	if err != nil || highWater == 0 || len(events) == 0 {
		t.Fatalf("real PostgreSQL replay = high=%d events=%d err=%v", highWater, len(events), err)
	}
}
