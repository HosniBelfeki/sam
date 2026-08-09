package node

import (
	"context"
	"errors"
	"testing"

	"github.com/google/sam/api"
	"github.com/multiformats/go-multiaddr"
)

type mockRouterConnectionManager struct {
	connected          bool
	meshConfigErr      error
	storedAddrs        []string
	controlPlaneURLErr error
	controlPlaneURL    string
	discoverErr        error
	discoverResp       *api.ControlPlaneInfoResponse
	connectP2PErr      error
	connectHTTPReq     bool
	connectHTTPErr     error
	saveConfigErr      error
}

func (m *mockRouterConnectionManager) IsConnected() bool {
	return m.connected
}

func (m *mockRouterConnectionManager) LoadMeshConfig() ([]byte, []string, error) {
	return nil, m.storedAddrs, m.meshConfigErr
}

func (m *mockRouterConnectionManager) ConnectAndAuthWithRouter(ctx context.Context, addr multiaddr.Multiaddr) error {
	if m.connectHTTPReq {
		return m.connectHTTPErr
	}
	return m.connectP2PErr
}

func (m *mockRouterConnectionManager) LoadControlPlaneURL() (string, error) {
	return m.controlPlaneURL, m.controlPlaneURLErr
}

func (m *mockRouterConnectionManager) DiscoverControlPlaneInfo(ctx context.Context, url string) (*api.ControlPlaneInfoResponse, error) {
	return m.discoverResp, m.discoverErr
}

func (m *mockRouterConnectionManager) SaveMeshConfig(pubKey []byte, addrs []string) error {
	return m.saveConfigErr
}

func (m *mockRouterConnectionManager) UpdateRelays(addrs []multiaddr.Multiaddr) {}

func TestCheckRouterConnection(t *testing.T) {
	cases := []struct {
		name       string
		mgr        *mockRouterConnectionManager
		wantStable bool
		wantReconn bool
	}{
		{
			name: "already connected",
			mgr: &mockRouterConnectionManager{
				connected: true,
			},
			wantStable: true,
			wantReconn: false,
		},
		{
			name: "disconnected, reconnects via P2P",
			mgr: &mockRouterConnectionManager{
				connected:     false,
				storedAddrs:   []string{"/ip4/127.0.0.1/tcp/4001"},
				connectP2PErr: nil,
			},
			wantStable: false,
			wantReconn: true,
		},
		{
			name: "disconnected, P2P fails, reconnects via HTTP",
			mgr: &mockRouterConnectionManager{
				connected:       false,
				storedAddrs:     []string{"/ip4/127.0.0.1/tcp/4001"},
				connectP2PErr:   errors.New("p2p failed"),
				controlPlaneURL: "https://cp.example.com",
				discoverResp: &api.ControlPlaneInfoResponse{
					RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4002"},
				},
				connectHTTPReq: true,
				connectHTTPErr: nil,
			},
			wantStable: false,
			wantReconn: true,
		},
		{
			name: "disconnected, all fail",
			mgr: &mockRouterConnectionManager{
				connected:       false,
				storedAddrs:     []string{"/ip4/127.0.0.1/tcp/4001"},
				connectP2PErr:   errors.New("p2p failed"),
				controlPlaneURL: "https://cp.example.com",
				discoverResp: &api.ControlPlaneInfoResponse{
					RouterAddresses: []string{"/ip4/127.0.0.1/tcp/4002"},
				},
				connectHTTPReq: true,
				connectHTTPErr: errors.New("http fallback connect failed"),
			},
			wantStable: false,
			wantReconn: false,
		},
		{
			name: "disconnected, HTTP discovery fails",
			mgr: &mockRouterConnectionManager{
				connected:       false,
				storedAddrs:     []string{"/ip4/127.0.0.1/tcp/4001"},
				connectP2PErr:   errors.New("p2p failed"),
				controlPlaneURL: "https://cp.example.com",
				discoverErr:     errors.New("discovery failed"),
			},
			wantStable: false,
			wantReconn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stable, reconnected := checkRouterConnection(context.Background(), tc.mgr)
			if stable != tc.wantStable {
				t.Errorf("expected stable %v, got %v", tc.wantStable, stable)
			}
			if reconnected != tc.wantReconn {
				t.Errorf("expected reconnected %v, got %v", tc.wantReconn, reconnected)
			}
		})
	}
}
