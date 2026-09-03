package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProtocolClientRejectsRedirectsAndUpdateClientFollows(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusNoContent) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	clients := newProtocolClients(defaultConfig())
	response, err := clients.secure.Get(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("secure client status=%d", response.StatusCode)
	}
	response, err = clients.updates.Get(redirect.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("update client status=%d", response.StatusCode)
	}
}

func TestAuthorizationReadsCurrentCredentialAtSendTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := commitCredential(path, credentialFile{SchemaVersion: 1, Credential: testCredential('A')}); err != nil {
		t.Fatal(err)
	}
	provider, err := newCredentialProvider(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- request.Header.Get("Authorization")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := newProtocolClients(defaultConfig()).secure
	for _, next := range []byte{'A', 'B'} {
		if next == 'B' {
			if err := commitCredential(path, credentialFile{SchemaVersion: 1, Credential: testCredential(next)}); err != nil {
				t.Fatal(err)
			}
			if changed, err := provider.Reload(time.Now()); err != nil || !changed {
				t.Fatalf("reload=%t %v", changed, err)
			}
		}
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, strings.NewReader("x"))
		addRelayHeaders(req, provider, "application/hl7-v2", strings.Repeat("k", 32))
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if got := <-seen; got != "Bearer "+testCredential(next) {
			t.Fatalf("authorization=%q", got)
		}
	}
}

func TestHL7IdempotencyKeysAreRandomBase64URL(t *testing.T) {
	first, err := randomHL7IdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := randomHL7IdempotencyKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || len(second) != 32 || first == second {
		t.Fatalf("keys %q %q", first, second)
	}
	for _, value := range []string{first, second} {
		if strings.ContainsAny(value, "+/=") {
			t.Fatalf("key is not raw base64url: %q", value)
		}
	}
}

func TestBoundedJSONRejectsOversizeAndUnknownFields(t *testing.T) {
	var result struct {
		Status string `json:"status"`
	}
	if err := decodeBoundedJSON(strings.NewReader(`{"status":"ok","secret":"no"}`), 1024, &result); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := decodeBoundedJSON(io.LimitReader(strings.NewReader(strings.Repeat("x", 200)), 200), 64, &result); err == nil {
		t.Fatal("oversize body accepted")
	}
}

func TestDeviceAuthorizationResponseRejectsSecretsInVerificationURL(t *testing.T) {
	now := time.Now().UTC()
	valid := deviceAuthorizationResponse{
		RequestID:       "request-1",
		DeviceSecret:    strings.Repeat("s", 40),
		VerificationURI: "https://app.example.test/relay-authorization/request-1",
		ExpiresAt:       now.Add(10 * time.Minute),
		IntervalSeconds: 5,
	}
	if !validDeviceAuthorization(valid, now) {
		t.Fatal("valid device authorization was rejected")
	}
	valid.VerificationURI += "?deviceSecret=secret"
	if validDeviceAuthorization(valid, now) {
		t.Fatal("verification URL containing a query was accepted")
	}
	valid.VerificationURI = "https://app.example.test/relay-authorization/" + valid.DeviceSecret
	if validDeviceAuthorization(valid, now) {
		t.Fatal("verification URL containing the device secret was accepted")
	}
}

func TestRuntimeReadyDependsOnFreshIngestNotControl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.json")
	now := time.Now().UTC()
	status := relayRuntimeStatus{State: "ready", UpdatedAt: now, Version: version, ProcessID: 42, IngestReady: true, ControlConnected: false, ReportReturnAvailable: false}
	if err := atomicWriteJSON(runtimeStatusPath(path), status); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntimeReady(path, now); err != nil {
		t.Fatal(err)
	}
	status.AuthenticationAttention = true
	if err := atomicWriteJSON(runtimeStatusPath(path), status); err != nil {
		t.Fatal(err)
	}
	if err := checkRuntimeReady(path, now); err == nil {
		t.Fatal("authentication attention was ready")
	}
}

func TestRetryAfterSupportsSecondsAndHTTPDate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	if got := retryAfterDelay("12", now); got != 12*time.Second {
		t.Fatalf("seconds delay=%s", got)
	}
	if got := retryAfterDelay(now.Add(20*time.Second).Format(http.TimeFormat), now); got != 20*time.Second {
		t.Fatalf("date delay=%s", got)
	}
}

func TestPresentationContextChoosesFirstSupportedTransferSyntax(t *testing.T) {
	value := []byte{3, 0, 0, 0}
	value = append(value, associationItem(0x30, []byte("1.2.840.10008.5.1.4.1.1.2"))...)
	value = append(value, associationItem(0x40, []byte("1.2.3.4.5"))...)
	value = append(value, associationItem(0x40, []byte("1.2.840.10008.1.2.4.90"))...)
	value = append(value, associationItem(0x40, []byte("1.2.840.10008.1.2.1"))...)
	context, err := parsePresentationContext(value)
	if err != nil {
		t.Fatal(err)
	}
	if context.TransferSyntax != "1.2.840.10008.1.2.4.90" {
		t.Fatalf("selected syntax=%s", context.TransferSyntax)
	}
}

func TestPart10FileMetaHasZeroPreambleAndExactGroupLength(t *testing.T) {
	header := buildPart10Header("1.2.840.10008.5.1.4.1.1.2", "1.2.3.4", "1.2.840.10008.1.2.1")
	if !bytes.Equal(header[:128], make([]byte, 128)) || string(header[128:132]) != "DICM" {
		t.Fatal("invalid Part 10 preamble")
	}
	if binary.LittleEndian.Uint16(header[132:134]) != 2 || binary.LittleEndian.Uint16(header[134:136]) != 0 || string(header[136:138]) != "UL" {
		t.Fatal("group length element is invalid")
	}
	if got, want := int(binary.LittleEndian.Uint32(header[140:144])), len(header)-144; got != want {
		t.Fatalf("group length=%d want=%d", got, want)
	}
	if !bytes.Contains(header, []byte(implementationUID)) || !bytes.Contains(header, []byte(implementationName)) {
		t.Fatal("implementation identity missing")
	}
}

func TestDICOMPDUAndCommandLimitsRejectOversizeInput(t *testing.T) {
	header := make([]byte, 6)
	header[0] = 0x04
	binary.BigEndian.PutUint32(header[2:], maxDICOMPDUBytes+1)
	if _, _, err := readDICOMPDU(bytes.NewReader(header)); err == nil {
		t.Fatal("oversize DICOM PDU was accepted")
	}
	if _, err := parseDIMSECommand(make([]byte, maxDIMSECommandBytes+1)); err == nil {
		t.Fatal("oversize DIMSE command was accepted")
	}
}
