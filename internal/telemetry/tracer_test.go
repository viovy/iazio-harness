package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHarnessTracerSpanCreationAndPropagation(t *testing.T) {
	exp := NewZipkinExporter("http://localhost:9411")
	tracer := NewTracer("test-harness", exp)

	ctx, span := tracer.StartSpan(context.Background(), "test.harness.run", WithKind("INTERNAL"), WithTag("job.id", "sch-101"))
	if span.TraceID == "" || span.ID == "" {
		t.Fatalf("expected non-empty trace and span IDs, got trace=%s, id=%s", span.TraceID, span.ID)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.com/api", nil)
	InjectHTTP(span, req.Header)

	traceID, spanID := ExtractHTTP(req.Header)
	if traceID != span.TraceID || spanID != span.ID {
		t.Fatalf("expected extracted %s/%s, got %s/%s", span.TraceID, span.ID, traceID, spanID)
	}

	span.AddAnnotation("harness started")
	span.SetTag("status", "success")
	span.End()
}

func TestHarnessZipkinExporterSendSpans(t *testing.T) {
	received := make(chan []ZipkinSpan, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var spans []ZipkinSpan
		_ = json.NewDecoder(r.Body).Decode(&spans)
		received <- spans
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	exp := NewZipkinExporter(ts.URL)
	defer func() { _ = exp.Close() }()

	tracer := NewTracer("test-harness", exp)
	_, span := tracer.StartSpan(context.Background(), "harness.execute", WithTag("repo", "iazio"))
	span.End()

	_ = exp.Flush(context.Background())

	select {
	case spans := <-received:
		if len(spans) == 0 {
			t.Fatalf("expected received spans")
		}
		if spans[0].Name != "harness.execute" {
			t.Fatalf("expected span name harness.execute, got %s", spans[0].Name)
		}
		if spans[0].LocalEndpoint.ServiceName != "test-harness" {
			t.Fatalf("expected service test-harness, got %s", spans[0].LocalEndpoint.ServiceName)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for zipkin exporter payload")
	}
}
