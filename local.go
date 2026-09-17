package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/engine"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	"tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTutorServer(deps *tools.Deps, timeout time.Duration) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "tutor-mcp", Version: mcpVersion()}, nil)
	tools.RegisterTools(server, deps)
	server.AddReceivingMiddleware(mcpToolCallDeadlineMiddleware(timeout))
	return server
}

func runLocal(options commandOptions, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := profileDataDir("local", options.DataDir)
	if err != nil {
		return err
	}
	store, _, err := openProfileStore(ctx, "local", dir, "")
	if err != nil {
		return err
	}
	defer store.Close()
	id, err := store.EnsureLocalIdentity(ctx)
	if err != nil {
		return err
	}
	principal, err := store.GetPrincipalForLearner(ctx, id, []string{models.OAuthScopeLearner})
	if err != nil {
		return err
	}
	ctx, err = auth.WithPrincipal(ctx, principal)
	if err != nil {
		return err
	}
	ctx = auth.WithOAuthScope(ctx, models.OAuthScopeLearner)
	memory.ConfigureNarrativeStore(store)
	defer memory.ConfigureNarrativeStore(nil)
	limits, err := memory.LimitsFromEnv()
	if err != nil {
		return err
	}
	if err = memory.ConfigureLimits(limits); err != nil {
		return err
	}
	server := newTutorServer(&tools.Deps{Store: store, Logger: logger, LocalStdio: true}, defaultMCPToolCallTimeout)
	// The distributed scheduler's durable lease protocol also works on SQLite.
	// Multiple local clients elect one owner per scheduled window.
	scheduler := engine.NewDistributedScheduler(store, logger)
	if err = scheduler.Start(); err != nil {
		return err
	}
	defer scheduler.Stop()
	logger.Info("local MCP ready", "transport", "stdio")
	return server.Run(ctx, &mcp.StdioTransport{})
}
