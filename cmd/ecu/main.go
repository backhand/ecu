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
	// Blank import for its init(), which registers the "local" provider (runs
	// each disposable desktop as a Docker container ON this control-plane box;
	// selected via ECU_PROVIDER=local).
	_ "github.com/backhand/ecu/internal/provider/local"
	"github.com/backhand/ecu/internal/store"
	"github.com/backhand/ecu/internal/tlsboot"
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

// callbackBaseURL derives the publicly reachable control-plane BASE URL the C7
// bake instance curls (the per-bake token + "/internal/bake/<token>/done" is
// appended by the baker). It mirrors tunnelURL's hostname-vs-listen logic but
// over HTTP(S): https://<hostname> in production (C10 wires the cert), else
// http://<listen> for local/dev. Same public-reachability dependency as the
// tunnel URL — a real cloud instance must be able to reach it.
func callbackBaseURL(cfg *config.Config) string {
	if cfg.Hostname != "" {
		return "https://" + cfg.Hostname
	}
	return "http://" + cfg.Listen
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
		// The configured provider name gates provider-capability-specific behavior
		// (notably rejecting persistence on the local provider). Applies in all
		// modes, so it goes on the base opts.
		controlplane.WithProviderName(cfg.Provider),
		// C9 live watch: the HMAC secret for short-lived view tokens (empty ->
		// random key generated in NewServer, which invalidates tokens across
		// restarts; fine for minutes-long tokens) and the public base URL used to
		// build watch_url. The base derives the same way as the bake callback URL
		// (https://<hostname> in production, http://<listen> in dev) — same
		// public-reachability dependency as the tunnel/bake URLs (C10).
		controlplane.WithSigningKey(cfg.SigningKey),
		controlplane.WithPublicBaseURL(callbackBaseURL(cfg)),
		// C5: the global active-session cap and the reaper apply in BOTH dev and
		// production. In dev mode there is no provider, so reaper teardown is a
		// no-op, but idle/lifetime reaping of dev sessions and the cap are still
		// correct and desirable, so these go on the base opts (not the prod-only
		// block below). OrphanGrace mirrors controlplane.defaultOrphanGrace
		// (unexported, so the literal is repeated here).
		controlplane.WithMaxSessions(cfg.MaxSessions),
		// C8: the persistent-session cap and the persistent reaper lifetimes apply
		// in BOTH dev and production (in dev there is no provider, so snapshot /
		// teardown are no-ops, but the cap + status transitions are still correct),
		// so they go on the base opts alongside the C5 cap/reaper.
		controlplane.WithMaxPersistentSessions(cfg.MaxPersistentSessions),
		controlplane.WithReaperConfig(controlplane.ReaperConfig{
			IdleTimeout:           cfg.IdleTimeout,
			MaxLifetime:           cfg.MaxLifetime,
			ReapInterval:          cfg.ReapInterval,
			OrphanGrace:           2 * time.Minute, // mirrors controlplane.defaultOrphanGrace
			PersistentMaxLifetime: cfg.PersistentMaxLifetime,
			PersistentMaxAge:      cfg.PersistentMaxAge,
		}),
	}

	// Production path only: construct the cloud Provider and the provisioning
	// config. In dev-toolserver mode we provision nothing, so no provider is
	// built or required.
	if cfg.DevToolServer == "" {
		prov, err := provider.New(cfg.Provider, provider.Config{
			Token:          cfg.HCloudToken,
			DefaultType:    cfg.InstanceType,
			DefaultRegion:  cfg.Region,
			ContainerImage: cfg.ContainerImage,
			Width:          0, // renderer/provider defaults to 1280x800
			Height:         0,
		})
		if err != nil {
			return err
		}
		opts = append(opts,
			controlplane.WithProvider(prov),
			controlplane.WithProvisionConfig(controlplane.ProvisionConfig{
				TunnelURL: tunnelURL(cfg),
				// The container (Docker) image sessions run — ECU_CONTAINER_IMAGE,
				// distinct from ECU_IMAGE (the pre-baked snapshot name).
				ImageRef:         cfg.ContainerImage,
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
		// C7 pre-baking: only when ECU_IMAGE (the snapshot NAME) is set. The
		// bake pulls ECU_CONTAINER_IMAGE onto a temp instance and snapshots it
		// under ECU_IMAGE; StartBake (below) either finds an existing snapshot
		// (fast path) or bakes one in the background. Unset ECU_IMAGE -> no bake,
		// sessions cold-boot (unchanged).
		if cfg.Image != "" {
			opts = append(opts, controlplane.WithBakeConfig(controlplane.BakeConfig{
				ImageName:       cfg.Image,
				ContainerImage:  cfg.ContainerImage,
				CallbackBaseURL: callbackBaseURL(cfg),
				InstanceType:    cfg.InstanceType,
				Region:          cfg.Region,
				BaseImage:       cfg.BaseImage,
				Timeout:         cfg.BakeTimeout,
			}))
		}
	}

	srv := controlplane.NewServer(st, cfg.DevToolServer, opts...)
	handler := srv.Handler()

	log.Printf("ecu: control plane starting")
	log.Printf("ecu: listen=%s db=%s provider=%s", cfg.Listen, cfg.DBPath, cfg.Provider)
	if cfg.DevToolServer != "" {
		log.Printf("ecu: DEV tool-server seam enabled (ECU_DEV_TOOLSERVER set); sessions are marked ready immediately")
	} else {
		log.Printf("ecu: sessions start in provisioning; they become ready when an instance agent connects to /agent/connect")
		if cfg.Image != "" {
			log.Printf("ecu: pre-baking enabled (ECU_IMAGE=%q); will use/auto-build a snapshot of container image %q", cfg.Image, cfg.ContainerImage)
		} else {
			log.Printf("ecu: pre-baking disabled (ECU_IMAGE unset); sessions cold-boot from base image %q and pull container image %q", cfg.BaseImage, cfg.ContainerImage)
		}
	}
	if exposeTunnelToken {
		log.Printf("ecu: DEV tunnel-token exposure enabled (ECU_DEV_EXPOSE_TUNNEL_TOKEN=1); POST /sessions returns the tunnel secret — do NOT use in production")
	}
	// C9 live watch: with no ECU_SIGNING_KEY the server uses a random key, so
	// watch tokens minted before a restart stop validating after it. Fine for
	// minutes-long tokens; note it so operators can set a stable key if desired.
	if cfg.SigningKey == "" {
		log.Printf("ecu: ECU_SIGNING_KEY unset; using a random watch-token signing key (watch URLs do not survive a restart)")
	}

	// Graceful lifecycle: cancel ctx on SIGINT/SIGTERM, run the reaper and the
	// HTTP server concurrently, and on shutdown drain in-flight requests with a
	// bounded timeout. Cancelling ctx stops the reaper loop too.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The reaper sweeps idle/lifetime/orphan sessions and destroys leaked
	// instances. It runs in dev mode too (teardown is a no-op without a
	// provider, but idle/lifetime reaping still applies). It runs regardless of
	// TLS mode.
	go srv.RunReaper(ctx)

	// C7 pre-baking: when ECU_IMAGE is set, find an existing snapshot or kick off
	// a background bake. It is a no-op when ECU_IMAGE is unset or no provider is
	// configured (dev mode). The bake runs in the background so the control plane
	// is usable immediately; sessions during a bake cold-boot. ctx is the server
	// lifecycle context so shutdown aborts an in-flight bake (its temp instance is
	// still torn down by the baker's deferred teardown). Runs regardless of TLS
	// mode.
	srv.StartBake(ctx)

	// Serve. Two paths, selected by ECU_TLS:
	//
	//   - "auto": the C10 autocert path (internal/tlsboot). It obtains Let's
	//     Encrypt certificates over the HTTP-01 challenge and serves HTTPS on
	//     :443 with the challenge + an HTTP->HTTPS redirect on :80. This IGNORES
	//     ECU_LISTEN — autocert must bind the well-known ports for HTTP-01
	//     validation. tlsboot.Serve blocks until ctx is cancelled (or a listener
	//     fails) and handles its own graceful shutdown; we return its error so a
	//     fatal (e.g. "auto but no hostname resolvable", or :443 already in use)
	//     exits non-zero.
	//   - "off" (default): plain HTTP on ECU_LISTEN, exactly as before. This is
	//     the dev default AND the production path behind a TLS-terminating
	//     fronting layer — notably the k3s deployment, where the traefik Ingress
	//     terminates TLS and the pod runs ECU_TLS=off (so no privileged :443/:80
	//     bind is needed in-cluster). The agent tunnel (/agent/connect) and live
	//     watch (/sessions/{id}/watch) ride this same handler over WebSocket.
	if tlsboot.Enabled(cfg.TLS) {
		log.Printf("ecu: TLS mode=auto (autocert, HTTP-01); serving HTTPS :443, HTTP-01+redirect :80 (ECU_LISTEN ignored)")
		return tlsboot.Serve(ctx, handler, tlsboot.Options{
			Mode:     cfg.TLS,
			Hostname: cfg.Hostname,
			CacheDir: cfg.TLSCacheDir,
		})
	}

	log.Printf("ecu: TLS mode=off; serving plain HTTP on %s (terminate TLS at a fronting Ingress/LB, or set ECU_TLS=auto for built-in autocert)", cfg.Listen)
	httpServer := &http.Server{Addr: cfg.Listen, Handler: handler}

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
