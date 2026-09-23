package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testHostedBindingID = "ec6cac4a-87eb-462f-b1a7-cc9fcf6d54a8"
const testHostedConnectorID = "b9101a81-99f5-43b6-815f-b8bce142496a"
const testHostedVPNID = "26a9a751-2b39-4a93-b6b8-c8ea87442897"
const testHostedRouteID = "a295a53e-73b1-47cf-bc6f-182e7c70c811"
const testHostedLeaseID = "1f1380bd-0568-47b3-8963-f6c1ffb23071"

func validHostedSnapshot() hostedSnapshot {
	now := time.Now().UTC().Truncate(time.Second)
	entry := hostedSnapshotEntry{
		BindingID: testHostedBindingID, Generation: 7, State: "ENABLED",
		ConnectorID: testHostedConnectorID, IPsecConnectionID: testHostedVPNID,
		IngressIdentityIP: "100.96.0.10", VPNGeneration: 19,
		UsableUntil: now.Add(40 * time.Second), IngestMode: "PRODUCTION",
		Limits: hostedLimits{MaxConnections: 8, MaxDicomConnections: 6, MaxHL7Connections: 2},
	}
	entry.Endpoints.ControlURL = "https://ingest.example.invalid/v1/relay/control"
	entry.Endpoints.DicomURL = "https://ingest.example.invalid/v1/relay/ingest/dicom"
	entry.Endpoints.HL7URL = "https://ingest.example.invalid/v1/relay/ingest/hl7"
	entry.Endpoints.RenewalURL = "https://ingest.example.invalid/v1/relay/credentials/renew"
	entry.ReportRoute.RouteID = testHostedRouteID
	entry.ReportRoute.EgressLeaseID = "cd600f41-e132-4be9-a8c4-498c05a6b20c"
	entry.ReportRoute.ReportHost = "100.100.0.42"
	entry.ReportRoute.ReportPort = 2575
	return hostedSnapshot{
		Version: 1, RequestID: "190d2577-923a-42da-b3a3-2df09074c3bd",
		RuntimeID: "d3b3c49e-1d6d-4b37-945b-2cde11d59647",
		BootID:    "05ee9874-a4fd-4ef2-9e24-d6f899d7c978",
		LeaseID:   testHostedLeaseID, Revision: "13", ServerTime: now,
		LeaseExpiresAt: now.Add(time.Minute),
		Limits:         hostedLimits{MaxConnections: 128, MaxDicomConnections: 96, MaxHL7Connections: 32, MaxConcurrentReports: 16},
		Bindings:       []hostedSnapshotEntry{entry},
	}
}

func TestManagedHostedExpiryClosesExistingAndNewClinicSockets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	binding := &hostedBinding{
		ctx: ctx, cancel: cancel,
		status:     newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")),
		validUntil: time.Now().Add(time.Minute), sockets: make(map[net.Conn]struct{}),
	}
	registry := &managedHostedRegistry{bindings: map[string]*hostedBinding{"100.96.0.10": binding}}
	peer := &net.TCPAddr{IP: net.ParseIP("100.96.0.10")}
	clinic, relay := net.Pipe()
	defer clinic.Close()
	if registry.lookup(peer) != binding || !binding.register(relay) {
		t.Fatal("current binding rejected its assigned peer")
	}
	binding.setDeadline(time.Now().Add(-time.Second))
	pruneManagedBindings(registry, time.Now())
	if registry.lookup(peer) != nil || binding.register(relay) {
		t.Fatal("expired binding still accepts clinic work")
	}
	_ = clinic.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clinic.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("expired binding left an active socket: %v", err)
	}
}

func TestManagedHostedSnapshotRejectsIdentityAndAuthorityChanges(t *testing.T) {
	baseline := validHostedSnapshot()
	check := func(snapshot hostedSnapshot) error {
		return validateHostedSnapshot(snapshot, baseline.RequestID, baseline.BootID, baseline.LeaseID, "12")
	}
	if err := check(baseline); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	for name, change := range map[string]func(*hostedSnapshot){
		"request replay":             func(s *hostedSnapshot) { s.RequestID = "2c3c2577-923a-42da-b3a3-2df09074c3bd" },
		"lease replacement":          func(s *hostedSnapshot) { s.LeaseID = "2c3c2577-923a-42da-b3a3-2df09074c3bd" },
		"revision rollback":          func(s *hostedSnapshot) { s.Revision = "11" },
		"deadline beyond lease":      func(s *hostedSnapshot) { s.Bindings[0].UsableUntil = s.LeaseExpiresAt.Add(time.Second) },
		"raw private report address": func(s *hostedSnapshot) { s.Bindings[0].ReportRoute.ReportHost = "10.0.0.999" },
		"unbudgeted reservation":     func(s *hostedSnapshot) { s.Bindings[0].Limits.MaxConnections = 129 },
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := validHostedSnapshot()
			change(&snapshot)
			if err := check(snapshot); err == nil {
				t.Fatal("unsafe hosted snapshot accepted")
			}
		})
	}
}

func TestHostedHeaderTransportSetsSingleContextIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, expected := range map[string]string{
			"X-Telrad-Hosted-Binding":    testHostedBindingID,
			"X-Telrad-Hosted-Generation": "7",
			"X-Telrad-Hosted-Lease":      testHostedLeaseID,
		} {
			if values := r.Header.Values(name); len(values) != 1 || values[0] != expected {
				t.Errorf("%s did not carry the assigned context", name)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := &http.Client{Transport: hostedHeaderTransport{
		base: http.DefaultTransport, bindingID: testHostedBindingID,
		generation: 7, leaseID: testHostedLeaseID,
	}}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Telrad-Hosted-Binding", "forged")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected response: %d", response.StatusCode)
	}
}

func TestManagedHostedConfigurationConflictIsRecognizedAsCompetingBoot(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	_, _, err := fetchHostedSnapshot(
		context.Background(), server.Client(),
		&managedHostedConfig{ManagementURL: server.URL},
		"thr_v1_"+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		validHostedSnapshot().BootID, "", "0",
	)
	if !errors.Is(err, errHostedRuntimeLeaseHeld) {
		t.Fatalf("conflicting boot was not distinguished: %v", err)
	}
}

func TestManagedHostedContainerDoesNotEnterClinicPairing(t *testing.T) {
	previous := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = previous })
	cfg := defaultConfig()
	cfg.HostedRuntime = &managedHostedConfig{
		ManagementURL:            "https://ingest.example.invalid:3443/internal/relay/hosted/configuration",
		ManagementCredentialPath: filepath.Join(t.TempDir(), "management-token"),
		StateDirectory:           filepath.Join(t.TempDir(), "state"),
	}
	if bootstrap, err := pairingBootstrapForRun("run", cfg); err != nil || bootstrap {
		t.Fatalf("managed container entered clinic pairing: bootstrap=%t error=%v", bootstrap, err)
	}
	if err := os.WriteFile(cfg.HostedRuntime.ManagementCredentialPath,
		[]byte("thr_v1_"+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := doctor(cfg); err != nil {
		t.Fatalf("managed doctor required a clinic credential: %v", err)
	}
}
