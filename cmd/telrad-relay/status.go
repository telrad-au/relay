package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pickupReadyWindow = 5 * time.Minute

// reportHostPlaceholder is what the installers write when no report receiver
// was given. It never resolves (RFC 6761), so the service starts and pairs and
// every report is answered AE until the operator sets the real receiver.
const reportHostPlaceholder = "report-receiver.invalid"

// statusReport is what the local status endpoint publishes. Nothing in it is
// clinical or secret; accession counts are the only ledger detail.
type statusReport struct {
	State               string          `json:"state"`
	Version             string          `json:"version"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	Paired              bool            `json:"paired"`
	RelayID             string          `json:"relayId,omitempty"`
	PairingLink         string          `json:"pairingLink,omitempty"`
	PairingError        string          `json:"pairingError,omitempty"`
	CertificateNotAfter *time.Time      `json:"certificateNotAfter,omitempty"`
	RenewalError        string          `json:"renewalError,omitempty"`
	Listeners           listenerStatus  `json:"listeners"`
	Telrad              telradStatus    `json:"telrad"`
	Pickup              pickupStatus    `json:"reportPickup"`
	Reports             reportCounters  `json:"reports"`
	LedgerEntries       int             `json:"ledgerEntries"`
	LedgerError         string          `json:"ledgerError,omitempty"`
	Connections         map[string]int  `json:"activeConnections"`
	RefusedConnections  map[string]int  `json:"refusedConnections"`
	LastTelradFailure   map[string]bool `json:"telradFailure,omitempty"`
	// ReportReceiverConfigured is false while reportHost is empty or the
	// installer placeholder; ReportReceiver (host:port) is then omitted.
	ReportReceiverConfigured bool   `json:"reportReceiverConfigured"`
	ReportReceiver           string `json:"reportReceiver,omitempty"`
	// AcceptBacklogUntil is the end of an open backlog acceptance window, or
	// null when none is open.
	AcceptBacklogUntil *time.Time `json:"acceptBacklogUntil"`
}

type listenerStatus struct {
	Dicom bool `json:"dicom"`
	HL7   bool `json:"hl7"`
}

type telradStatus struct {
	Host           string     `json:"host,omitempty"`
	LastDicom      *time.Time `json:"lastDicomConnection,omitempty"`
	LastHL7        *time.Time `json:"lastHl7Connection,omitempty"`
	LastReportPort *time.Time `json:"lastReportConnection,omitempty"`
}

type pickupStatus struct {
	Connected bool       `json:"connected"`
	Since     *time.Time `json:"since,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type reportCounters struct {
	Delivered int `json:"delivered"`
	Refused   int `json:"refused"`
	Failed    int `json:"failed"`
}

type statusServer struct {
	mu            sync.Mutex
	address       string
	backlogPath   string
	store         *identityStore
	ledger        *ledger
	report        statusReport
	lastTelrad    map[string]time.Time
	failed        map[string]bool
	pickupSince   time.Time
	pickupLastUp  time.Time
	pickupUp      bool
	pickupError   string
	listenerReady listenerStatus
	server        *http.Server
	listener      net.Listener
}

func newStatusServer(cfg *config, store *identityStore, ledgerStore *ledger) *statusServer {
	report := statusReport{Version: version, Connections: map[string]int{"dicom": 0, "hl7": 0}, RefusedConnections: map[string]int{"dicom": 0, "hl7": 0}}
	if reportReceiverConfigured(cfg.ReportHost) {
		report.ReportReceiverConfigured = true
		report.ReportReceiver = net.JoinHostPort(strings.TrimSpace(cfg.ReportHost), strconv.Itoa(cfg.ReportPort))
	}
	return &statusServer{
		address: cfg.StatusAddress, backlogPath: cfg.dataPath(acceptBacklogFileName), store: store, ledger: ledgerStore,
		lastTelrad: make(map[string]time.Time), failed: make(map[string]bool),
		report: report,
	}
}

// reportReceiverConfigured reports whether reportHost names a real receiver
// rather than the installer placeholder.
func reportReceiverConfigured(host string) bool {
	host = strings.TrimSpace(host)
	return host != "" && !strings.EqualFold(strings.TrimSuffix(host, "."), reportHostPlaceholder)
}

