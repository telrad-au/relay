package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

func retrievalTestConfig(t *testing.T) *config {
	t.Helper()
	cfg := pairedTestConfig(t.TempDir())
	if e := os.Chmod(filepath.Dir(cfg.configPath), 0700); e != nil {
		t.Fatal(e)
	}
	if e := generatePermitKey(cfg.configPath); e != nil {
		t.Fatal(e)
	}
	raw, e := safeReadFile(filepath.Join(filepath.Dir(cfg.configPath), permitKeyFilename), 4096)
	if e != nil {
		t.Fatal(e)
	}
	var key permitKeyFile
	if e := json.Unmarshal(raw, &key); e != nil {
		t.Fatal(e)
	}
	cfg.Retrieval = &retrievalConfig{Enabled: true, CompanyID: "company", ConnectorID: cfg.RelayID, SigningKeyID: key.KeyID, TrustedPublicKeys: []string{key.PublicKey}, PACS: []retrievalPACS{{ID: "pacs", DICOMwebURL: "https://pacs.example.invalid/dicom-web", PatientIssuer: "CLINIC", AccessionIssuer: "CLINIC", MaxStudyBytes: 16 * 1024 * 1024, MaxInstanceBytes: 8 * 1024 * 1024, MaxInstances: 100, RequestTimeoutSeconds: 5, Adapter: "dicomweb-qido-wado-v1"}}, Policies: []referralPolicy{{ID: "policy", PACSID: "pacs", SourceAddresses: []string{"127.0.0.1"}, SendingApplication: "SYNTHETIC", SendingFacility: "CLINIC", OrderControls: []string{"NW", "XO", "CA", "DC"}, AccessionSource: "OBR-18"}}}
	saveRetrievalTestConfig(t, cfg)
	return cfg
}
func saveRetrievalTestConfig(t *testing.T, cfg *config) {
	t.Helper()
	if e := validateConfig(cfg, "run"); e != nil {
		t.Fatal(e)
	}
	if e := atomicWriteJSON(cfg.configPath, cfg); e != nil {
		t.Fatal(e)
	}
}
func retrievalTestHL7(control string, obrs int) []byte {
	s := "MSH|^~\\&|SYNTHETIC|CLINIC|TELRAD|RIS|20260909000000||ORM^O01|message-1|P|2.5\rPID|1||PATIENT^^^CLINIC\rORC|" + control + "||ACC\r"
	for i := 0; i < obrs; i++ {
		fields := make([]string, 19)
		fields[0] = "OBR"
		fields[1] = "set"
		fields[3] = "not-the-accession"
		fields[4] = "CT^Synthetic"
		fields[18] = "ACC"
		s += strings.Join(fields, "|") + "\rZDS|1.2.999^Application^DICOM\r"
	}
	return []byte(s)
}
func testSignedReferral(t *testing.T, cfg *config) string {
	t.Helper()
	permits, qualifies, e := signReferrals(cfg, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12000}, retrievalTestHL7("NW", 1))
	if e != nil || !qualifies || len(permits) != 1 {
		t.Fatalf("sign referral: %v", e)
	}
	return permits[0]
}
func TestRetrievalReferralSourceMappingAndPermanentScope(t *testing.T) {
	cfg := retrievalTestConfig(t)
	peer := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12000}
	for _, version := range []string{"2.3.1", "2.5"} {
		message := bytes.Replace(retrievalTestHL7("NW", 2), []byte("|2.5\r"), []byte("|"+version+"\r"), 1)
		permits, qualifies, e := signReferrals(cfg, peer, message)
		if e != nil || !qualifies || len(permits) != 2 {
			t.Fatalf("multi OBR signing: %v", e)
		}
		for i, envelope := range permits {
			p, _, e := authorizeRetrieval(cfg, envelope)
			if e != nil || p.Examination.Accession != "ACC" || p.Examination.AccessionSource != "OBR-18" || p.Procedure.Sequence != i+1 || p.Procedure.SourceSetID != "set" || p.ClinicCompleted != nil {
				t.Fatalf("wrong scope: %v", e)
			}
		}
	}
	for _, control := range []string{"CA", "DC"} {
		p, q, e := signReferrals(cfg, peer, retrievalTestHL7(control, 2))
		if e != nil || !q || len(p) != 0 {
			t.Fatal("cancellation minted a permit")
		}
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"sender":             func(b []byte) []byte { return bytes.Replace(b, []byte("SYNTHETIC"), []byte("UNAPPROVED"), 1) },
		"unapproved trigger": func(b []byte) []byte { return bytes.Replace(b, []byte("ORC|NW"), []byte("ORC|SC"), 1) },
		"wildcard accession": func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("ACC"), []byte("AC*")) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, e := signReferrals(cfg, peer, mutate(retrievalTestHL7("NW", 1))); e == nil {
				t.Fatal("unapproved referral signed")
			}
		})
	}
	if _, _, e := signReferrals(cfg, &net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 12000}, retrievalTestHL7("NW", 1)); e == nil {
		t.Fatal("unapproved socket signed spoofed MSH")
	}
	report := bytes.Replace(retrievalTestHL7("NW", 1), []byte("ORM^O01"), []byte("ORU^R01"), 1)
	if permits, q, e := signReferrals(cfg, peer, report); e != nil || q || len(permits) != 0 {
		t.Fatal("report entered signing path")
	}
	cfg.Retrieval.Policies[0].AllowMissingPatientIssuer = true
	missing := bytes.Replace(retrievalTestHL7("NW", 1), []byte("PATIENT^^^CLINIC"), []byte("PATIENT"), 1)
	permits, _, e := signReferrals(cfg, peer, missing)
	if e != nil {
		t.Fatal(e)
	}
	p, _, e := authorizeRetrieval(cfg, permits[0])
	if e != nil || p.Patient != nil {
		t.Fatal("new permit retained patient binding")
	}
	cfg.Retrieval.Policies[0].AccessionSource = "OBR-3"
	permits, _, e = signReferrals(cfg, peer, retrievalTestHL7("XO", 1))
	if e != nil {
		t.Fatal(e)
	}
	p, _, e = authorizeRetrieval(cfg, permits[0])
	if e != nil || p.Examination.Accession != "not-the-accession" {
		t.Fatal("explicit mapping ignored")
	}
}
func TestRetrievalRejectsScopeChangesAndKeyReplacement(t *testing.T) {
	cfg := retrievalTestConfig(t)
	envelope := testSignedReferral(t, cfg)
	p, _, e := authorizeRetrieval(cfg, envelope)
	if e != nil {
		t.Fatal(e)
	}
	key, e := readPermitSigningKey(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer zeroBytes(key)
	for name, edit := range map[string]func(*retrieval.Permit){"company": func(p *retrieval.Permit) { p.CompanyID = "other" }, "connector": func(p *retrieval.Permit) { p.ConnectorID = "other" }, "PACS": func(p *retrieval.Permit) { p.PACSID = "other" }, "policy": func(p *retrieval.Permit) { p.SourcePolicyID = "other" }, "patient namespace": func(p *retrieval.Permit) {
		p.Patient = &retrieval.Patient{ID: "PATIENT", Issuer: "OTHER", IssuerSource: "hl7"}
	}, "accession namespace": func(p *retrieval.Permit) { p.Examination.Issuer = "OTHER" }} {
		t.Run(name, func(t *testing.T) {
			copy := p
			edit(&copy)
			signed, e := retrieval.Sign(copy, cfg.Retrieval.SigningKeyID, key)
			if e != nil {
				t.Fatal(e)
			}
			if _, _, e := authorizeRetrieval(cfg, signed); e == nil {
				t.Fatal("foreign scope accepted")
			}
		})
	}
	if e := generatePermitKey(cfg.configPath); e == nil {
		t.Fatal("key generation replaced authority")
	}
	cfg.Retrieval.TrustedPublicKeys = nil
	if _, _, e := authorizeRetrieval(cfg, envelope); e == nil {
		t.Fatal("retired authority accepted")
	}
}
func TestRetrievalMLLPAtomicACKAndIdenticalRetries(t *testing.T) {
	for _, ackCode := range []string{"AA", "AE", "AR"} {
		t.Run(ackCode, func(t *testing.T) {
			cfg := retrievalTestConfig(t)
			provider := testProvider(t, testCredential('A'))
			status := newRuntimeStatus(cfg.configPath)
			ack := []byte("MSH|^~\\&|TELRAD|RIS|SYNTHETIC|CLINIC|20260909000000||ACK|ack-1|P|2.5\rMSA|" + ackCode + "|message-1\r")
			var mu sync.Mutex
			var bodies [][]byte
			var keys []string
			registered := false
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testCredential('A') || len(r.Header.Values("X-Telrad-Protocol-Version")) != 1 || r.Header.Get("X-Telrad-Protocol-Version") != "1" {
					t.Error("invalid authorization headers")
				}
				switch r.URL.Path {
				case "/v1/relay/control/retrieval-settings":
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(retrievalSettings{2, cfg.Retrieval.CompanyID, cfg.RelayID, "PRODUCTION", "RETRIEVE", true})
				case "/v1/relay/signing-keys":
					mu.Lock()
					registered = true
					mu.Unlock()
					w.WriteHeader(201)
				case "/v1/relay/ingest/referrals":
					b, _ := io.ReadAll(r.Body)
					mu.Lock()
					bodies = append(bodies, b)
					keys = append(keys, r.Header.Get("Idempotency-Key"))
					n := len(bodies)
					wasRegistered := registered
					mu.Unlock()
					if !wasRegistered {
						t.Error("key not registered")
					}
					if n == 1 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(503)
						return
					}
					w.Header().Set("Content-Type", "application/hl7-v2")
					w.Write(ack)
				default:
					t.Error("unexpected raw fallback")
					w.WriteHeader(404)
				}
			}))
			defer cloud.Close()
			setRetrievalCloud(cfg, cloud.URL)
			saveRetrievalTestConfig(t, cfg)
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer listener.Close()
			done := make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() {
				defer close(done)
				conn, e := listener.Accept()
				if e == nil {
					defer conn.Close()
					serveHL7(ctx, conn, cfg, cloud.Client(), provider, status)
				}
			}()
			conn, e := net.Dial("tcp", listener.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			message := retrievalTestHL7("NW", 2)
			conn.Write(append(append([]byte{mllpStart}, message...), mllpEnd, mllpCR))
			frame, e := readMLLPFrameFrom(bufio.NewReader(conn), 65536)
			conn.Close()
			<-done
			if e != nil || !bytes.Equal(frame[1:len(frame)-2], ack) {
				t.Fatalf("exact ACK not returned: %v", e)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || keys[0] == "" || keys[0] != keys[1] {
				t.Fatal("retry identity changed")
			}
			var body struct {
				Version int
				HL7     string
				Permits []string
			}
			if json.Unmarshal(bodies[0], &body) != nil || body.Version != 2 || len(body.Permits) != 2 || body.HL7 != base64.StdEncoding.EncodeToString(message) {
				t.Fatal("wrong envelope")
			}
		})
	}
}
func setRetrievalCloud(cfg *config, origin string) {
	cfg.PairingURL = origin + "/v1/relay/pairing-enrollments"
	cfg.ControlURL = origin + "/v1/relay/control"
	cfg.HL7URL = origin + "/v1/relay/ingest/hl7"
	cfg.DicomURL = origin + "/v1/relay/ingest/dicom"
}

