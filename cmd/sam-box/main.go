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

// Command sam-box is the sandbox dataplane: one per agent sandbox, serving the
// boundary an agent's traffic leaves through.
//
// It holds no libp2p host, no enrollment and no mesh identity of its own. It
// consumes a local sam-node over that node's API socket and offers the sandbox
// a curated surface: mesh inference and tools addressed by name, plus whatever
// egress policy allows. The node's own API stays on the node's side of the
// boundary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	golog "github.com/ipfs/go-log/v2"
	"github.com/spf13/cobra"

	"github.com/google/sam/api"
	"github.com/google/sam/internal/sambox"
)

var logger = golog.Logger("sam-box")

func main() {
	var (
		sandboxSocket string
		sidecarSocket string
		bundlePath    string
		egressAllow   []string
		logLevel      string
	)

	rootCmd := &cobra.Command{
		Use:   "sam-box",
		Short: "Sovereign Agent Mesh sandbox gateway",
	}

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Serve the sandbox boundary for an agent",
		Long: "Serves SOCKS5 on a sandbox-facing Unix socket, so an unmodified agent reaches\n" +
			"mesh inference and tools by name, and reaches nothing else unless egress policy\n" +
			"allows it.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			golog.SetAllLoggers(golog.LevelInfo)
			if lvl, err := golog.LevelFromString(logLevel); err == nil {
				golog.SetAllLoggers(lvl)
			}

			agentID, egress, err := resolveAgent(bundlePath, egressAllow, cmd.Flags().Changed("egress-allow"))
			if err != nil {
				return err
			}

			listener, err := sambox.ListenSandboxSocket(sandboxSocket)
			if err != nil {
				return err
			}
			defer func() {
				_ = listener.Close()
				_ = os.Remove(sandboxSocket)
			}()

			server := &sambox.SOCKS5Server{
				Dialer: &sambox.AgentDialer{
					Router:        &sambox.Router{Egress: egress},
					SidecarSocket: sidecarSocket,
					AgentID:       agentID,
				},
			}

			logger.Infof("Sandbox boundary listening on %s, node at %s", sandboxSocket, sidecarSocket)
			if agentID == "" {
				logger.Warn("No agent bundle: this sandbox is unidentified, and mesh policy will see only the node it came through")
			} else {
				logger.Infof("Serving agent %s", agentID)
			}
			logger.Infof("Agents reach the mesh at http://%s", api.MeshEntrypointHost)

			if err := server.Serve(cmd.Context(), listener); err != nil {
				return err
			}
			logger.Info("Sandbox boundary stopped")
			return nil
		},
	}

	runCmd.Flags().StringVar(&sandboxSocket, "socket", "", "Path to the sandbox-facing Unix socket to serve SOCKS5 on (required)")
	runCmd.Flags().StringVar(&sidecarSocket, "sidecar-socket", "", "Path to the local sam-node API Unix socket (required)")
	runCmd.Flags().StringVar(&bundlePath, "bundle", "", "Path to the agent bundle declaring the agent's identity and its egress allowance")
	runCmd.Flags().StringSliceVar(&egressAllow, "egress-allow", nil, "Destinations an unidentified sandbox may reach, e.g. api.github.com or *.pypi.org; use --bundle instead where an agent has an identity")
	runCmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	for _, required := range []string{"socket", "sidecar-socket"} {
		if err := runCmd.MarkFlagRequired(required); err != nil {
			panic(err)
		}
	}

	rootCmd.AddCommand(runCmd)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		logger.Fatalf("%v", err)
	}
}

// resolveAgent settles who this boundary serves. A bundle is the real answer;
// --egress-allow covers a sandbox with no identity yet, which mesh policy can
// only attribute to the node it came through. Accepting both would leave the
// egress allowance ambiguous, so it is refused rather than silently resolved.
func resolveAgent(bundlePath string, egressAllow []string, egressSet bool) (string, *sambox.EgressPolicy, error) {
	if bundlePath == "" {
		policy, err := sambox.NewEgressPolicy(egressAllow)
		return "", policy, err
	}
	if egressSet {
		return "", nil, fmt.Errorf("--bundle already declares the egress allowance; drop --egress-allow")
	}

	bundle, err := sambox.LoadAgentBundle(bundlePath)
	if err != nil {
		return "", nil, err
	}
	return bundle.Agent.ID, bundle.EgressPolicy(), nil
}
