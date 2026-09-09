package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

// This is actual binary/PACS interoperability evidence, with a synthetic cloud.
// The separate platform qualification must reconcile receipts using the real API.
func TestRetrievalOrthancBinaryRestart(t *testing.T) {
	if os.Getenv("TELRAD_RETRIEVAL_ORTHANC_TEST") != "1" {
		t.Skip("set TELRAD_RETRIEVAL_ORTHANC_TEST=1 for disposable actual-binary PACS retrieval qualification")
	}
	if runtime.GOOS != "linux" {
		t.Skip("disposable Linux binary harness")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	orthanc := startOrthancContainer(t, ctx, "retrieval", nil, map[string]any{"DicomServerEnabled": true, "DicomAet": "ORTHANC", "DicomPort": 4242, "DicomAlwaysAllowFind": true, "DicomAlwaysAllowGet": true})
	cfg := retrievalTestConfig(t)
	cfg.ListenAddress = "127.0.0.1"
	cfg.DisableDICOMListener = true
	cfg.Retrieval.PACS[0].RequestTimeoutSeconds = 30
	{
		output, err := exec.CommandContext(ctx, "docker", "port", orthanc.name, "4242/tcp").Output()
		if err != nil {
			t.Fatal(err)
		}
		host, port, err := net.SplitHostPort(strings.TrimSpace(string(output)))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Retrieval.PACS[0].Host = host
		cfg.Retrieval.PACS[0].Port, _ = strconv.Atoi(port)
		cfg.Retrieval.PACS[0].CalledAETitle = "ORTHANC"
		cfg.Retrieval.PACS[0].CallingAETitle = "TELRAD"
		cfg.Retrieval.PACS[0].StorageSOPClasses = []string{"1.2.840.10008.5.1.4.1.1.7"}
		cfg.Retrieval.PACS[0].Adapter = "dimse-find-get-v1"
	}
	cfg.HL7Port = reserveRetrievalPort(t)
	bound, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer bound.Close()
	cfg.DicomPort = bound.Addr().(*net.TCPAddr).Port
	if e := atomicWriteJSON(cfg.CredentialPath, credentialFile{SchemaVersion: 1, Credential: testCredential('A')}); e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	phase := 1
	claimed := false
	var permit string
	var public ed25519.PublicKey
	sessions := 0
	uploads := map[int]int{}
	selected := map[int][]string{}
	results := make(chan retrievalResult, 4)
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testCredential('A') {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/relay/control/retrieval-settings":
			json.NewEncoder(w).Encode(retrievalSettings{2, cfg.Retrieval.CompanyID, cfg.RelayID, "PRODUCTION", "RETRIEVE", true})
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		case r.URL.Path == "/v1/relay/signing-keys":
			var body struct{ KeyID, PublicKey string }
			json.NewDecoder(r.Body).Decode(&body)
			key, e := retrieval.DecodeBase64(body.PublicKey, 32)
			if e != nil || permitKeyID(key) != body.KeyID {
				t.Error("invalid registered key")
			}
			public = key
			w.WriteHeader(201)
		case r.URL.Path == "/v1/relay/ingest/referrals":
			var body struct {
				Version int
				HL7     string
				Permits []string
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Version != 2 || len(body.Permits) != 1 {
				t.Error("invalid referral")
				w.WriteHeader(422)
				return
			}
			if _, _, e := retrieval.Verify(body.Permits[0], map[string]ed25519.PublicKey{permitKeyID(public): public}); e != nil {
				t.Error("signature failed")
			}
			permit = body.Permits[0]
			w.Header().Set("Content-Type", "application/hl7-v2")
			io.WriteString(w, "MSH|^~\\&|TELRAD|RIS|SYNTHETIC|CLINIC|20260909000000||ACK|ack|P|2.5\rMSA|AA|message-1\r")
		case r.URL.Path == "/v1/relay/control/sessions":
			sessions++
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(readyMessage{Type: "ready", SessionID: fmt.Sprintf("session-%d", sessions), ConnectorID: cfg.RelayID, IngestMode: "PRODUCTION", Transports: map[string]readyTransport{"dicom": {URL: cfg.DicomURL, ContentType: "application/dicom"}, "hl7": {URL: cfg.HL7URL, ContentType: "application/hl7-v2"}}})
		case strings.HasSuffix(r.URL.Path, "/retrievals/poll"):
			if permit == "" || claimed {
				w.WriteHeader(204)
				return
			}
			claimed = true
			json.NewEncoder(w).Encode(retrievalClaim{Type: "retrieval", JobID: "job", AttemptID: fmt.Sprintf("attempt-%d", phase), Token: "claim-token", ClaimExpiresAt: time.Now().Add(5 * time.Minute), RenewAfterSeconds: 30, Version: 2, ReadinessRule: "PACS_OR_ORDER", Permit: permit})
		case strings.HasSuffix(r.URL.Path, "/poll"):
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/studies"):
			var body struct{ StudyInstanceUIDs []string }
			json.NewDecoder(r.Body).Decode(&body)
			selected[phase] = body.StudyInstanceUIDs
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "studyInstanceUids": body.StudyInstanceUIDs})
		case strings.HasSuffix(r.URL.Path, "/dicom"):
			b, e := io.ReadAll(r.Body)
			if e != nil || len(b) == 0 {
				w.WriteHeader(422)
				return
			}
			if r.Header.Get("X-Telrad-Retrieval-Attempt") != fmt.Sprintf("attempt-%d", phase) {
				t.Error("wrong attempt")
			}
			uploads[phase]++
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"status":"accepted","receiptId":"receipt-%d-%d"}`, phase, uploads[phase])
		case strings.HasSuffix(r.URL.Path, "/result"):
			var body struct{ Result retrievalResult }
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid result")
			}
			results <- body.Result
			io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	setRetrievalCloud(cfg, cloud.URL)
	saveRetrievalTestConfig(t, cfg)
	ca := filepath.Join(t.TempDir(), "roots.pem")
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cloud.Certificate().Raw})
	if e := os.WriteFile(ca, roots, 0600); e != nil {
		t.Fatal(e)
	}
	binaryPath := filepath.Join(t.TempDir(), "telrad")
	build := exec.CommandContext(ctx, "go", "build", "-o", binaryPath, ".")
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build actual Relay: %v: %s", e, out)
	}
	t.Logf("Relay source build; Go %s; PACS %s; adapter %s, completion unsupported", runtime.Version(), orthancInteropImage, "dimse-find-get-v1")
	for run := 1; run <= 2; run++ {
		syntax := explicitLittleEndian
		if run == 2 {
			syntax = implicitLittleEndian
		}
		object := dimseFixture(syntax, "ACC", fmt.Sprintf("1.2.826.0.1.3680043.10.543.99.%d", run), fmt.Sprintf("1.2.826.0.1.3680043.10.543.99.%d.1", run))
		orthanc.importInstance(t, ctx, object)
		mu.Lock()
		phase = run
		claimed = false
		mu.Unlock()
		command := exec.CommandContext(ctx, binaryPath, "--config", cfg.configPath, "run")
		command.Env = append(os.Environ(), "SSL_CERT_FILE="+ca)
		var output bytes.Buffer
		command.Stdout = &output
		command.Stderr = &output
		if e := command.Start(); e != nil {
			t.Fatal(e)
		}
		stopped := false
		stop := func() {
			if !stopped {
				stopped = true
				command.Process.Signal(os.Interrupt)
				command.Wait()
			}
		}
		defer stop()
		if run == 1 {
			conn := waitRetrievalMLLP(t, ctx, cfg.HL7Port)
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			message := bytes.Replace(retrievalTestHL7("NW", 1), []byte("PATIENT^^^CLINIC"), nil, 1)
			conn.Write(append(append([]byte{mllpStart}, message...), mllpEnd, mllpCR))
			frame, e := readMLLPFrame(conn, 65536)
			conn.Close()
			if e != nil {
				stop()
				t.Fatalf("binary MLLP failed: %v; %s", e, output.String())
			}
			code, id, e := parseHL7Acknowledgement(frame[1 : len(frame)-2])
			if e != nil || code != "AA" || id != "message-1" {
				t.Fatal("binary returned wrong ACK")
			}
		}
		select {
		case result := <-results:
			stop()
			if result.Outcome != "uploaded" || len(result.Studies) != run || result.RetrievalMethod != "C_GET" {
				t.Fatalf("binary outcome=%s studies=%d; %s", result.Outcome, len(result.Studies), output.String())
			}
		case <-ctx.Done():
			stop()
			t.Fatalf("binary timed out: %s", output.String())
		}
		mu.Lock()
		if uploads[run] != run || len(selected[run]) != run {
			t.Error("binary omitted matching studies")
		}
		mu.Unlock()
		files, _ := os.ReadDir(filepath.Dir(cfg.configPath))
		for _, f := range files {
			switch f.Name() {
			case "relay.json", "relay-credential.json", permitKeyFilename, "runtime-status.json":
			default:
				t.Fatalf("binary persisted work: %s", f.Name())
			}
		}
	}
}
func reserveRetrievalPort(t *testing.T) int {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
func waitRetrievalMLLP(t *testing.T, ctx context.Context, port int) net.Conn {
	t.Helper()
	for i := 0; i < 100; i++ {
		conn, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
		if e == nil {
			return conn
		}
		if ctx.Err() != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("binary MLLP listener did not start")
	return nil
}
