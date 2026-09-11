package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeLifecycleResponse(t *testing.T, writer http.ResponseWriter, server *httptest.Server, family string, generation int, access, renewable string, migration bool) {
	t.Helper()
	now := time.Now().UTC()
	response := map[string]any{
		"credentialVersion":   2,
		"familyId":            family,
		"generation":          generation,
		"accessCredential":    access,
		"accessExpiresAt":     now.Add(10 * time.Minute),
		"renewableCredential": renewable,
		"renewableExpiresAt":  now.Add(30 * 24 * time.Hour),
	}
	if migration {
		response["oldCredentialValidUntil"] = now.Add(5 * time.Minute)
		response["renewalUrl"] = server.URL + "/v1/relay/credentials/renew"
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		t.Fatal(err)
	}
}

func TestPairingNegotiatesAndPersistsCredentialLifecycleV2(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/relay/pairing-enrollments" || request.Header.Get("X-Telrad-Credential-Version") != "2" {
			t.Fatalf("unexpected pairing request %s", request.URL.Path)
		}
		now := time.Now().UTC()
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"relayId":"relay-v2","protocolVersion":1,"credentialVersion":2,"familyId":"family-v2","generation":1,"accessCredential":%q,"accessExpiresAt":%q,"renewableCredential":%q,"renewableExpiresAt":%q,"renewalUrl":%q,"pairingUrl":%q,"controlUrl":%q,"dicomUrl":%q,"hl7Url":%q}`,
			testAccessCredential('A'), now.Add(10*time.Minute).Format(time.RFC3339Nano), testRenewableCredential('R'), now.Add(30*24*time.Hour).Format(time.RFC3339Nano), server.URL+"/v1/relay/credentials/renew", server.URL+"/v1/relay/pairing-enrollments", server.URL+"/v1/relay/control", server.URL+"/v1/relay/ingest/dicom", server.URL+"/v1/relay/ingest/hl7")
	}))
	defer server.Close()
	oldFactory := clientFactory
	clientFactory = func(*config) protocolClients {
		return protocolClients{secure: server.Client(), updates: server.Client()}
	}
	t.Cleanup(func() { clientFactory = oldFactory })

	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	cfg := defaultConfig()
	cfg.configPath = configPath
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	cfg.CredentialPath = filepath.Join(directory, "relay-credential.json")
	cfg.credentialPathConfigured = "relay-credential.json"
	initial, _ := jsonMarshalIndent(cfg)
	if err := atomicWriteFile(configPath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	if err := enrollWithPairingToken(context.Background(), cfg, configPath, []byte("synthetic-pairing-token-with-safe-length")); err != nil {
		t.Fatal(err)
	}
	record, err := readCredentialFile(cfg.CredentialPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if record.SchemaVersion != credentialLifecycleSchemaVersion || record.FamilyID != "family-v2" || record.Generation != 1 || record.AccessCredential != testAccessCredential('A') || record.RenewableCredential != testRenewableCredential('R') || record.PendingOperation != nil {
		t.Fatalf("stored lifecycle metadata is invalid: %#v", record)
	}
}

func TestCredentialMigrationRetriesDurableOperation(t *testing.T) {
	legacy := testCredential('L')
	var attempts atomic.Int32
	var mu sync.Mutex
	var operationIDs []string
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/relay/credentials/migrate" || request.Header.Get("Authorization") != "Bearer "+legacy {
			t.Fatal("unexpected migration request")
		}
		var body struct {
			OperationID string `json:"operationId"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || !credentialOperationPattern.MatchString(body.OperationID) {
			t.Fatal("invalid migration operation")
		}
		mu.Lock()
		operationIDs = append(operationIDs, body.OperationID)
		mu.Unlock()
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeLifecycleResponse(t, writer, server, "family-migrated", 1, testAccessCredential('M'), testRenewableCredential('N'), true)
	}))
	defer server.Close()
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	if err := commitCredential(cfg.CredentialPath, credentialFile{SchemaVersion: credentialSchemaVersion, Credential: legacy}); err != nil {
		t.Fatal(err)
	}

	if changed, terminal, err := advanceCredentialLifecycle(context.Background(), cfg, server.Client(), false); err == nil || changed || terminal {
		t.Fatalf("first migration changed=%t terminal=%t error=%v", changed, terminal, err)
	}
	pending, err := readCredentialFile(cfg.CredentialPath, time.Now())
	if err != nil || pending.PendingOperation == nil || pending.PendingOperation.Kind != "migrate" {
		t.Fatalf("pending migration=%#v error=%v", pending.PendingOperation, err)
	}
	if changed, terminal, err := advanceCredentialLifecycle(context.Background(), cfg, server.Client(), false); err != nil || !changed || terminal {
		t.Fatalf("recovered migration changed=%t terminal=%t error=%v", changed, terminal, err)
	}
	if len(operationIDs) != 2 || operationIDs[0] != operationIDs[1] || operationIDs[0] != pending.PendingOperation.ID {
		t.Fatalf("migration operation IDs were not stable")
	}
	stored, err := readCredentialFile(cfg.CredentialPath, time.Now())
	if err != nil || stored.SchemaVersion != credentialLifecycleSchemaVersion || stored.PendingOperation != nil {
		t.Fatalf("stored migration=%#v error=%v", stored, err)
	}
}

