package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

// The backlog window is a deliberate clinic action for recovery: while it is
// open, a report whose accessions are not in the ledger is accepted and its
// accessions recorded, so Telrad can replay reports ordered before a ledger
// was lost. It is a file on the data volume, not configuration, so it has no
// environment override and the running service sees it without a restart.
const (
	acceptBacklogFileName     = "accept-backlog.json"
	acceptBacklogDefaultHours = 72
	acceptBacklogMaximumHours = 168
)

type acceptBacklogFile struct {
	Until time.Time `json:"until"`
}

// acceptBacklogUntil returns the end of the open window, or false when none is
// open. The file is read on every call; an expired file is removed.
func acceptBacklogUntil(path string, now time.Time) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return time.Time{}, false
	}
	var window acceptBacklogFile
	if err == nil {
		err = json.Unmarshal(data, &window)
	}
	if err != nil || window.Until.IsZero() {
		slog.Warn("backlog acceptance file is unreadable; ignoring it", "file", acceptBacklogFileName)
		return time.Time{}, false
	}
	if !now.Before(window.Until) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("expired backlog acceptance file could not be removed", "error", err)
		}
		return time.Time{}, false
	}
	return window.Until, true
}

func openAcceptBacklog(path string, hours int, now time.Time) (time.Time, error) {
	until := now.UTC().Add(time.Duration(hours) * time.Hour).Truncate(time.Second)
	data, err := json.Marshal(acceptBacklogFile{Until: until})
	if err != nil {
		return time.Time{}, err
	}
	if err := atomicWriteFile(path, append(data, '\n'), 0600); err != nil {
		return time.Time{}, err
	}
	// Run as root, give the file to the service account that owns the data directory.
	return until, matchDirectoryOwner(path)
}

// acceptBacklogCommand opens or cancels the window:
//
//	accept-backlog [--hours N]
//	accept-backlog --cancel
func acceptBacklogCommand(cfg *config, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("accept-backlog", flag.ContinueOnError)
	flags.SetOutput(out)
	hours := flags.Int("hours", acceptBacklogDefaultHours, "hours to accept reports for accessions not in the ledger (1 to 168)")
	cancel := flags.Bool("cancel", false, "close the window now")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("accept-backlog accepts only --hours or --cancel")
	}
	hoursGiven := false
	flags.Visit(func(f *flag.Flag) { hoursGiven = hoursGiven || f.Name == "hours" })
	path := cfg.dataPath(acceptBacklogFileName)
	if *cancel {
		if hoursGiven {
			return errors.New("use either --hours or --cancel")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Fprintln(out, "Backlog acceptance closed. Reports for accessions not in the ledger are refused.")
		return nil
	}
	if *hours < 1 || *hours > acceptBacklogMaximumHours {
		return fmt.Errorf("--hours must be between 1 and %d", acceptBacklogMaximumHours)
	}
	until, err := openAcceptBacklog(path, *hours, time.Now())
	if err != nil {
		return fmt.Errorf("open backlog acceptance: %w", err)
	}
	fmt.Fprintf(out, "Backlog acceptance open until %s (%d hours).\n", until.Format(time.RFC3339), *hours)
	fmt.Fprintln(out, "Until then, reports for accessions not in the ledger are delivered and their accessions recorded.")
	return nil
}
