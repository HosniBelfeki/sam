// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/node"
	"github.com/google/sam/internal/sambox"
	"github.com/google/sam/internal/secrets"
	golog "github.com/ipfs/go-log/v2"
	"github.com/multiformats/go-multiaddr"
	madns "github.com/multiformats/go-multiaddr-dns"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

func init() {
	if dnsServer := os.Getenv("SAM_TEST_DNS_SERVER"); dnsServer != "" {
		customResolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "udp", dnsServer)
			},
		}
		net.DefaultResolver = customResolver
		madns.DefaultResolver, _ = madns.NewResolver(madns.WithDefaultResolver(customResolver))
	}
}

var (
	controlPlaneAddr          string
	jwtFlag                   string
	jwtPathFlag               string
	bootstrapTokenFlag        string
	bootstrapTokenPathFlag    string
	clientIDFlag              string
	clientSecretFlag          string
	clientSecretPathFlag      string
	controlPlanePublicKeyFlag string
	meshFlag                  string
	discoveryIntervalFlag     string
	listenAddrs               []string
	enableRelayFlag           bool
	configFile                string
	oidcIssuerFlag            string
	deviceAuthURLFlag         string
	audienceFlag              string
	dataDirFlag               string
	headlessFlag              bool
	offlineAccessFlag         bool
	logLevelFlag              string
	keyGracePeriodFlag        time.Duration
	allowLoopbackFlag         bool
	monitorBootstrapFlag      time.Duration
	monitorCheckIntervalFlag  time.Duration
	autoRelayMinIntervalFlag  time.Duration
	autoRelayBootDelayFlag    time.Duration
	autoRelayBackoffFlag      time.Duration
	routerConnectTimeoutFlag  time.Duration

	// sam-box specific flags
	udsPathFlag         string
	secretsFileFlag     string
	interceptorsDirFlag string
)

var logger = golog.Logger("sam-box-cli")

// resolveSecretFlag folds a --<name>-path file variant into its value flag
// and warns when the secret was passed on the command line, where it leaks
// via /proc/<pid>/cmdline, shell history, and pod specs.
func resolveSecretFlag(name, value, path string) string {
	if value != "" {
		logger.Warnf("--%s passes a secret on the command line; prefer --%s-path", name, name)
	}
	secret, err := secrets.Resolve(name, value, path)
	if err != nil {
		logger.Fatalf("%v", err)
	}
	return secret
}

