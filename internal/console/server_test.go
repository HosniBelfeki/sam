package console

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/sam/api"
	"google.golang.org/protobuf/proto"
)

func TestNewServer_OIDCAutoDiscovery(t *testing.T) {
	// 1. Generate a mock RSA key for OIDC signing
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	// 2. Start mock control plane + OIDC server
	var serverURL string
	mux := http.NewServeMux()

	// Mock Control Plane /info endpoint
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		resp := &api.ControlPlaneInfoResponse{
			OidcIssuer: serverURL,
			ClientId:   "mock-console-client",
		}
		data, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	// Mock OIDC Discovery endpoint
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		cfg := map[string]any{
			"issuer":                 serverURL,
			"authorization_endpoint": serverURL + "/auth",
			"token_endpoint":         serverURL + "/token",
			"jwks_uri":               serverURL + "/keys",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	})

	// Mock OIDC JWKS keys endpoint
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		// Minimum empty JWKS to satisfy client discovery
		jwks := map[string]any{
			"keys": []map[string]any{
				{
					"kty": "RSA",
					"alg": "RS256",
					"use": "sig",
					"n":   privateKey.N.String(),
					"e":   "AQAB",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})

	mockSrv := httptest.NewServer(mux)
	defer mockSrv.Close()
	serverURL = mockSrv.URL

	// 3. Instantiate console Server with auto-discovery flags (empty issuer and client ID)
	cfg := Config{
		ControlPlaneURL: serverURL,
		AdminToken:      "test-admin-token",
		StaticDir:       t.TempDir(),
	}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 4. Verify OIDC parameters were discovered and set
	if srv.provider == nil {
		t.Fatal("provider config was not initialized")
	}
	if srv.clientID != "mock-console-client" {
		t.Errorf("expected clientID 'mock-console-client', got '%s'", srv.clientID)
	}
	if srv.provider.Endpoint().AuthURL != serverURL+"/auth" {
		t.Errorf("expected AuthURL '%s', got '%s'", serverURL+"/auth", srv.provider.Endpoint().AuthURL)
	}
}

// TestNewServer_OIDCDiscoveryRetriesTransientFailure guards against a real
// deployment race: if the OIDC issuer (e.g. Dex) is still starting up when
// sam-console boots, discovery must retry instead of permanently disabling
// login for the life of the pod (console's /info reports healthy either way,
// so Kubernetes never restarts it to retry on its own).
func TestNewServer_OIDCDiscoveryRetriesTransientFailure(t *testing.T) {
	var attempts int32
	var serverURL string
	mux := http.NewServeMux()

	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		resp := &api.ControlPlaneInfoResponse{
			OidcIssuer: serverURL,
			ClientId:   "mock-console-client",
		}
		data, err := proto.Marshal(resp)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			http.Error(w, "upstream connect error", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 serverURL,
			"authorization_endpoint": serverURL + "/auth",
			"token_endpoint":         serverURL + "/token",
			"jwks_uri":               serverURL + "/keys",
		})
	})

	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{}})
	})

	mockSrv := httptest.NewServer(mux)
	defer mockSrv.Close()
	serverURL = mockSrv.URL

	srv, err := NewServer(Config{
		ControlPlaneURL: serverURL,
		AdminToken:      "test-admin-token",
		StaticDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}
	if srv.provider == nil {
		t.Fatal("expected OIDC discovery to succeed after transient failures, provider is nil")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected discovery to take 3 attempts, got %d", got)
	}
}

func TestDiscoverProviderWithRetry(t *testing.T) {
	t.Run("succeeds after transient failures", func(t *testing.T) {
		var attempts int32
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()
		issuer := srv.URL

		mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) < 3 {
				http.Error(w, "upstream connect error", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":   issuer,
				"jwks_uri": issuer + "/keys",
			})
		})

		if _, err := discoverProviderWithRetry(context.Background(), issuer, 5, time.Millisecond, 5*time.Millisecond); err != nil {
			t.Fatalf("expected discovery to eventually succeed, got: %v", err)
		}
		if got := atomic.LoadInt32(&attempts); got != 3 {
			t.Errorf("expected 3 attempts, got %d", got)
		}
	})

	t.Run("gives up after max attempts", func(t *testing.T) {
		mux := http.NewServeMux()
		srv := httptest.NewServer(mux)
		defer srv.Close()

		mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "always down", http.StatusServiceUnavailable)
		})

		if _, err := discoverProviderWithRetry(context.Background(), srv.URL, 3, time.Millisecond, 5*time.Millisecond); err == nil {
			t.Fatal("expected discovery to fail after exhausting retries")
		}
	})
}
