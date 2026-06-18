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
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/backhand/ecu/internal/agent"
	"github.com/backhand/ecu/internal/config"
	"github.com/backhand/ecu/internal/controlplane"
	"github.com/backhand/ecu/internal/provider"
	// Blank import for its init(), which registers the "hetzner" provider with
	// the provider factory. This is OUR package (not the hcloud-go SDK), so the
	// SDK stays confined to internal/provider/hcloud. Importing it here makes
	// provider.New("hetzner", ...) work without the factory importing any SDK.
	_ "github.com/backhand/ecu/internal/provider/hcloud"
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

// defaultContainerImageRef is the cold-boot container image cloud-init runs
// when ECU_IMAGE is unset. ECU_IMAGE is the pre-baked-image NAME field today
// (C7); for a cold boot the instance pulls this container image ref. C7 will
// govern pre-baking; for now this is a placeholder default.
const defaultContainerImageRef = "ghcr.io/backhand/ecu-image:latest"

// tunnelURL derives the publicly reachable tunnel ingress the instance agent
// dials OUT to. With ECU_HOSTNAME set it is wss://<hostname>/agent/connect
// (TLS, the production form — C10 wires the cert). Otherwise it falls back to
// ws://<listen>/agent/connect for local/dev. NOTE: a real cloud instance needs
// a PUBLICLY reachable URL; the ws://<listen> fallback only works when the
// instance can reach that address (i.e. local/dev), which is why production
// deployments must set ECU_HOSTNAME (C10).
func tunnelURL(cfg *config.Config) string {
	if cfg.Hostname != "" {
		return "wss://" + cfg.Hostname + "/agent/connect"
	}
	return "ws://" + cfg.Listen + "/agent/connect"
}

// imageRef returns the container image cloud-init runs: cfg.Image if set, else
// the placeholder default. (cfg.Image is the pre-baked-name field today; this
// wires it through for the container image ref. C7 handles real pre-baking.)
func imageRef(cfg *config.Config) string {
	if cfg.Image != "" {
		return cfg.Image
	}
	return defaultContainerImageRef
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

	opts := []controlplane.ServerOption{
		controlplane.WithExposeTunnelToken(exposeTunnelToken),
		controlplane.WithListenAddr(cfg.Listen),
		// C5: the global active-session cap and the reaper apply in BOTH dev and
		// production. In dev mode there is no provider, so reaper teardown is a
		// no-op, but idle/lifetime reaping of dev sessions and the cap are still
		// correct and desirable, so these go on the base opts (not the prod-only
		// block below). OrphanGrace mirrors controlplane.defaultOrphanGrace
		// (unexported, so the literal is repeated here).
		controlplane.WithMaxSessions(cfg.MaxSessions),
		controlplane.WithReaperConfig(controlplane.ReaperConfig{
			IdleTimeout:  cfg.IdleTimeout,
			MaxLifetime:  cfg.MaxLifetime,
			ReapInterval: cfg.ReapInterval,
			OrphanGrace:  2 * time.Minute, // mirrors controlplane.defaultOrphanGrace
		}),
	}

	// Production path only: construct the cloud Provider and the provisioning
	// config. In dev-toolserver mode we provision nothing, so no provider is
	// built or required.
	if cfg.DevToolServer == "" {
		prov, err := provider.New(cfg.Provider, provider.Config{
			Token:         cfg.HCloudToken,
			DefaultType:   cfg.InstanceType,
			DefaultRegion: cfg.Region,
		})
		if err != nil {
			return err
		}
		opts = append(opts,
			controlplane.WithProvider(prov),
			controlplane.WithProvisionConfig(controlplane.ProvisionConfig{
				TunnelURL:        tunnelURL(cfg),
				ImageRef:         imageRef(cfg),
				AgentBinaryURL:   cfg.AgentBinaryURL,
				InstanceType:     cfg.InstanceType,
				Region:           cfg.Region,
				BaseImage:        cfg.BaseImage,
				ProvisionTimeout: cfg.ProvisionTimeout,
				// WIDTH/HEIGHT default in the cloud-init renderer; left zero
				// here so the renderer's 1280x800 default applies until they
				// become configurable.
			}),
		)
	}

	srv := controlplane.NewServer(st, cfg.DevToolServer, opts...)
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

	// Graceful lifecycle: cancel ctx on SIGINT/SIGTERM, run the reaper and the
	// HTTP server concurrently, and on shutdown drain in-flight requests with a
	// bounded timeout. Cancelling ctx stops the reaper loop too.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Plain HTTP on ECU_LISTEN; automatic TLS is Component 10.
	httpServer := &http.Server{Addr: cfg.Listen, Handler: handler}

	// The reaper sweeps idle/lifetime/orphan sessions and destroys leaked
	// instances. It runs in dev mode too (teardown is a no-op without a
	// provider, but idle/lifetime reaping still applies).
	go srv.RunReaper(ctx)

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()

	select {
	case err := <-serveErr:
		// ListenAndServe always returns a non-nil error; ErrServerClosed is the
		// benign "we shut it down" case.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Printf("ecu: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	return nil
}
