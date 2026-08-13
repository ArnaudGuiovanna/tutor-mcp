// Copyright (c) 2026 Arnaud Guiovanna <https://github.com/ArnaudGuiovanna/tutor-mcp>
// SPDX-License-Identifier: MIT

package observability

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTenantPseudonymIsStableBoundedAndOpaque(t *testing.T) {
	first := TenantPseudonym("tenant-sensitive-customer-name")
	second := TenantPseudonym("tenant-sensitive-customer-name")
	if first != second || len(first) != 16 || strings.Contains(first, "tenant") {
		t.Fatalf("tenant pseudonym=%q second=%q", first, second)
	}
	if first == TenantPseudonym("tenant-other") {
		t.Fatal("different tenants collided")
	}
}

func TestHTTPHandlerContinuesW3CTraceContext(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	shutdown, err := Setup(context.Background(), "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	handler := HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if TraceID(r.Context()) != "0af7651916cd43dd8448eb211c80319c" {
			t.Errorf("continued trace ID=%q", TraceID(r.Context()))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "https://mcp.test/ready", nil)
	request.Header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status=%d", response.Code)
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Parent().TraceID().String() != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("ended spans=%d", len(spans))
	}
}