func TestConcurrentRenewalCreatesOneGenerationAndAdoptsAfterCommit(t *testing.T) {
	now := time.Now().UTC()
	oldAccess := testAccessCredential('A')
	newAccess := testAccessCredential('B')
	oldRenewable := testRenewableCredential('C')
	newRenewable := testRenewableCredential('D')
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/relay/credentials/renew" || request.Header.Get("Authorization") != "Bearer "+oldRenewable {
			t.Fatal("unexpected renewal request")
		}
		requests.Add(1)
		writeLifecycleResponse(t, writer, server, "family-renew", 2, newAccess, newRenewable, false)
	}))
	defer server.Close()
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.PairingURL = server.URL + "/v1/relay/pairing-enrollments"
	renewalURL := server.URL + "/v1/relay/credentials/renew"
	record := credentialFile{
		SchemaVersion: credentialLifecycleSchemaVersion, CredentialVersion: 2,
		FamilyID: "family-renew", Generation: 1,
		AccessCredential: oldAccess, AccessObtainedAt: now.Add(-9 * time.Minute), AccessExpiresAt: now.Add(time.Minute),
		RenewableCredential: oldRenewable, RenewableExpiresAt: now.Add(24 * time.Hour), RenewalURL: renewalURL,
	}
	if err := commitCredential(cfg.CredentialPath, record); err != nil {
		t.Fatal(err)
	}
	provider, err := newCredentialProvider(cfg.CredentialPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inFlightCredential := provider.Current()
	type result struct {
		changed  bool
		terminal bool
		err      error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			changed, terminal, err := advanceCredentialLifecycle(context.Background(), cfg, server.Client(), false)
			results <- result{changed, terminal, err}
		}()
	}
	changedCount := 0
	for range 2 {
		result := <-results
		if result.err != nil || result.terminal {
			t.Fatalf("renewal result=%#v", result)
		}
		if result.changed {
			changedCount++
		}
	}
	if requests.Load() != 1 || changedCount != 1 {
		t.Fatalf("requests=%d changed=%d", requests.Load(), changedCount)
	}
	if inFlightCredential != oldAccess || provider.Current() != oldAccess {
		t.Fatal("credential changed before the durable replacement was adopted")
	}
	changed := make(chan struct{}, 1)
	status := newRuntimeStatus(filepath.Join(directory, "relay.json"))
	if err := adoptCredentialLifecycle(provider, changed, status); err != nil || provider.Current() != newAccess {
		t.Fatalf("new credential was not adopted: %v", err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("credential adoption did not notify control")
	}
	if inFlightCredential != oldAccess {
		t.Fatal("adoption modified already-started work")
	}
}

func TestCredentialRenewalScheduleIsJitteredBeforeExpiry(t *testing.T) {
	obtained := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	record := credentialFile{
		SchemaVersion:    credentialLifecycleSchemaVersion,
		FamilyID:         "family-schedule",
		AccessObtainedAt: obtained,
		AccessExpiresAt:  obtained.Add(10 * time.Minute),
	}
	renewAt := credentialRenewalTime(record)
	if renewAt.Before(obtained.Add(450*time.Second)) || !renewAt.Before(obtained.Add(510*time.Second)) {
		t.Fatalf("renewal time %s is outside the 75-85%% window", renewAt)
	}
}

func TestControlSessionSurvivesCredentialAdoption(t *testing.T) {
	oldAccess := testCredential('A')
	newAccess := testCredential('B')
	var sessions atomic.Int32
	oldPoll := make(chan struct{}, 1)
	newPoll := make(chan struct{}, 1)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/relay/control/sessions":
			sessions.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(writer, `{"type":"ready","sessionId":"session-1","connectorId":"relay-1","ingestMode":"TEST","transports":{"dicom":{"url":%q,"contentType":"application/dicom"},"hl7":{"url":%q,"contentType":"application/hl7-v2"}}}`,
				server.URL+"/v1/relay/ingest/dicom", server.URL+"/v1/relay/ingest/hl7")
		case request.Method == http.MethodPost && request.URL.Path == "/v1/relay/control/sessions/session-1/poll":
			if request.Header.Get("Authorization") == "Bearer "+oldAccess {
				select {
				case oldPoll <- struct{}{}:
				default:
				}
			} else if request.Header.Get("Authorization") == "Bearer "+newAccess {
				select {
				case newPoll <- struct{}{}:
				default:
				}
			} else {
				t.Fatal("control poll used an unexpected credential")
			}
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodDelete:
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected control request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	directory := t.TempDir()
	cfg := pairedTestConfig(directory)
	cfg.configPath = ""
	cfg.ControlURL = server.URL + "/v1/relay/control"
	cfg.DicomURL = server.URL + "/v1/relay/ingest/dicom"
	cfg.HL7URL = server.URL + "/v1/relay/ingest/hl7"
	if err := commitCredential(cfg.CredentialPath, credentialFile{SchemaVersion: credentialSchemaVersion, Credential: oldAccess}); err != nil {
		t.Fatal(err)
	}
	provider, err := newCredentialProvider(cfg.CredentialPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	changed := make(chan struct{}, 1)
	go func() {
		defer close(done)
		superviseControl(ctx, ctx, cfg, server.Client(), provider, changed, newWorkDrainer(), newRuntimeStatus(filepath.Join(directory, "relay.json")))
	}()
	select {
	case <-oldPoll:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("control did not poll with the original credential")
	}
	if err := commitCredential(cfg.CredentialPath, credentialFile{SchemaVersion: credentialSchemaVersion, Credential: newAccess}); err != nil {
		t.Fatal(err)
	}
	if adopted, err := provider.Reload(time.Now()); err != nil || !adopted {
		t.Fatalf("credential adoption=%t error=%v", adopted, err)
	}
	changed <- struct{}{}
	select {
	case <-newPoll:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("control did not adopt the replacement credential")
	}
	cancel()
	<-done
	if sessions.Load() != 1 {
		t.Fatalf("credential adoption created %d control sessions", sessions.Load())
	}
}
