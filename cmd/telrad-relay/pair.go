package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// pairCommand pairs the Relay again.
//
//	pair [--yes]
//
// A native service pairs by link: a current pairing is removed, after
// confirmation, by deleting identity.json only, and the restarted service
// publishes a new link. A container pairs with TELRAD_RELAY_PAIRING_TOKEN and
// replaces a current pairing only after the new identity has been issued.
func pairCommand(cfg *config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	flags.SetOutput(out)
	yes := flags.Bool("yes", false, "replace the current pairing without asking")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("pair accepts only --yes")
	}
	if distribution == "docker" {
		return pairContainer(cfg, *yes, out)
	}
	return pairNative(newOperatorEnv(cfg.StatusAddress, out), cfg, *yes)
}

// describePairing names the stored pairing without anything secret.
func describePairing(store *identityStore) string {
	return fmt.Sprintf("Relay %s, certificate expires %s", store.relayID(), store.notAfter().UTC().Format(time.RFC3339))
}

func pairNative(env *operatorEnv, cfg *config, yes bool) error {
	if err := env.requireElevation("pair"); err != nil {
		return err
	}
	identityPath := cfg.dataPath(identityFileName)
	store, loadErr := openIdentity(cfg)
	switch {
	case loadErr != nil:
		// The service cannot start with an unreadable identity, so replacing
		// it is the way out; it still needs the operator's agreement.
		fmt.Fprintf(env.out, "The stored pairing in %s cannot be used: %v\n", identityPath, loadErr)
	case store.paired():
		fmt.Fprintf(env.out, "This Relay is paired: %s.\n", describePairing(store))
	default:
		return showOrRestartPairing(env)
	}
	ok, err := env.confirm("Pair this Relay again?", yes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(env.out, "Nothing was changed.")
		return nil
	}
	if err := env.service.stop(); err != nil {
		return err
	}
	// Only the identity goes: the ledger and any backlog window stay.
	if err := os.Remove(identityPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w (the service is stopped; start it with telrad start)", identityFileName, err)
	}
	if err := syncDirectory(cfg.DataDir); err != nil {
		return err
	}
	if err := env.service.start(); err != nil {
		return err
	}
	fmt.Fprintf(env.out, "Removed the previous pairing (%s); the accession ledger is kept.\n", identityFileName)
	if loadErr == nil {
		fmt.Fprintf(env.out, "Telrad keeps Relay %s until a company administrator revokes or replaces it in Telrad settings.\n", store.relayID())
	}
	return waitForPairingLink(env)
}

// showOrRestartPairing handles an unpaired Relay without deleting anything: it
// prints the current link, starts a stopped service, and restarts one that
// shows a pairing problem instead of a link.
func showOrRestartPairing(env *operatorEnv) error {
	report, err := env.status()
	switch {
	case err != nil:
		running, runningErr := env.service.running()
		if runningErr != nil {
			return runningErr
		}
		if running {
			// Running but not answering yet, for example while starting.
			return waitForPairingLink(env)
		}
		fmt.Fprintf(env.out, "Starting %s.\n", serviceDisplayName)
		if err := env.service.start(); err != nil {
			return err
		}
	case report.Paired:
		fmt.Fprintf(env.out, "This Relay is paired as %s.\n", report.RelayID)
		return nil
	case report.PairingLink != "":
		printPairingLink(env.out, report)
		return nil
	case report.PairingError != "" || report.RenewalError != "":
		printPairingProblem(env.out, report)
		fmt.Fprintf(env.out, "Restarting %s to request a new pairing link.\n", serviceDisplayName)
		if err := env.service.restart(); err != nil {
			return err
		}
	}
	return waitForPairingLink(env)
}

func waitForPairingLink(env *operatorEnv) error {
	report, ok := env.waitForStatus(func(report *statusReport) bool { return report.Paired || report.PairingLink != "" })
	if ok && report.Paired {
		fmt.Fprintf(env.out, "This Relay is paired as %s.\n", report.RelayID)
		return nil
	}
	if ok {
		printPairingLink(env.out, report)
		return nil
	}
	if report != nil {
		printPairingProblem(env.out, report)
	}
	return fmt.Errorf("no pairing link was published within %s; the service keeps trying, so run telrad later to see the link", env.statusWait)
}

func printPairingLink(out io.Writer, report *statusReport) {
	fmt.Fprintf(out, "\nApprove this Relay in your browser:\n\n  %s\n\n", report.PairingLink)
	printPairingProblem(out, report)
}

func printPairingProblem(out io.Writer, report *statusReport) {
	if report.PairingError != "" {
		fmt.Fprintf(out, "pairing problem: %s\n", report.PairingError)
	}
	if report.RenewalError != "" {
		fmt.Fprintf(out, "certificate problem: %s\n", report.RenewalError)
	}
}

// pairContainer pairs with a token. A paired volume is replaced only with
// --yes, and the stored identity changes only once Telrad has issued the new
// one, so a refused token leaves the current pairing in place.
func pairContainer(cfg *config, yes bool, out io.Writer) error {
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return err
	}
	store, err := openIdentity(cfg)
	if err != nil {
		if !yes {
			return fmt.Errorf("%w; run pair --yes to replace it", err)
		}
		store = newIdentityStore(cfg)
	}
	previous := ""
	if store.paired() {
		if !yes {
			return fmt.Errorf("this Relay is already paired (%s); to replace the pairing, run pair --yes with a new token", describePairing(store))
		}
		previous = store.relayID()
	}
	token, err := consumePairingToken()
	if err != nil {
		return err
	}
	if err := pairWithToken(context.Background(), cfg, store, token); err != nil {
		if previous != "" {
			return fmt.Errorf("%w; the current pairing (Relay %s) is unchanged", err, previous)
		}
		return err
	}
	fmt.Fprintf(out, "Paired relay %s\n", store.relayID())
	if previous != "" {
		fmt.Fprintf(out, "Replaced Relay %s. Telrad keeps it until a company administrator revokes or replaces it in Telrad settings.\n", previous)
		fmt.Fprintln(out, "Restart the Relay container to use the new pairing.")
	}
	return nil
}
