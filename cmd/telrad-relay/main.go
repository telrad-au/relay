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
  telrad pair [--yes]     Pair this Relay again and print the new pairing link,
                          asking before replacing a current pairing; a container
                          pairs with TELRAD_RELAY_PAIRING_TOKEN
  telrad report-receiver [HOST[:PORT]]
                          Show the clinic report receiver, or set it and restart
                          the service (an IPv6 address with a port is [ADDRESS]:PORT)
  telrad accept-backlog [--hours N]
                          For N hours (1 to 168, default 72), accept reports for
                          accessions not in the ledger and record them
  telrad accept-backlog --cancel
                          Close the backlog acceptance window
  telrad start            Start the background service
  telrad stop             Stop the background service
  telrad restart          Restart the background service
  telrad uninstall [--purge] [--yes]
                          Remove the service and program, keeping the
                          configuration, pairing and ledger; --purge deletes
                          them too
  telrad version          Print the installed version

pair, report-receiver HOST, accept-backlog and uninstall change the
installation: run them as root (sudo) on Linux or from an Administrator
PowerShell on Windows. --yes answers the confirmation question of pair and
uninstall; without a terminal it is required.

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
	args = flags.Args()
	if len(args) > 0 {
		args = args[1:]
	}
	switch command {
	case "accept-backlog", "pair", "report-receiver", "uninstall":
	default:
		if len(args) > 0 {
			return fmt.Errorf("%s accepts no arguments", command)
		}
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
	case "uninstall":
		return uninstallCommand(*configPath, args, out)
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "report-receiver" {
		// Setting the receiver may repair a configuration that is invalid
		// only because of it, so it validates the edited result itself.
		return reportReceiverCommand(cfg, *configPath, args, out)
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
		printStatus(out, report)
		return nil
	case "pair":
		return pairCommand(cfg, args, out)
	case "accept-backlog":
		return acceptBacklogCommand(newOperatorEnv(cfg.StatusAddress, out), cfg, args)
	case "run":
		return runPlatformService(cfg)
	default:
		return fmt.Errorf("unknown command %q", command)
	}
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