func retrievalTestElement(tag uint32, vr string, value []byte) []byte {
	if len(value)%2 != 0 {
		padding := byte(' ')
		if vr == "UI" || vr == "OB" || vr == "OW" {
			padding = 0
		}
		value = append(append([]byte{}, value...), padding)
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint16(tag>>16))
	binary.Write(&b, binary.LittleEndian, uint16(tag))
	b.WriteString(vr)
	switch vr {
	case "SQ", "UT", "OB", "OW":
		b.Write([]byte{0, 0})
		binary.Write(&b, binary.LittleEndian, uint32(len(value)))
	default:
		binary.Write(&b, binary.LittleEndian, uint16(len(value)))
	}
	b.Write(value)
	return b.Bytes()
}
func retrievalTestDICOM(study, sop, patient string) []byte {
	class := "1.2.840.10008.5.1.4.1.1.7"
	b := buildPart10Header(class, sop, "1.2.840.10008.1.2.1")
	for _, e := range []struct {
		tag       uint32
		vr, value string
	}{{0x00080016, "UI", class}, {0x00080018, "UI", sop}, {0x00080050, "SH", "ACC"}} {
		b = append(b, retrievalTestElement(e.tag, e.vr, []byte(e.value))...)
	}
	issuer := retrievalTestElement(0x00400031, "UT", []byte("CLINIC"))
	var item bytes.Buffer
	binary.Write(&item, binary.LittleEndian, uint16(0xfffe))
	binary.Write(&item, binary.LittleEndian, uint16(0xe000))
	binary.Write(&item, binary.LittleEndian, uint32(len(issuer)))
	item.Write(issuer)
	b = append(b, retrievalTestElement(0x00080051, "SQ", item.Bytes())...)
	for _, e := range []struct {
		tag       uint32
		vr, value string
	}{{0x00100020, "LO", patient}, {0x00100021, "LO", "CLINIC"}, {0x0020000d, "UI", study}, {0x0020000e, "UI", study + ".1"}} {
		b = append(b, retrievalTestElement(e.tag, e.vr, []byte(e.value))...)
	}
	return append(b, retrievalTestElement(0x7fe00010, "OW", []byte{0, 1, 2, 3})...)
}
func retrievalTestQIDO(study, patient string) map[string]any {
	a := func(vr, s string) any { return map[string]any{"vr": vr, "Value": []string{s}} }
	return map[string]any{"00100020": a("LO", patient), "00100021": a("LO", "CLINIC"), "00080050": a("SH", "ACC"), "0020000D": a("UI", study), "00080051": map[string]any{"vr": "SQ", "Value": []any{map[string]any{"00400031": a("UT", "CLINIC")}}}}
}
func writeRetrievalMultipart(w http.ResponseWriter, objects [][]byte, truncate bool) {
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for _, object := range objects {
		part, _ := parts.CreatePart(textproto.MIMEHeader{"Content-Type": []string{"application/dicom"}})
		part.Write(object)
	}
	if !truncate {
		parts.Close()
	}
	w.Header().Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+parts.Boundary())
	w.Write(body.Bytes())
}
func TestRetrievalRequeriesAfterRestartAndReplaysCommittedSubmissions(t *testing.T) {
	cfg := retrievalTestConfig(t)
	provider := testProvider(t, testCredential('A'))
	status := newRuntimeStatus(cfg.configPath)
	var mu sync.Mutex
	studies := []string{"1.2.10", "1.2.20"}
	queryCount := 0
	var arrivals [][]byte
	var selections [][]byte
	var results [][]byte
	pacs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("cloud bearer leaked to PACS")
		}
		mu.Lock()
		snapshot := append([]string{}, studies...)
		mu.Unlock()
		if r.URL.Path == "/dicom-web/studies" {
			if r.URL.Query().Has("00100020") || r.URL.Query().Has("00100021") || r.URL.Query().Get("00080050") != "ACC" || r.URL.Query().Get("00080051.00400031") != "CLINIC" {
				t.Error("query not scoped")
			}
			mu.Lock()
			queryCount++
			mu.Unlock()
			values := []any{}
			for _, s := range snapshot {
				values = append(values, retrievalTestQIDO(s, "OTHER"))
			}
			w.Header().Set("Content-Type", "application/dicom+json")
			json.NewEncoder(w).Encode(values)
			return
		}
		study := strings.TrimPrefix(r.URL.Path, "/dicom-web/studies/")
		object := retrievalTestDICOM(study, study+".1.1", "")
		writeRetrievalMultipart(w, [][]byte{object, object}, false)
	}))
	defer pacs.Close()
	cfg.Retrieval.PACS[0].DICOMwebURL = pacs.URL + "/dicom-web"
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/studies"):
			var body struct {
				AttemptID, Token  string
				StudyInstanceUIDs []string
			}
			json.Unmarshal(b, &body)
			if body.Token != "claim-token" || body.AttemptID == "" {
				t.Error("missing lease")
			}
			mu.Lock()
			selections = append(selections, b)
			n := len(selections)
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "studyInstanceUids": body.StudyInstanceUIDs})
		case strings.HasSuffix(r.URL.Path, "/dicom"):
			if r.Header.Get("X-Telrad-Retrieval-Attempt") == "" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("incorrect correlation")
			}
			mu.Lock()
			arrivals = append(arrivals, b)
			mu.Unlock()
			w.WriteHeader(201)
			io.WriteString(w, `{"status":"accepted","receiptId":"receipt"}`)
		case strings.HasSuffix(r.URL.Path, "/result"):
			mu.Lock()
			results = append(results, b)
			n := len(results)
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"ok":true}`)
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
		}
	}))
	defer cloud.Close()
	setRetrievalCloud(cfg, cloud.URL)
	saveRetrievalTestConfig(t, cfg)
	envelope := testSignedReferral(t, cfg)
	for _, attempt := range []string{"attempt-one", "attempt-two"} {
		restarted, e := loadConfigMode(cfg.configPath, false)
		if e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		executeRetrieval(ctx, restarted, cfg.ControlURL+"/sessions/session", retrievalClaim{JobID: "job", AttemptID: attempt, Token: "claim-token", ClaimExpiresAt: time.Now().Add(8 * time.Second), Permit: envelope}, pacs.Client(), cloud.Client(), provider, status)
		cancel()
		mu.Lock()
		studies = append(studies, "1.2.30")
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if queryCount != 2 || len(arrivals) != 10 || len(selections) != 3 || len(results) != 3 {
		t.Fatalf("queries=%d uploads=%d selections=%d results=%d", queryCount, len(arrivals), len(selections), len(results))
	}
	if !bytes.Equal(selections[0], selections[1]) || !bytes.Equal(results[0], results[1]) {
		t.Fatal("committed retry changed")
	}
	for i, body := range results {
		var value struct{ Result retrievalResult }
		if json.Unmarshal(body, &value) != nil || value.Result.Outcome != "uploaded" || value.Result.OutstandingUploads == nil || *value.Result.OutstandingUploads != 0 {
			t.Fatal("invalid completion")
		}
		want := 2
		if i == 2 {
			want = 3
		}
		if len(value.Result.Studies) != want {
			t.Fatal("later matching study excluded")
		}
		for _, s := range value.Result.Studies {
			if s.UniqueInstanceCount != 1 || s.InventorySHA256 != inventoryDigest(map[string]bool{s.StudyInstanceUID + ".1.1": true}) || s.Completion.Status != "not_provided" {
				t.Fatal("invalid inventory")
			}
		}
	}
	files, e := os.ReadDir(filepath.Dir(cfg.configPath))
	if e != nil {
		t.Fatal(e)
	}
	for _, file := range files {
		if file.Name() != "relay.json" && file.Name() != permitKeyFilename && file.Name() != "runtime-status.json" {
			t.Fatalf("unexpected persistent file %s", file.Name())
		}
	}
}
func TestRetrievalQueryAndTransferFailures(t *testing.T) {
	cfg := retrievalTestConfig(t)
	envelope := testSignedReferral(t, cfg)
	p, _, e := authorizeRetrieval(cfg, envelope)
	if e != nil {
		t.Fatal(e)
	}
	conflict := retrievalTestQIDO("1.2.3", "PATIENT")
	conflict["00080050"] = map[string]any{"vr": "SH", "Value": []string{"OTHER"}}
	for _, test := range []struct {
		name    string
		objects []any
		warning bool
		want    string
	}{{"none", []any{}, false, "not_found"}, {"conflict", []any{conflict}, false, "identity_mismatch"}, {"duplicate", []any{retrievalTestQIDO("1.2.3", "PATIENT"), retrievalTestQIDO("1.2.3", "PATIENT")}, false, "ambiguous_identity"}, {"truncated", []any{retrievalTestQIDO("1.2.3", "PATIENT")}, true, "local_policy_rejected"}} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/dicom+json")
				if test.warning {
					w.Header().Set("Warning", "299 truncated")
				}
				json.NewEncoder(w).Encode(test.objects)
			}))
			defer server.Close()
			pacs := cfg.Retrieval.PACS[0]
			pacs.DICOMwebURL = server.URL
			if _, e := queryAccession(context.Background(), server.Client(), pacs, p); e == nil || e.Error() != test.want {
				t.Fatalf("error=%v", e)
			}
		})
	}
	for _, test := range []struct {
		name              string
		patient           string
		truncate, receipt bool
	}{{"identity", "OTHER", false, true}, {"multipart", "PATIENT", true, true}, {"receipt", "PATIENT", false, false}} {
		t.Run(test.name, func(t *testing.T) {
			uploads := 0
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				uploads++
				w.Header().Set("Content-Type", "application/json")
				if test.receipt {
					w.WriteHeader(201)
					io.WriteString(w, `{"status":"accepted","receiptId":"receipt"}`)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer cloud.Close()
			pacs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				object := retrievalTestDICOM("1.2.3", "1.2.3.4", test.patient)
				if test.name == "identity" {
					object = bytes.Replace(object, []byte("ACC "), []byte("BAD "), 1)
				}
				writeRetrievalMultipart(w, [][]byte{object}, test.truncate)
			}))
			defer pacs.Close()
			local := cfg.Retrieval.PACS[0]
			local.DICOMwebURL = pacs.URL
			candidate := *cfg
			candidate.DicomURL = cloud.URL
			_, e := retrieveWADO(context.Background(), pacs.Client(), cloud.Client(), &candidate, testProvider(t, testCredential('A')), newRuntimeStatus(cfg.configPath), local, p, "1.2.3", "attempt", func() error { return nil })
			if e == nil {
				t.Fatal("failed transfer reported complete")
			}
			if test.patient == "OTHER" && uploads != 0 {
				t.Fatal("foreign identity forwarded")
			}
		})
	}
}
func TestRetrievalDICOMIntegrityAndBounds(t *testing.T) {
	cfg := retrievalTestConfig(t)
	p, _, e := authorizeRetrieval(cfg, testSignedReferral(t, cfg))
	if e != nil {
		t.Fatal(e)
	}
	object := retrievalTestDICOM("1.2.3", "1.2.3.4", "PATIENT")
	stream, e := retrieval.OpenDICOM(bytes.NewReader(object), p, "1.2.3", int64(len(object)))
	if e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if _, e := stream.WriteTo(&out); e != nil || !bytes.Equal(out.Bytes(), object) {
		t.Fatalf("byte integrity failed: %v", e)
	}
	for _, bad := range [][]byte{object[:len(object)-1], append(append([]byte{}, object...), retrievalTestElement(0x00100020, "LO", []byte("OTHER"))...)} {
		stream, e := retrieval.OpenDICOM(bytes.NewReader(bad), p, "1.2.3", int64(len(bad)))
		if e == nil {
			_, e = stream.WriteTo(io.Discard)
		}
		if e == nil {
			t.Fatal("truncated or conflicting DICOM accepted")
		}
	}
	stream, e = retrieval.OpenDICOM(bytes.NewReader(object), p, "1.2.3", int64(len(object)-1))
	if e == nil {
		_, e = stream.WriteTo(io.Discard)
	}
	if e == nil {
		t.Fatal("instance limit ignored")
	}
}
func TestRetrievalSchemaFourMigrationPreservesPush(t *testing.T) {
	cfg := pairedTestConfig(t.TempDir())
	cfg.SchemaVersion = 4
	if e := atomicWriteJSON(cfg.configPath, cfg); e != nil {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(cfg.configPath)
	if _, e := loadConfigMode(cfg.configPath, false); e == nil {
		t.Fatal("read-only path migrated")
	}
	after, _ := os.ReadFile(cfg.configPath)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only path wrote")
	}
	upgraded, e := loadConfig(cfg.configPath)
	if e != nil {
		t.Fatal(e)
	}
	if upgraded.SchemaVersion != 5 || upgraded.Retrieval != nil || upgraded.DisableDICOMListener || upgraded.RelayID != cfg.RelayID || upgraded.HL7URL != cfg.HL7URL {
		t.Fatal("push compatibility changed")
	}
	again, e := loadConfig(cfg.configPath)
	if e != nil || !reflect.DeepEqual(upgraded, again) {
		t.Fatal("migration not idempotent")
	}
}
func TestRetrievalTestModeNeverStartsPACS(t *testing.T) {
	cfg := retrievalTestConfig(t)
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer server.Close()
	stop := startRetrievalSession(context.Background(), cfg, readyMessage{ConnectorID: cfg.RelayID, IngestMode: "TEST"}, server.URL, server.Client(), testProvider(t, testCredential('A')), newWorkDrainer(), newRuntimeStatus(cfg.configPath))
	stop()
	if calls != 0 {
		t.Fatal("TEST started retrieval")
	}
}

func TestRetrievalReportLoopCannotMintReferrals(t *testing.T) {
	referral := string(retrievalTestHL7("NW", 1))
	report := strings.Replace(referral, "ORM^O01", "ORU^R01", 1)
	if !safeRetrievalReport(report) {
		t.Fatal("valid report blocked")
	}
	for _, payload := range []string{referral, report + referral, report + "\x1c\r\x0b" + referral} {
		if safeRetrievalReport(payload) {
			t.Fatal("cloud-origin referral loop accepted")
		}
	}
}

func TestRetrievalRoutingUsesExplicitDiscoveryAndNeverFailureFallback(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		available  bool
		code       int
		wantRaw    bool
	}{{"push", "PUSH", false, 200, true}, {"retrieve disabled", "RETRIEVE", false, 200, false}, {"missing discovery", "", false, 404, false}, {"unknown mode", "OTHER", true, 200, false}} {
		t.Run(test.name, func(t *testing.T) {
			cfg := retrievalTestConfig(t)
			raw := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/retrieval-settings") {
					if r.Method != http.MethodGet || r.ContentLength > 0 {
						t.Error("invalid discovery request")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.code)
					json.NewEncoder(w).Encode(retrievalSettings{2, cfg.Retrieval.CompanyID, cfg.RelayID, "PRODUCTION", test.mode, test.available})
					return
				}
				if r.URL.Path == "/v1/relay/ingest/hl7" {
					raw++
					w.Header().Set("Content-Type", "application/hl7-v2")
					io.WriteString(w, "MSH|^~\\&|TELRAD|RIS|SYNTHETIC|CLINIC|20260909000000||ACK|ack|P|2.5\rMSA|AA|message-1\r")
					return
				}
				t.Error("unexpected route")
				w.WriteHeader(404)
			}))
			defer server.Close()
			setRetrievalCloud(cfg, server.URL)
			saveRetrievalTestConfig(t, cfg)
			_, e := ingestClinicHL7(context.Background(), cfg, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12000}, server.Client(), testProvider(t, testCredential('A')), newRuntimeStatus(cfg.configPath), retrievalTestHL7("NW", 1), "message-1")
			if (e == nil) != test.wantRaw || (raw == 1) != test.wantRaw {
				t.Fatalf("raw=%d error=%v", raw, e)
			}
		})
	}
}

func TestAccessionReferralDoesNotRequirePatient(t *testing.T) {
	cfg := retrievalTestConfig(t)
	cfg.Retrieval.PACS[0].PatientIssuer = ""
	saveRetrievalTestConfig(t, cfg)
	for _, patient := range []string{"", "PATIENT", "OTHER^^^OTHER", "ONE^^^A~TWO^^^B"} {
		message := bytes.Replace(retrievalTestHL7("NW", 1), []byte("PATIENT^^^CLINIC"), []byte(patient), 1)
		permits, qualifies, err := signReferrals(cfg, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12000}, message)
		if err != nil || !qualifies || len(permits) != 1 {
			t.Fatal("patient-independent signing failed", err)
		}
		p, _, err := authorizeRetrieval(cfg, permits[0])
		if err != nil || p.Patient != nil || p.Examination.Accession != "ACC" {
			t.Fatal("incorrect accession scope", err)
		}
	}
}
