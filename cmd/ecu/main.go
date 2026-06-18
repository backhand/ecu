// Command ecu is the ECU (Easy Computer Use) control-plane binary.
//
// By default it runs in control-plane mode: it loads configuration, opens the
// embedded store, seeds the bootstrap admin API key, builds the HTTP server,
// and serves the control-plane API on ECU_LISTEN.
//
// The same binary is intended to also run the instance-side tunnel agent under
// the --agent flag; that mode is recognized here but stubbed until Component 3
// implements it.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/backhand/ecu/internal/config"
	"github.com/backhand/ecu/internal/controlplane"
	"github.com/backhand/ecu/internal/store"
)

func main() {
	agentMode := flag.Bool("agent", false, "run the instance-side tunnel agent (Component 3 — not yet implemented)")
	flag.Parse()

	if *agentMode {
		// Component 3 fills this in (reverse WSS+yamux tunnel + registration).
		log.Fatal("agent mode not implemented yet")
	}

	if err := runControlPlane(); err != nil {
		// Fail fast (e.g. missing required config with no TTY) with a clear,
		// non-zero exit.
		log.Printf("ecu: fatal: %v", err)
		os.Exit(1)
	}
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

	srv := controlplane.NewServer(st, cfg.DevToolServer)
	handler := srv.Handler()

	log.Printf("ecu: control plane starting")
	log.Printf("ecu: listen=%s db=%s provider=%s", cfg.Listen, cfg.DBPath, cfg.Provider)
	if cfg.DevToolServer != "" {
		log.Printf("ecu: DEV tool-server seam enabled (ECU_DEV_TOOLSERVER set); sessions are marked ready immediately")
	} else {
		log.Printf("ecu: no provisioning backend wired up yet; new sessions will report status=error (expected for the skeleton)")
	}

	// Plain HTTP on ECU_LISTEN; automatic TLS is Component 10.
	if err := http.ListenAndServe(cfg.Listen, handler); err != nil {
		return err
	}
	return nil
}
