package idempotency_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/viovy/iazio-harness/internal/idempotency"
)

func TestIdempotencyClient(t *testing.T) {
	var callCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path != "/v1/steps/checksum" {
			http.NotFound(w, r)
			return
		}
		var req idempotency.StepRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(idempotency.StepResponse{
				Recorded:        true,
				AlreadyExecuted: false,
			})
		} else {
			_ = json.NewEncoder(w).Encode(idempotency.StepResponse{
				Recorded:        false,
				AlreadyExecuted: true,
			})
		}
	}))
	defer ts.Close()

	client := idempotency.NewClient(ts.URL, "test-token")
	ctx := context.Background()

	// First execution
	resp1, err := client.CheckOrRecordStep(ctx, idempotency.StepRequest{
		ConversationID: "c1",
		StepIndex:      1,
		Command:        "git push",
	})
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	if !resp1.Recorded || resp1.AlreadyExecuted {
		t.Fatalf("expected recorded=true, already_executed=false, got %+v", resp1)
	}

	// Second execution (resume)
	resp2, err := client.CheckOrRecordStep(ctx, idempotency.StepRequest{
		ConversationID: "c1",
		StepIndex:      1,
		Command:        "git push",
	})
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if resp2.Recorded || !resp2.AlreadyExecuted {
		t.Fatalf("expected recorded=false, already_executed=true, got %+v", resp2)
	}
}

func TestComputeChecksum(t *testing.T) {
	c1 := idempotency.ComputeChecksum("conv1", 1, "test-cmd", "payload")
	c2 := idempotency.ComputeChecksum("conv1", 1, "test-cmd", "payload")
	c3 := idempotency.ComputeChecksum("conv1", 2, "test-cmd", "payload")

	if c1 != c2 {
		t.Errorf("checksums should be deterministic: %s != %s", c1, c2)
	}
	if c1 == c3 {
		t.Errorf("checksums with different step index should differ: %s == %s", c1, c3)
	}
}
