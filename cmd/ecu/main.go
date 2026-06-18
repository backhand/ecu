// Command ecu is the ECU (Easy Computer Use) control-plane binary.
//
// By default it runs in control-plane mode: it loads configuration, opens the
// embedded store, seeds the bootstrap admin API key, builds the HTTP server,
// and serves the control-plane API on ECU_LISTEN.
//
// The same binary also runs the instance-side tunnel agent under the --agent
// flag (Component 3): it dials OUT to the control plane's /agent/connect
// WebSocket and proxies the multiplexed tool requests to the local tool server.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/backhand/ecu/internal/agent"
	"github.com/backhand/ecu/internal/config"
	"github.com/backhand/ecu/internal/controlplane"
	"github.com/backhand/ecu/internal/store"
)

func main() {
	agentMode := flag.Bool("agent", false, "run the instance-side reverse-tunnel agent")
	// Agent-mode flags. Each defaults to its ECU_AGENT_* env var so either a
	// flag or the environment works (an explicitly set flag overrides env
	// because the flag default IS the env value, and Parse only overwrites it
	// when the flag is present).
	controlPlane := flag.String("control-plane", os.Getenv("ECU_AGENT_CONTROL_PLANE"),
		"agent: control-plane tunnel URL, e.g. ws://host:8080/agent/connect (env ECU_AGENT_CONTROL_PLANE)")
	token := flag.String("token", os.Getenv("ECU_AGENT_TOKEN"),
		"agent: session tunnel token (env ECU_AGENT_TOKEN)")
	toolServer := flag.String("tool-server", agentToolServerDefault(),
		"agent: local tool-server base URL (env ECU_AGENT_TOOL_SERVER; default http://127.0.0.1:8000)")
	flag.Parse()

	if *agentMode {
		if err := runAgent(*controlPlane, *token, *toolServer); err != nil {
			log.Printf("ecu agent: fatal: %v", err)
			os.Exit(1)
		}
		return
	}

	if err := runControlPlane(); err != nil {
		// Fail fast (e.g. missing required config with no TTY) with a clear,
		// non-zero exit.
		log.Printf("ecu: fatal: %v", err)
		os.Exit(1)
	}
}

// agentToolServerDefault returns the tool-server URL default for the agent:
// ECU_AGENT_TOOL_SERVER if set, else the conventional local tool server.
func agentToolServerDefault() string {
	if v := os.Getenv("ECU_AGENT_TOOL_SERVER"); v != "" {
		return v
	}
	return "http://127.0.0.1:8000"
}

// runAgent validates inputs, installs signal handling, and runs the tunnel
// agent until SIGINT/SIGTERM. It returns an error for invalid configuration so
// main can map it to a non-zero exit.
func runAgent(controlPlane, token, toolServer string) error {
	// Context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return agent.Run(ctx, agent.Config{
		ControlPlaneURL: controlPlane,
		Token:           token,
		ToolServer:      toolServer,
	})
}

// runControlPlane wires up and starts the control-plane server. It returns an
// error rather than calling os.Exit so the failure path is explicit and
// testable; main maps a returned error to a non-zero exit.
func runControlPlane() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Seed the bootstrap admin key (create-if-absent; never re-activates a key
	// an operator has disabled).
	if err := st.SeedBootstrapKey(cfg.APIKey); err != nil {
		return err
	}

	// DEV-ONLY: when ECU_DEV_EXPOSE_TUNNEL_TOKEN=1, POST /sessions also returns
	// the per-session tunnel_token and a ws:// tunnel_url so a local agent (or
	// an E2E test) can connect without out-of-band token plumbing. Never enable
	// in production — it discloses the tunnel secret to API clients.
	exposeTunnelToken := os.Getenv("ECU_DEV_EXPOSE_TUNNEL_TOKEN") == "1"

	srv := controlplane.NewServer(st, cfg.DevToolServer,
		controlplane.WithExposeTunnelToken(exposeTunnelToken),
		controlplane.WithListenAddr(cfg.Listen),
	)
	handler := srv.Handler()

	log.Printf("ecu: control plane starting")
	log.Printf("ecu: listen=%s db=%s provider=%s", cfg.Listen, cfg.DBPath, cfg.Provider)
	if cfg.DevToolServer != "" {
		log.Printf("ecu: DEV tool-server seam enabled (ECU_DEV_TOOLSERVER set); sessions are marked ready immediately")
	} else {
		log.Printf("ecu: sessions start in provisioning; they become ready when an instance agent connects to /agent/connect")
	}
	if exposeTunnelToken {
		log.Printf("ecu: DEV tunnel-token exposure enabled (ECU_DEV_EXPOSE_TUNNEL_TOKEN=1); POST /sessions returns the tunnel secret — do NOT use in production")
	}

	// Plain HTTP on ECU_LISTEN; automatic TLS is Component 10.
	if err := http.ListenAndServe(cfg.Listen, handler); err != nil {
		return err
	}
	return nil
}
