package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"tutor-mcp/db"
	"tutor-mcp/models"
)

func TestSaaSConsumerPostgresRestrictedRuntimeDelivery(t *testing.T) {
	raw, fixture, runtime := tenantDeliveryTestStore(t, "postgres")
	keyring, err := db.NewIntegrationSecretKeyring("key:"+base64.StdEncoding.EncodeToString(make([]byte, 32)), "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*db.Store{fixture, runtime} {
		s.SetIntegrationSecretKeyring(keyring)
		if err := db.ConfigureTenantIntegrationAllowedHosts(s, []string{"hooks.customer.test"}); err != nil {
			t.Fatal(err)
		}
	}
	var role string
	if err := runtime.RawDB().QueryRow(`SELECT current_user`).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`GRANT SELECT,UPDATE ON async_jobs TO ` + pgx.Identifier{role}.Sanitize() + `;GRANT SELECT,INSERT,UPDATE ON integration_deliveries TO ` + pgx.Identifier{role}.Sanitize() + `;GRANT SELECT ON tenant_integrations,tenant_dsar_requests TO ` + pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	scopes, _, err := fixture.ListWorkerTenantScopes(t.Context(), models.WorkerPrincipal{ActorID: "consumer-test"}, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 {
		t.Fatal("expected two tenants for RLS proof", len(scopes))
	}
	for i, scope := range scopes {
		actor := models.Principal{TenantID: scope.TenantID, UserID: fmt.Sprintf("owner-%d", i), MembershipID: fmt.Sprintf("member-%d", i), Roles: []string{models.RoleOwner}, Scopes: []string{models.OAuthScopeLearner}, TokenVersion: 1}
		integration, _, err := fixture.CreateTenantIntegration(t.Context(), actor, "webhook", fmt.Sprintf("https://hooks.customer.test/tenant-%d", i), []string{"test.event"})
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := json.Marshal(models.TenantWebhookJob{IntegrationID: integration.ID, EventID: fmt.Sprintf("event-%d", i), EventType: "test.event", Payload: json.RawMessage(fmt.Sprintf(`{"tenant":%d}`, i))})
		if err := fixture.EnqueueAsyncJob(t.Context(), scope, models.AsyncJob{ID: fmt.Sprintf("restricted-%d", i), TenantID: scope.TenantID, Kind: "tenant_webhook_delivery", IdempotencyKey: fmt.Sprintf("restricted-%d", i), PayloadJSON: string(payload), CreatedAt: time.Now().UTC().Add(-time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	var sends atomic.Int32
	s := NewTenantDistributedScheduler(runtime, quietLogger(), "consumer-test")
	s.integrationClient = &http.Client{Transport: webhookRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := runtime.RawDB().PingContext(ctx); err != nil {
			t.Error("HTTP retained restricted SQL transaction", err)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
		}
		var index int
		if _, err := fmt.Sscanf(req.URL.Path, "/tenant-%d", &index); err != nil {
			t.Error(err)
		}
		var decoded struct {
			Tenant int `json:"tenant"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Error(err)
		}
		// PostgreSQL JSONB may normalize insignificant whitespace.
		if decoded.Tenant != index || req.Header.Get("X-Tutor-Event-ID") != fmt.Sprintf("event-%d", index) {
			t.Error("tenant payload/event crossed integration boundary")
		}
		return webhookResponse(204), nil
	})}
	done := startSaaSTestCallback(t, s)
	select {
	case result := <-done:
		if result.failed() {
			t.Fatal(result)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("restricted PG consumer did not drain")
	}
	if sends.Load() != 2 {
		t.Fatal("tenant jobs missing/duplicated", sends.Load())
	}
	var completed int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM async_jobs WHERE status='completed'`).Scan(&completed); err != nil || completed != 2 {
		t.Fatalf("completed=%d %v", completed, err)
	}
	for _, table := range []string{"async_jobs", "integration_deliveries"} {
		var count int
		if err := runtime.RawDB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unscoped %s visible=%d %v", table, count, err)
		}
	}
}
