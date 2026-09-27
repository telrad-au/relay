package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	version      = "dev"
	distribution = "native"
)

const pairingTokenVariable = "TELRAD_RELAY_PAIRING_TOKEN"

func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, errNotReady) {
			os.Exit(1)
		}
		slog.Error("relay failed", "error", err)
		os.Exit(1)
	}
}

func printHelp(out io.Writer) {
	fmt.Fprint(out, `Telrad Relay

Usage:
  telrad                  Show status, including the pairing link while unpaired
  telrad status           Show status
  telrad ready            Exit 0 when the relay is ready, otherwise print why and exit 1
  telrad run              Run the relay in the foreground (the service entry point)
  telrad enroll           Pair using TELRAD_RELAY_PAIRING_TOKEN (containers)
  telrad accept-backlog [--hours N]
                          For N hours (1 to 168, default 72), accept reports for
                          accessions not in the ledger and record them
  telrad accept-backlog --cancel
                          Close the backlog acceptance window
  telrad start            Start the background service
  telrad stop             Stop the background service
  telrad restart          Restart the background service
  telrad version          Print the installed version

Options:
  --config PATH           Use a different relay configuration file
`)
}

func execute(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("telrad", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.Usage = func() { printHelp(out) }
	configPath := flags.String("config", defaultConfigPath(), "path to relay configuration")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	command := "status"
	if flags.NArg() > 0 {
		command = flags.Arg(0)
	}
	if flags.NArg() > 1 && command != "accept-backlog" {
		return fmt.Errorf("%s accepts no arguments", command)
	}
	switch command {
	case "help":
		printHelp(out)
		return nil
	case "version":
		fmt.Fprintln(out, version)
		return nil
	case "start", "stop", "restart":
		if distribution == "docker" {
			return errors.New("manage the Relay container through its container runtime")
		}
		return serviceAction(command)
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if err := validateConfig(cfg); err != nil {
		return fmt.Errorf("invalid relay configuration: %w", err)
	}
	switch command {
	case "ready":
		return checkReady(cfg.StatusAddress, out)
	case "status":
		report, err := fetchStatus(cfg.StatusAddress)
		if err != nil {
			return err
		}
		printStatus(out, report, *configPath)
		return nil
	case "enroll":
		return enrollCommand(cfg, *configPath, out)
	case "accept-backlog":
		return acceptBacklogCommand(cfg, flags.Args()[1:], out)
	case "run":
		return runPlatformService(cfg)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// enrollCommand pairs a container with a token. On a native install pairing is
// interactive through the running service, so the command shows the link.
func enrollCommand(cfg *config, configPath string, out io.Writer) error {
	if distribution != "docker" {
		report, err := fetchStatus(cfg.StatusAddress)
		if err != nil {
			return err
		}
		printStatus(out, report, configPath)
		return nil
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	store, err := openIdentity(cfg)
	if err != nil {
		return err
	}
	if store.paired() {
		fmt.Fprintln(out, "Relay is already paired.")
		return nil
	}
	token, err := consumePairingToken()
	if err != nil {
		return err
	}
	if err := pairWithToken(context.Background(), cfg, store, token); err != nil {
		return err
	}
	fmt.Fprintf(out, "Paired relay %s\n", store.relayID())
	return nil
}

func consumePairingToken() (string, error) {
	value, ok := os.LookupEnv(pairingTokenVariable)
	_ = os.Unsetenv(pairingTokenVariable)
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return "", fmt.Errorf("%s is required for container pairing", pairingTokenVariable)
	}
	return value, nil
}

// runRelay is the service. It pairs if needed, then serves until ctx ends.
func runRelay(ctx context.Context, cfg *config) error {
	// Stop the status server and helpers however runRelay returns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	ledgerStore, err := openLedger(cfg.dataPath(ledgerFileName))
	if err != nil {
		return fmt.Errorf("open ledger: %w", err)
	}
	defer ledgerStore.close()
	store, err := openIdentity(cfg)
	if err != nil {
		return err
	}
	status := newStatusServer(cfg, store, ledgerStore)
	if err := status.start(ctx); err != nil {
		return err
	}
	if !store.paired() {
		if store.expired() {
			slog.Warn("relay certificate has expired; pairing again", "notAfter", store.notAfter().UTC().Format(time.RFC3339))
		}
		if distribution == "docker" {
			token, err := consumePairingToken()
			if err != nil {
				return err
			}
			if err := pairWithToken(ctx, cfg, store, token); err != nil {
				return fmt.Errorf("pairing failed: %w", err)
			}
		} else {
			slog.Info("relay is not paired; waiting for approval", "hint", "run telrad to see the link")
			if err := pairInteractively(ctx, cfg, store, status); err != nil {
				if ctx.Err() != nil {
					slog.Info("shutting down before pairing completed")
					return nil
				}
				return fmt.Errorf("pairing failed: %w", err)
			}
		}
		slog.Info("relay paired", "relayId", store.relayID())
	} else {
		_ = os.Unsetenv(pairingTokenVariable)
	}
	r := &relay{cfg: cfg, store: store, ledger: ledgerStore, status: status}
	dicomListener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.DicomPort)))
	if err != nil {
		return fmt.Errorf("listen for DICOM: %w", err)
	}
	defer dicomListener.Close()
	hl7Listener, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(cfg.HL7Port)))
	if err != nil {
		return fmt.Errorf("listen for HL7: %w", err)
	}
	defer hl7Listener.Close()
	status.setListeners(true, true)
	slog.Info("relay listening", "dicom", dicomListener.Addr().String(), "hl7", hl7Listener.Addr().String())

	serveCtx, cancelServe := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelServe()
	go maintainIdentity(ctx, cfg, store, status)
	// Pickup stops at shutdown after acknowledging any report it has started.
	pickupDone := make(chan struct{})
	go func() { defer close(pickupDone); r.runPickup(ctx) }()
	var acceptors sync.WaitGroup
	acceptors.Add(2)
	go func() {
		defer acceptors.Done()
		r.serveListener(serveCtx, dicomListener, cfg.MaxDicomConnections, "dicom", r.serveDICOM)
	}()
	go func() {
		defer acceptors.Done()
		r.serveListener(serveCtx, hl7Listener, cfg.MaxHL7Connections, "hl7", r.serveHL7)
	}()

	<-ctx.Done()
	status.setListeners(false, false)
	_ = dicomListener.Close()
	_ = hl7Listener.Close()
	// No connection can be added to r.work once both accept loops have returned.
	acceptors.Wait()
	drained := make(chan struct{})
	go func() { r.work.Wait(); <-pickupDone; close(drained) }()
	select {
	case <-drained:
	case <-time.After(serviceDrainTimeout):
		slog.Warn("relay shutdown drain deadline expired")
	}
	cancelServe()
	<-pickupDone
	return nil
}