// resolveDaemonSecret resolves a secret that lives for the daemon's whole
// lifetime: file (recommended) or environment variable, never a flag value.
func resolveDaemonSecret(name, path, envVar string) string {
	secret, err := secrets.FromPathOrEnv(name, path, envVar)
	if err != nil {
		logger.Fatalf("%v", err)
	}
	return secret
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "sam-box",
		Short: "Sovereign Agent Mesh Secure Gateway Node",
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Start the secure gateway node",
		Run: func(cmd *cobra.Command, args []string) {
			ctx := cmd.Context()
			golog.SetAllLoggers(golog.LevelInfo)
			if logLevelFlag != "" {
				lvl, err := golog.LevelFromString(logLevelFlag)
				if err == nil {
					golog.SetAllLoggers(lvl)
				}
			}

			// Suppress noisy DHT logs
			_ = golog.SetLogLevel("dht", "fatal")
			_ = golog.SetLogLevel("dht/RtRefreshManager", "fatal")

			bootstrapTokenFlag = resolveSecretFlag("bootstrap-token", bootstrapTokenFlag, bootstrapTokenPathFlag)
			clientSecretFlag = resolveDaemonSecret("client-secret", clientSecretPathFlag, "SAM_CLIENT_SECRET")
			if jwtFlag != "" {
				logger.Warn("--jwt passes a secret on the command line; prefer --jwt-path")
			}

			if udsPathFlag == "" {
				logger.Fatal("missing required flag: --uds-path")
			}

			store, err := node.NewStore(resolveDataDir())
			if err != nil {
				logger.Fatalf("Failed to open store: %v", err)
			}
			nodeConfig, err := node.LoadNodeConfig(configFile)
			if err != nil {
				logger.Fatalf("Failed to load node config: %v", err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					logger.Errorf("closing store: %v", err)
				}
			}()

			var controlPlanePubKey ed25519.PublicKey
			var routerAddrs []multiaddr.Multiaddr

			storedPubKey, syncedAddrs, err := node.SyncMeshConfig(ctx, store)
			if err != nil {
				logger.Warnf("Failed to sync mesh config: %v", err)
			}
			if len(storedPubKey) > 0 {
				controlPlanePubKey = storedPubKey
				routerAddrs = syncedAddrs
			}

			if controlPlanePublicKeyFlag != "" {
				pubBytes, err := hex.DecodeString(strings.TrimSpace(controlPlanePublicKeyFlag))
				if err != nil {
					logger.Fatalf("Invalid control plane public key: %v", err)
				}
				controlPlanePubKey = pubBytes
			}

			var meshNode *node.SamNode
			var jwtStr string

			if jwtFlag != "" {
				jwtStr = jwtFlag
			} else if jwtPathFlag != "" {
				data, err := os.ReadFile(jwtPathFlag)
				if err != nil {
					logger.Fatalf("Failed to read JWT file: %v", err)
				}
				jwtStr = strings.TrimSpace(string(data))
			} else if oidcIssuerFlag != "" {
				logger.Info("Discovering OIDC endpoints...")
				dummyNode := &node.SamNode{}
				tokenURL, err := dummyNode.DiscoverTokenURL(ctx, oidcIssuerFlag)
				if err != nil {
					logger.Fatalf("Failed to discover OIDC endpoints: %v", err)
				}
				logger.Info("Fetching JWT via OIDC Client Credentials...")
				jwtStr, err = dummyNode.FetchJWT(ctx, tokenURL, clientIDFlag, clientSecretFlag)
				if err != nil {
					logger.Fatalf("Failed to fetch JWT: %v", err)
				}
			}

			if jwtStr == "" && bootstrapTokenFlag == "" {
				token, _ := store.LoadIdentity()
				if len(token) == 0 {
					displayControlPlane := controlPlaneAddr
					if displayControlPlane == "" {
						if h, err := store.LoadControlPlaneURL(); err == nil && h != "" {
							displayControlPlane = h
						}
					}
					logger.Infof("No identity found. Starting unauthenticated sidecar for enrollment over MCP...")
					unauthSrv, err := node.StartUnauthSidecarServer(displayControlPlane, "127.0.0.1:8080", "", "", "")
					if err != nil {
						logger.Fatalf("Failed to start unauthenticated sidecar: %v", err)
					}
					defer func() {
						_ = unauthSrv.Close()
					}()
					<-ctx.Done()
					return
				}
				logger.Infoln("Using stored identity.")

				if len(controlPlanePubKey) == 0 {
					logger.Fatal("Control plane public key not found in store and not provided. Cannot verify peers.")
				}
				priv := node.GetOrGenerateKey(store)
				meshNode, err = node.NewSamNode(node.Options{
					PrivKey:              priv,
					ControlPlanePubKey:   controlPlanePubKey,
					RouterAddrs:          routerAddrs,
					Store:                store,
					MeshID:               meshFlag,
					DiscoveryInterval:    discoveryIntervalFlag,
					ListenAddrs:          listenAddrs,
					EnableRelay:          enableRelayFlag,
					NodeConfig:           nodeConfig,
					KeyGracePeriod:       keyGracePeriodFlag,
					AllowLoopback:        allowLoopbackFlag,
					MonitorBootstrap:     monitorBootstrapFlag,
					MonitorInterval:      monitorCheckIntervalFlag,
					AutoRelayMinInterval: autoRelayMinIntervalFlag,
					AutoRelayBootDelay:   autoRelayBootDelayFlag,
					AutoRelayBackoff:     autoRelayBackoffFlag,
					RouterConnectTimeout: routerConnectTimeoutFlag,
					RequiredRole:         api.RoleSamBox,
				})
				if err != nil {
					logger.Fatalf("Failed to initialize mesh node: %v", err)
				}
				if err := meshNode.Start(ctx); err != nil {
					logger.Fatalf("Failed to start mesh node: %v", err)
				}
			} else {
				// We have a new JWT (from flag or interactive login), need to enroll
				var initRouterAddrs []multiaddr.Multiaddr
				if !strings.HasPrefix(controlPlaneAddr, "http://") && !strings.HasPrefix(controlPlaneAddr, "https://") {
					ma, err := multiaddr.NewMultiaddr(controlPlaneAddr)
					if err == nil {
						initRouterAddrs = []multiaddr.Multiaddr{ma}
					} else {
						host, port, err := net.SplitHostPort(controlPlaneAddr)
						if err == nil {
							ip := net.ParseIP(host)
							var maddr multiaddr.Multiaddr
							var parseErr error
							if ip != nil {
								maddr, parseErr = multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%s", host, port))
							} else {
								maddr, parseErr = multiaddr.NewMultiaddr(fmt.Sprintf("/dns4/%s/tcp/%s", host, port))
							}
							if parseErr != nil {
								logger.Fatalf("Failed to parse multiaddr: %v", parseErr)
							}
							initRouterAddrs = []multiaddr.Multiaddr{maddr}
						} else {
							logger.Fatalf("Invalid control plane address: %v", err)
						}
					}
				}

				priv := node.GetOrGenerateKey(store)
				meshNode, err = node.NewSamNode(node.Options{
					PrivKey:              priv,
					RouterAddrs:          initRouterAddrs,
					Store:                store,
					MeshID:               meshFlag,
					DiscoveryInterval:    discoveryIntervalFlag,
					ListenAddrs:          listenAddrs,
					EnableRelay:          enableRelayFlag,
					NodeConfig:           nodeConfig,
					KeyGracePeriod:       keyGracePeriodFlag,
					AllowLoopback:        allowLoopbackFlag,
					MonitorBootstrap:     monitorBootstrapFlag,
					MonitorInterval:      monitorCheckIntervalFlag,
					AutoRelayMinInterval: autoRelayMinIntervalFlag,
					AutoRelayBootDelay:   autoRelayBootDelayFlag,
					AutoRelayBackoff:     autoRelayBackoffFlag,
					RouterConnectTimeout: routerConnectTimeoutFlag,
					RequiredRole:         api.RoleSamBox,
				})
				if err != nil {
					logger.Fatalf("Failed to initialize node for enrollment: %v", err)
				}
				if err := meshNode.Start(ctx); err != nil {
					logger.Fatalf("Failed to start node for enrollment: %v", err)
				}

				if bootstrapTokenFlag != "" {
					err = meshNode.EnrollBootstrap(ctx, controlPlaneAddr, bootstrapTokenFlag)
				} else {
					err = meshNode.Enroll(ctx, controlPlaneAddr, jwtStr)
				}
				if err != nil {
					logger.Fatalf("Enrollment failed: %v", err)
				}
				if err := store.SaveControlPlaneURL(controlPlaneAddr); err != nil {
					logger.Warnf("Failed to save control plane URL: %v", err)
				}
				logger.Infoln("Successfully enrolled with control plane!")

				if len(controlPlanePubKey) == 0 {
					if syncPubKey, _, err := node.SyncMeshConfig(ctx, store); err != nil || len(syncPubKey) == 0 {
						logger.Fatal("Control plane public key not found in store after enrollment.")
					}
				}
			}

			// Load secrets config if provided
			secretStore := make(map[string]sambox.SecretConfig)
			if secretsFileFlag != "" {
				s, err := loadSecrets(secretsFileFlag)
				if err != nil {
					logger.Fatalf("Failed to load secrets config: %v", err)
				}
				secretStore = s
				logger.Infof("Loaded %d secrets from %s", len(secretStore), secretsFileFlag)
			}

			// Listen on UDS for Secure Outbound Gateway
			if err := os.Remove(udsPathFlag); err != nil && !os.IsNotExist(err) {
				logger.Fatalf("Failed to remove existing UDS file: %v", err)
			}
			listener, err := net.Listen("unix", udsPathFlag)
			if err != nil {
				logger.Fatalf("Failed to listen on UDS socket: %v", err)
			}
			if err := os.Chmod(udsPathFlag, 0600); err != nil {
				logger.Fatalf("Failed to set permissions on UDS socket: %v", err)
			}
			defer func() {
				_ = listener.Close()
				_ = os.Remove(udsPathFlag)
			}()

			logger.Infof("Secure Gateway listening on UDS %s", udsPathFlag)
			gateway, err := sambox.NewGateway(secretStore, nil, interceptorsDirFlag)
			if err != nil {
				logger.Fatalf("Failed to initialize gateway: %v", err)
			}

			serverErrChan := make(chan error, 1)
			go func() {
				if err := gateway.Serve(listener); err != nil {
					serverErrChan <- err
				}
			}()

			select {
			case <-ctx.Done():
				logger.Info("Shutting down Secure Outbound Gateway...")
				_ = listener.Close()
			case err := <-serverErrChan:
				logger.Fatalf("Secure Outbound Gateway server error: %v", err)
			}
		},
	}

	joinCmd := &cobra.Command{
		Use:   "join",
		Short: "Join the mesh interactively",
		Run: func(cmd *cobra.Command, args []string) {
			ctx := cmd.Context()
			targetControlPlane := controlPlaneAddr
			if targetControlPlane == "" {
				logger.Fatal("join needs a control plane to enroll with: pass --control-plane <url> (e.g. https://bananas.sam-mesh.dev for the public testnet).")
			}
			if !strings.HasPrefix(targetControlPlane, "http://") && !strings.HasPrefix(targetControlPlane, "https://") {
				targetControlPlane = "https://" + targetControlPlane
			}
			targetControlPlane = strings.TrimSuffix(targetControlPlane, "/")

			store, err := node.NewStore(resolveDataDir())
			if err != nil {
				logger.Fatalf("Failed to open store: %v", err)
			}
			nodeConfig, err := node.LoadNodeConfig(configFile)
			if err != nil {
				logger.Fatalf("Failed to load node config: %v", err)
			}
			defer func() {
				if err := store.Close(); err != nil {
					logger.Errorf("closing store: %v", err)
				}
			}()

			dummyNode := &node.SamNode{Store: store}
			fmt.Printf("Discovering control plane info from %s...\n", targetControlPlane)
			controlPlaneInfo, err := node.FetchControlPlaneInfo(ctx, targetControlPlane)
			if err != nil {
				logger.Fatalf("Failed to discover control plane info: %v", err)
			}

			var jwtStr string
			if bootstrapTokenFlag == "" {
				fmt.Printf("OIDC Issuer discovered: %s\n", controlPlaneInfo.OidcIssuer)
				fmt.Printf("Client ID discovered: %s\n", controlPlaneInfo.ClientId)

				logger.Info("Discovering OIDC endpoints...")
				tokenURL, authURL, err := dummyNode.DiscoverEndpoints(ctx, controlPlaneInfo.OidcIssuer)
				if err != nil {
					logger.Fatalf("Failed to discover OIDC endpoints: %v", err)
				}

				jwtStr, err = dummyNode.InteractiveLogin(ctx, authURL, tokenURL, controlPlaneInfo.ClientId, controlPlaneInfo.Audience, offlineAccessFlag, headlessFlag)
				if err != nil {
					logger.Fatalf("Failed to get token: %v", err)
				}
			}

			controlPlaneAddr = targetControlPlane
			var initRouterAddrs []multiaddr.Multiaddr
			if !strings.HasPrefix(controlPlaneAddr, "http://") && !strings.HasPrefix(controlPlaneAddr, "https://") {
				ma, err := multiaddr.NewMultiaddr(controlPlaneAddr)
				if err == nil {
					initRouterAddrs = []multiaddr.Multiaddr{ma}
				} else {
					host, port, err := net.SplitHostPort(controlPlaneAddr)
					if err == nil {
						ip := net.ParseIP(host)
						var maddr multiaddr.Multiaddr
						var parseErr error
						if ip != nil {
							maddr, parseErr = multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%s", host, port))
						} else {
							maddr, parseErr = multiaddr.NewMultiaddr(fmt.Sprintf("/dns4/%s/tcp/%s", host, port))
						}
						if parseErr != nil {
							logger.Fatalf("Failed to parse multiaddr: %v", parseErr)
						}
						initRouterAddrs = []multiaddr.Multiaddr{maddr}
					} else {
						logger.Fatalf("Invalid control plane address: %v", err)
					}
				}
			}

			priv := node.GetOrGenerateKey(store)
			meshNode, err := node.NewSamNode(node.Options{
				PrivKey:              priv,
				RouterAddrs:          initRouterAddrs,
				Store:                store,
				MeshID:               meshFlag,
				DiscoveryInterval:    discoveryIntervalFlag,
				ListenAddrs:          []string{"/ip4/0.0.0.0/udp/0/quic-v1", "/ip4/0.0.0.0/tcp/0"},
				EnableRelay:          enableRelayFlag,
				NodeConfig:           nodeConfig,
				KeyGracePeriod:       keyGracePeriodFlag,
				AllowLoopback:        allowLoopbackFlag,
				MonitorBootstrap:     2 * time.Minute,
				MonitorInterval:      1 * time.Minute,
				AutoRelayMinInterval: 30 * time.Second,
				AutoRelayBootDelay:   0 * time.Second,
				AutoRelayBackoff:     3 * time.Second,
				RouterConnectTimeout: routerConnectTimeoutFlag,
				RequiredRole:         api.RoleSamBox,
			})
			if err != nil {
				logger.Fatalf("Failed to initialize node for enrollment: %v", err)
			}
			if err := meshNode.Start(ctx); err != nil {
				logger.Fatalf("Failed to start node for enrollment: %v", err)
			}

			if bootstrapTokenFlag != "" {
				err = meshNode.EnrollBootstrap(ctx, targetControlPlane, bootstrapTokenFlag)
			} else {
				err = meshNode.Enroll(ctx, targetControlPlane, jwtStr)
			}
			if err != nil {
				logger.Fatalf("Enrollment failed: %v", err)
			}
			if err := store.SaveControlPlaneURL(targetControlPlane); err != nil {
				logger.Warnf("Failed to save control plane URL: %v", err)
			}
			if bootstrapTokenFlag == "" {
				if err := store.SaveOIDCConfig(controlPlaneInfo.OidcIssuer, controlPlaneInfo.ClientId, controlPlaneInfo.Audience); err != nil {
					logger.Warnf("Failed to save OIDC config: %v", err)
				}
			}

			fmt.Println("Successfully joined the Sovereign Agent Mesh!")
		},
	}

	// Register flags
	runCmd.Flags().StringSliceVar(&listenAddrs, "listen", []string{"/ip4/0.0.0.0/udp/5001/quic-v1", "/ip4/0.0.0.0/tcp/5002"}, "libp2p Listen Addrs")
	runCmd.Flags().StringVar(&jwtFlag, "jwt", "", "Pre-fetched JWT token")
	runCmd.Flags().StringVar(&jwtPathFlag, "jwt-path", "", "Path to file containing JWT token")
	runCmd.Flags().StringVar(&bootstrapTokenFlag, "bootstrap-token", "", "Pre-shared bootstrap token for enrollment")
	runCmd.Flags().StringVar(&bootstrapTokenPathFlag, "bootstrap-token-path", "", "Path to file containing the bootstrap token (recommended over --bootstrap-token)")
	runCmd.Flags().StringVar(&clientIDFlag, "client-id", "", "OIDC Client ID for M2M")
	runCmd.Flags().StringVar(&clientSecretPathFlag, "client-secret-path", "", "Path to file containing the OIDC client secret (or env SAM_CLIENT_SECRET)")
	runCmd.Flags().StringVar(&controlPlanePublicKeyFlag, "control-plane-public-key", "", "Control plane public key (32-byte Hex)")
	runCmd.Flags().StringVar(&meshFlag, "mesh", node.DefaultMeshName, "Mesh federation name")
	runCmd.Flags().StringVar(&discoveryIntervalFlag, "discovery-interval", node.DefaultDiscoveryInterval, "Polling interval for DHT discovery")
	runCmd.Flags().DurationVar(&monitorBootstrapFlag, "monitor-bootstrap", 2*time.Minute, "Initial wait before monitoring router connection")
	runCmd.Flags().DurationVar(&monitorCheckIntervalFlag, "monitor-interval", 1*time.Minute, "Interval for checking router connection")
	runCmd.Flags().DurationVar(&autoRelayMinIntervalFlag, "autorelay-min-interval", 30*time.Second, "AutoRelay Min Interval")
	runCmd.Flags().DurationVar(&autoRelayBootDelayFlag, "autorelay-boot-delay", 0*time.Second, "AutoRelay Boot Delay")
	runCmd.Flags().DurationVar(&autoRelayBackoffFlag, "autorelay-backoff", 3*time.Second, "AutoRelay Backoff")
	runCmd.Flags().DurationVar(&routerConnectTimeoutFlag, "router-connect-timeout", node.DefaultRouterConnectTimeout, "Timeout for dialing each router address")
	runCmd.Flags().BoolVar(&enableRelayFlag, "enable-relay", false, "Allow this node to serve as a relay for others")
	runCmd.Flags().StringVar(&logLevelFlag, "log-level", "info", "Log level (debug, info, warn, error)")
	runCmd.Flags().DurationVar(&keyGracePeriodFlag, "key-grace-period", 24*time.Hour, "Key grace period for old keys (e.g. 24h)")
	runCmd.Flags().BoolVar(&allowLoopbackFlag, "allow-loopback", false, "Allow publishing and connecting to loopback/link-local addresses")

	// sam-box specific flags
	runCmd.Flags().StringVarP(&udsPathFlag, "uds-path", "u", "", "Path to the Unix Domain Socket to listen on")
	runCmd.Flags().StringVarP(&secretsFileFlag, "secrets-file", "s", "", "Path to the YAML file containing secrets configuration")
	runCmd.Flags().StringVar(&interceptorsDirFlag, "interceptors-dir", "", "Path to the directory containing precompiled libinterceptor.so binaries")

	joinCmd.Flags().BoolVar(&allowLoopbackFlag, "allow-loopback", false, "Allow publishing and connecting to loopback/link-local addresses")
	joinCmd.Flags().DurationVar(&routerConnectTimeoutFlag, "router-connect-timeout", node.DefaultRouterConnectTimeout, "Timeout for dialing each router address")
	joinCmd.Flags().BoolVar(&offlineAccessFlag, "offline-access", false, "Request OIDC offline access/refresh token for automatic renewal")
	joinCmd.Flags().StringVar(&bootstrapTokenFlag, "bootstrap-token", "", "Pre-shared bootstrap token for enrollment")
	joinCmd.Flags().StringVar(&bootstrapTokenPathFlag, "bootstrap-token-path", "", "Path to file containing the bootstrap token (recommended over --bootstrap-token)")

	rootCmd.PersistentFlags().StringVar(&controlPlaneAddr, "control-plane", "", "Control plane URL")
	rootCmd.PersistentFlags().StringVar(&configFile, "config", node.DefaultConfigFile, "Path to sam-node.yaml configuration file")
	rootCmd.PersistentFlags().StringVar(&oidcIssuerFlag, "oidc-issuer", "", "OIDC Issuer URL")
	rootCmd.PersistentFlags().StringVar(&deviceAuthURLFlag, "device-auth-url", "", "OIDC Device Authorization URL")
	rootCmd.PersistentFlags().StringVar(&audienceFlag, "audience", api.DefaultAudience, "OIDC Audience")
	rootCmd.PersistentFlags().StringVar(&dataDirFlag, "data-dir", "", "Override directory for the agent store (defaults to OS user config dir)")
	rootCmd.PersistentFlags().BoolVar(&headlessFlag, "headless", false, "Force headless out-of-band (OOB) authentication flow")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(joinCmd)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\n[Signal] Received interrupt, shutting down...")
		cancel()
		signal.Stop(sigChan)
	}()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func resolveDataDir() string {
	var dir string
	if dataDirFlag != "" {
		dir = dataDirFlag
	} else {
		d, err := node.GetDefaultDataDir()
		if err != nil {
			logger.Fatalf("Failed to get default data directory: %v", err)
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		logger.Fatalf("Failed to create data directory: %v", err)
	}
	return dir
}

func loadSecrets(path string) (map[string]sambox.SecretConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var store map[string]sambox.SecretConfig
	if err := yaml.Unmarshal(data, &store); err != nil {
		return nil, err
	}
	return store, nil
}