func (s *statusServer) start(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen for status: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /readyz", s.handleReady)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	s.listener = listener
	go func() { _ = s.server.Serve(listener) }()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()
	return nil
}

func (s *statusServer) snapshot() statusReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := s.report
	report.UpdatedAt = time.Now().UTC()
	report.Paired = s.store != nil && s.store.paired()
	if s.store != nil && s.store.relayID() != "" {
		// An expired identity is shown too, so status explains why pairing is needed.
		report.RelayID = s.store.relayID()
		report.CertificateNotAfter = timePointer(s.store.notAfter())
		report.Telrad.Host = s.store.endpoints().Host
		if s.store.expired() {
			report.RenewalError = certificateExpiredText
		}
	}
	if s.ledger != nil {
		report.LedgerEntries = s.ledger.count()
	}
	if until, open := acceptBacklogUntil(s.backlogPath, time.Now()); open {
		report.AcceptBacklogUntil = timePointer(until)
	}
	report.Listeners = s.listenerReady
	report.Telrad.LastDicom = timePointer(s.lastTelrad["dicom"])
	report.Telrad.LastHL7 = timePointer(s.lastTelrad["hl7"])
	report.Telrad.LastReportPort = timePointer(s.lastTelrad["report"])
	report.Pickup = pickupStatus{Connected: s.pickupUp, Since: timePointer(s.pickupSince), Error: s.pickupError}
	report.Connections = copyCounts(s.report.Connections)
	report.RefusedConnections = copyCounts(s.report.RefusedConnections)
	if len(s.failed) > 0 {
		report.LastTelradFailure = make(map[string]bool, len(s.failed))
		for key, value := range s.failed {
			if value {
				report.LastTelradFailure[key] = true
			}
		}
		if len(report.LastTelradFailure) == 0 {
			report.LastTelradFailure = nil
		}
	}
	switch {
	case !report.Paired:
		report.State = "pairing"
	case s.ready(time.Now()):
		report.State = "ready"
	default:
		report.State = "degraded"
	}
	return report
}

// ready requires pairing, both listeners and a report pickup connection that
// was live within the last five minutes.
func (s *statusServer) ready(now time.Time) bool { return s.notReadyReason(now) == "" }

// notReadyReason names the first unmet readiness condition, or "" when ready.
func (s *statusServer) notReadyReason(now time.Time) string {
	switch {
	case s.store == nil || !s.store.paired():
		return "not paired"
	case !s.listenerReady.Dicom || !s.listenerReady.HL7:
		return "listeners not bound"
	case !s.pickupUp && (s.pickupLastUp.IsZero() || now.Sub(s.pickupLastUp) >= pickupReadyWindow):
		return "report pickup not connected"
	}
	return ""
}

func (s *statusServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.snapshot())
}

type readiness struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

func (s *statusServer) handleReady(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	reason := s.notReadyReason(time.Now())
	s.mu.Unlock()
	code := http.StatusServiceUnavailable
	if reason == "" {
		code = http.StatusOK
	}
	writeJSON(w, code, readiness{Ready: reason == "", Reason: reason})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *statusServer) setPairing(link, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.PairingLink, s.report.PairingError = link, problem
}

func (s *statusServer) setRenewalError(problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.RenewalError = problem
}

func (s *statusServer) setListeners(dicom, hl7 bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listenerReady = listenerStatus{Dicom: dicom, HL7: hl7}
}

func (s *statusServer) telradConnected(purpose string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastTelrad[purpose] = time.Now().UTC()
	s.failed[purpose] = false
}

func (s *statusServer) telradFailure(purpose string, _ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[purpose] = true
}

func (s *statusServer) pickupState(connected bool, problem string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if connected && !s.pickupUp {
		s.pickupSince = now
	}
	if connected {
		s.pickupLastUp = now
	} else if s.pickupUp {
		s.pickupLastUp = now
		s.pickupSince = time.Time{}
	}
	s.pickupUp, s.pickupError = connected, problem
}

func (s *statusServer) connectionOpened(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.Connections[protocol]++
}

func (s *statusServer) connectionClosed(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.Connections[protocol]--
}

