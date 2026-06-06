package db_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"tutor-mcp/db"
	"tutor-mcp/store"
	"tutor-mcp/store/conformance"
)

var pgConfCounter atomic.Int64

func TestPostgresConformance(t *testing.T) {
	base := os.Getenv("TUTOR_TEST_PG_DSN")
	if base == "" {
		t.Skip("set TUTOR_TEST_PG_DSN to run Postgres conformance")
	}
	conformance.RunConformance(t, func(t *testing.T) store.Store {
		n := pgConfCounter.Add(1)
		schema := fmt.Sprintf("conf_%d", n)
		admin, err := sql.Open("pgx", base)
		if err != nil {
			t.Fatal(err)
		}
		admin.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		d, err := sql.Open("pgx", base+sep+"search_path="+schema)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.MigratePostgres(context.Background(), d); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			d.Close()
			admin.Exec("DROP SCHEMA " + schema + " CASCADE")
			admin.Close()
		})
		return db.NewStoreWithDialect(d, db.DialectPostgres)
	})
}
