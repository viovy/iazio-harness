package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyLiveRelease_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/debian/tools", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"host":                   "debian",
			"supported_capabilities": []string{"user_service_watchdog_v2", "deterministic_resume_4tuple_v1", "agy_conversation_pinning_v1"},
			"version":                "0.2.1",
			"commit_sha":             "sha-debian-test",
			"tools": []map[string]any{
				{
					"name":              "iazio-agent",
					"installed":         true,
					"installation_type": "systemd-user",
					"active_state":      "active",
					"watchdog_enabled":  true,
					"config_status":     "synced",
				},
				{
					"name":              "iazio-harvester",
					"installed":         true,
					"installation_type": "systemd-user",
					"active_state":      "active",
					"watchdog_enabled":  true,
					"config_status":     "synced",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v1/jobs/job-1311", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":                      "job-1311",
			"initial_conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("GET /v1/jobs/job-1313", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":                      "job-1313",
			"parent_job_id":           "job-1311",
			"initial_conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
			"conversation_lineage": []map[string]any{
				{
					"conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
					"role":            "root_initial",
				},
				{
					"conversation_id": "1d446877-5bf9-44eb-9db5-e1c007a9fe31",
					"role":            "legacy_unpinned_secondary",
				},
				{
					"conversation_id": "14de9a1f-5679-4f11-a08f-f32bc7e47663",
					"role":            "legacy_unpinned_secondary",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	res, err := VerifyLiveRelease(context.Background(), server.URL, "test-token")
	if err != nil {
		t.Fatalf("VerifyLiveRelease failed: %v", err)
	}
	if !res.DebianToolsPassed {
		t.Error("expected DebianToolsPassed = true")
	}
	if !res.MacMiniLineagePassed {
		t.Error("expected MacMiniLineagePassed = true")
	}
}

func TestVerifyLiveRelease_MissingDebianCapability(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/debian/tools", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"host":                   "debian",
			"supported_capabilities": []string{"legacy_cap"},
			"tools":                  []map[string]any{},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	_, err := VerifyLiveRelease(context.Background(), server.URL, "")
	if err == nil {
		t.Fatal("expected error for missing debian capabilities, got nil")
	}
}

func TestVerifyLiveRelease_DebianToolInactiveOrMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/debian/tools", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"host":                   "debian",
			"supported_capabilities": []string{"user_service_watchdog_v2", "deterministic_resume_4tuple_v1", "agy_conversation_pinning_v1"},
			"tools": []map[string]any{
				{
					"name":              "iazio-agent",
					"installed":         true,
					"installation_type": "systemd-user",
					"active_state":      "inactive",
					"watchdog_enabled":  true,
					"config_status":     "synced",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	if _, err := VerifyLiveRelease(context.Background(), server.URL, ""); err == nil {
		t.Fatal("expected error for inactive tool, got nil")
	}
}

func TestVerifyLiveRelease_Job1311Mismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/debian/tools", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"host":                   "debian",
			"supported_capabilities": []string{"user_service_watchdog_v2", "deterministic_resume_4tuple_v1", "agy_conversation_pinning_v1"},
			"tools": []map[string]any{
				{"name": "iazio-agent", "installed": true, "installation_type": "systemd-user", "active_state": "active", "watchdog_enabled": true, "config_status": "synced"},
				{"name": "iazio-harvester", "installed": true, "installation_type": "systemd-user", "active_state": "active", "watchdog_enabled": true, "config_status": "synced"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v1/jobs/job-1311", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"id": "job-1311", "initial_conversation_id": "wrong-uuid"}
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	if _, err := VerifyLiveRelease(context.Background(), server.URL, ""); err == nil {
		t.Fatal("expected error for job-1311 initial_conversation mismatch, got nil")
	}
}

func TestVerifyLiveRelease_Job1313Mismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents/debian/tools", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"host":                   "debian",
			"supported_capabilities": []string{"user_service_watchdog_v2", "deterministic_resume_4tuple_v1", "agy_conversation_pinning_v1"},
			"tools": []map[string]any{
				{"name": "iazio-agent", "installed": true, "installation_type": "systemd-user", "active_state": "active", "watchdog_enabled": true, "config_status": "synced"},
				{"name": "iazio-harvester", "installed": true, "installation_type": "systemd-user", "active_state": "active", "watchdog_enabled": true, "config_status": "synced"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v1/jobs/job-1311", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"id": "job-1311", "initial_conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399"}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v1/jobs/job-1313", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"id":                      "job-1313",
			"parent_job_id":           "job-wrong",
			"initial_conversation_id": "aad902c8-bda6-44c7-8a68-c468a1562399",
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	if _, err := VerifyLiveRelease(context.Background(), server.URL, ""); err == nil {
		t.Fatal("expected error for job-1313 parent_job_id mismatch, got nil")
	}
}

func TestHttpGet_Errors(t *testing.T) {
	// 404 error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := httpGet(context.Background(), srv.URL+"/notfound", ""); err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}

	// invalid URL
	if _, err := httpGet(context.Background(), "http://127.0.0.1:1/invalid", ""); err == nil {
		t.Fatal("expected error for invalid connect, got nil")
	}
}