func (s *statusServer) connectionRefused(protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.RefusedConnections[protocol]++
}

func (s *statusServer) reportDelivered() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.Reports.Delivered++
}

func (s *statusServer) reportRefused() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.Reports.Refused++
}

func (s *statusServer) reportFailed(error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.Reports.Failed++
}

func (s *statusServer) ledgerFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.LedgerError = err.Error()
}

func (s *statusServer) ledgerUpdated(int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.report.LedgerError = ""
}

func timePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func copyCounts(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var errNotReady = errors.New("relay is not ready")

// checkReady is the CLI health check: it prints "ready", or the readiness
// JSON explaining why not, and returns errNotReady unless /readyz answered 200.
func checkReady(address string, out io.Writer) error {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address + "/readyz")
	if err != nil {
		fmt.Fprintln(out, `{"ready":false,"reason":"status endpoint unreachable"}`)
		return errNotReady
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(out, strings.TrimSpace(string(body)))
		return errNotReady
	}
	fmt.Fprintln(out, "ready")
	return nil
}

// fetchStatus is the CLI side: it reads the running service's status endpoint.
func fetchStatus(address string) (*statusReport, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + address + "/status")
	if err != nil {
		return nil, errors.New("the Relay service is not running or its status endpoint is unreachable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, errors.New("the Relay service returned an invalid status")
	}
	var report statusReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, errors.New("the Relay service returned an invalid status")
	}
	return &report, nil
}

// printStatus renders a status report.
func printStatus(out io.Writer, report *statusReport) {
	fmt.Fprintf(out, "Telrad Relay %s\n", report.Version)
	fmt.Fprintf(out, "state: %s\n", report.State)
	switch {
	case report.ReportReceiverConfigured:
		fmt.Fprintf(out, "report receiver: %s\n", report.ReportReceiver)
	case distribution == "docker":
		fmt.Fprintln(out, "report receiver: NOT CONFIGURED - set TELRAD_RELAY_REPORT_HOST and recreate the container")
	default:
		fmt.Fprintf(out, "report receiver: NOT CONFIGURED - set it with: %s\n", reportReceiverUsage)
	}
	if report.AcceptBacklogUntil != nil {
		fmt.Fprintf(out, "backlog acceptance: open until %s (reports for accessions not in the ledger are accepted)\n", report.AcceptBacklogUntil.Format(time.RFC3339))
	}
	if !report.Paired {
		if report.PairingLink != "" {
			fmt.Fprintf(out, "\nApprove this Relay in your browser:\n\n  %s\n\n", report.PairingLink)
		}
		if report.PairingError != "" {
			fmt.Fprintf(out, "pairing problem: %s\n", report.PairingError)
		}
		if report.RenewalError != "" {
			fmt.Fprintf(out, "certificate problem: %s\n", report.RenewalError)
		}
		return
	}
	fmt.Fprintf(out, "relay: %s\n", report.RelayID)
	if report.CertificateNotAfter != nil {
		fmt.Fprintf(out, "certificate expires: %s\n", report.CertificateNotAfter.Format(time.RFC3339))
	}
	if report.RenewalError != "" {
		fmt.Fprintf(out, "certificate renewal problem: %s\n", report.RenewalError)
	}
	fmt.Fprintf(out, "listeners: dicom=%t hl7=%t\n", report.Listeners.Dicom, report.Listeners.HL7)
	fmt.Fprintf(out, "telrad: %s\n", report.Telrad.Host)
	fmt.Fprintf(out, "report pickup: connected=%t", report.Pickup.Connected)
	if report.Pickup.Error != "" {
		fmt.Fprintf(out, " (%s)", report.Pickup.Error)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "reports: delivered=%d refused=%d failed=%d\n", report.Reports.Delivered, report.Reports.Refused, report.Reports.Failed)
	fmt.Fprintf(out, "ledger entries: %d\n", report.LedgerEntries)
	if report.LedgerError != "" {
		fmt.Fprintf(out, "ledger problem: %s\n", report.LedgerError)
	}
	keys := make([]string, 0, len(report.Connections))
	for key := range report.Connections {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(out, "%s connections: active=%d refused=%d\n", key, report.Connections[key], report.RefusedConnections[key])
	}
}
