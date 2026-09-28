package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
)

// uninstallPlan lists, for the operator, what uninstall removes and keeps.
type uninstallPlan struct {
	remove []string
	keep   []string
	// purgeHint tells the operator how to delete what was kept by hand.
	purgeHint string
}

// uninstaller is the platform's removal of a native installation.
type uninstaller interface {
	plan(purge bool) uninstallPlan
	remove(purge bool, out io.Writer) error
}

// uninstallCommand removes a native installation.
//
//	uninstall [--purge] [--yes]
//
// By default the configuration and data directory stay, so a reinstall
// resumes with the same pairing, report receiver and ledger. --purge deletes
// them as well.
//
// A dataDir that relay.json moves away from the default is never deleted; it
// is named with the command that deletes it.
func uninstallCommand(configPath string, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	flags.SetOutput(out)
	purge := flags.Bool("purge", false, "also delete the configuration, pairing and accession ledger")
	yes := flags.Bool("yes", false, "remove without asking")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("uninstall accepts only --purge and --yes")
	}
	if distribution == "docker" {
		return errors.New("telrad uninstall is for native installations; remove the Relay container instead, and delete its telrad-relay-data volume only when retiring the Relay")
	}
	// A configuration that does not load changes nothing here: the default
	// paths are removed regardless.
	customDataDir := ""
	if cfg, err := loadConfig(configPath); err == nil && !sameDirectory(cfg.DataDir, defaultDataDir()) {
		customDataDir = cfg.DataDir
	}
	return runUninstall(newOperatorEnv("", out), nativeUninstaller(), *purge, *yes, customDataDir)
}

func runUninstall(env *operatorEnv, platform uninstaller, purge, yes bool, customDataDir string) error {
	if err := env.requireElevation("uninstall"); err != nil {
		return err
	}
	plan := platform.plan(purge)
	fmt.Fprintln(env.out, "This removes:")
	for _, item := range plan.remove {
		fmt.Fprintf(env.out, "  %s\n", item)
	}
	if len(plan.keep) > 0 {
		fmt.Fprintln(env.out, "This keeps, so a reinstall resumes with the same pairing, report receiver and ledger:")
		for _, item := range plan.keep {
			fmt.Fprintf(env.out, "  %s\n", item)
		}
	}
	customNote := ""
	if customDataDir != "" {
		customNote = fmt.Sprintf("%s, the dataDir set in relay.json, holds the pairing and ledger and is left in place. Delete it by hand only when retiring the Relay: %s",
			customDataDir, manualRemoveCommand(customDataDir))
		fmt.Fprintln(env.out, customNote)
	}
	question := "Remove Telrad Relay?"
	if purge {
		question = "Remove Telrad Relay and permanently delete its pairing and accession ledger?"
	}
	ok, err := env.confirm(question, yes)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(env.out, "Nothing was changed.")
		return nil
	}
	if err := platform.remove(purge, env.out); err != nil {
		return err
	}
	fmt.Fprintln(env.out, "Telrad Relay was removed.")
	if customNote != "" {
		fmt.Fprintln(env.out, customNote)
	}
	if purge {
		fmt.Fprintln(env.out, "Ask a company administrator to revoke this Relay in Telrad settings.")
	} else {
		fmt.Fprintln(env.out, "Reinstall to resume with the kept files. If this Relay is being retired, ask a company administrator to revoke it in Telrad settings.")
		if plan.purgeHint != "" {
			fmt.Fprintf(env.out, "To delete the kept files later: %s\n", plan.purgeHint)
		}
	}
	return nil
}
