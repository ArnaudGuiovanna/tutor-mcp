package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"tutor-mcp/db"
	"tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdio and HTTP both build their server with newTutorServer, so the
// initialize result must carry the learning-loop instructions.
func TestTutorServerAdvertisesInstructions(t *testing.T) {
	database, err := db.OpenDB(filepath.Join(t.TempDir(), "tutor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	server := newTutorServer(&tools.Deps{Store: db.NewStore(database), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), LocalStdio: true}, time.Second)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "instructions", Version: "0"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult().Instructions; got != tools.ServerInstructions || got == "" {
		t.Fatalf("initialize instructions = %q", got)
	}
}
