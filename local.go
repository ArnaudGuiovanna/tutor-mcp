package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tutor-mcp/auth"
	"tutor-mcp/db"
	"tutor-mcp/engine"
	"tutor-mcp/memory"
	"tutor-mcp/models"
	"tutor-mcp/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTutorServer(deps *tools.Deps, timeout time.Duration) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "tutor-mcp", Version: mcpVersion()}, nil)
	if timeout <= 0 {
		panic("MCP tool call timeout must be positive")
	}
	configured := *deps
	configured.ToolCallTimeout = timeout
	tools.RegisterTools(server, &configured)
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
	unregisterPoolMetrics, err := db.RegisterStoreDBPools(store)
	if err != nil {
		return err
	}
	defer func() { _ = unregisterPoolMetrics() }()
	hostLLMDemonstrates, err := onOffEnv("ASSESSMENT_HOST_LLM_DEMONSTRATES", true)
	if err != nil {
		return err
	}
	store.SetHostLLMDemonstrationPolicy(hostLLMDemonstrates)
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
	if memory.Enabled() {
		// A local profile imports its own rollback tree, never another profile's
		// default home-directory memory. An explicit legacy root remains supported.
		importRoot := strings.TrimSpace(os.Getenv("TUTOR_MCP_MEMORY_ROOT"))
		if importRoot == "" {
			importRoot = filepath.Join(dir, "memory")
		}
		if _, err := memory.BackfillLocalNarratives(ctx, store, importRoot); err != nil {
			return err
		}
		stopRepair := startNarrativeIndexRepair(ctx, store, logger, true, 30*time.Second)
		defer stopRepair()
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
