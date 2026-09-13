package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHL7CloudOwnsValidationAndAcknowledgements(t *testing.T) {
	good := string(retrievalTestHL7("NW", 2))
	editOBR := func(message string, occurrence int, value string) string {
		lines := strings.Split(message, "\r")
		n := 0
		for i, line := range lines {
			if strings.HasPrefix(line, "OBR|") {
				n++
				if n == occurrence {
					fields := strings.Split(line, "|")
					fields[18] = value
					lines[i] = strings.Join(fields, "|")
				}
			}
		}
		return strings.Join(lines, "\r")
	}
	cases := []struct {
		name, message string
		grants        int
		raw           bool
	}{
		{"version belongs to cloud", strings.Replace(good, "|2.5", "|2.6", 1), 2, false},
		{"duplicate patient belongs to cloud", good + "PID|2||SYNTHETIC\r", 2, false},
		{"missing accession withholds all grants", editOBR(good, 2, ""), 0, false},
		{"unapproved retrieval source", strings.Replace(good, "|SYNTHETIC|", "|OTHER|", 1), 2, false},
		{"invalid accession withholds all grants", editOBR(good, 2, "ACC*"), 0, false},
		{"unknown action", strings.ReplaceAll(good, "ORC|NW|", "ORC|ZZ|"), 0, false},
		{"mixed cancellation", strings.Replace(good, "ZDS|", "ORC|CA||ACC\rZDS|", 1), 0, false},
		{"cancellation", strings.ReplaceAll(good, "ORC|NW|", "ORC|CA|"), 0, false},
		{"ambiguous processing", strings.Replace(good, "|P|2.5", "||2.5", 1), 0, false},
		{"test processing", strings.Replace(good, "|P|2.5", "|T|2.5", 1), 2, false},
		{"embedded header", good + strings.Split(good, "\r")[0] + "\r", 0, false},
		{"malformed segment", good + "BROKEN\r", 0, true},
	}
	for _, mode := range []string{"PUSH", "RETRIEVE"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				cfg := retrievalTestConfig(t)
				if mode == "PUSH" {
					cfg.Retrieval = nil
				}
				var submissions atomic.Int32
				cloudACK := func(code string) []byte {
					return []byte("MSH|^~\\&|TELRAD|CLOUD|SYNTHETIC|CLINIC|20260101000000||ACK^O01^ACK|cloud-ack|P|2.5\rMSA|" + code + "|message-1\rERR|||207^Application internal error^HL70357|E||||Synthetic cloud decision\r")
				}
				cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/retrieval-settings") {
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(retrievalSettings{2, cfg.Retrieval.CompanyID, cfg.RelayID, "PRODUCTION", mode, true})
						return
					}
					if strings.HasSuffix(r.URL.Path, "/signing-keys") {
						w.WriteHeader(201)
						return
					}
					arrival := submissions.Add(1)
					raw, e := io.ReadAll(r.Body)
					if e != nil {
						t.Error(e)
					}
					want := tc.message
					expectedGrants := tc.grants
					expectedRaw := tc.raw
					if arrival == 2 {
						want = good
						expectedGrants = 2
						expectedRaw = false
					}
					if expectedRaw {
						if !strings.HasSuffix(r.URL.Path, "/ingest/hl7") {
							t.Error("expected raw ingest")
						}
					} else {
						if !strings.HasSuffix(r.URL.Path, "/ingest/referrals") {
							t.Error("expected atomic referral ingest")
						}
						var body struct {
							HL7     string   `json:"hl7"`
							Permits []string `json:"permits"`
							Reports []string `json:"reportAuthorizations"`
						}
						if json.Unmarshal(raw, &body) != nil {
							t.Error("invalid envelope")
						}
						raw, e = base64.StdEncoding.DecodeString(body.HL7)
						if e != nil {
							t.Error(e)
						}
						if len(body.Reports) != expectedGrants {
							t.Errorf("report grants=%d want=%d", len(body.Reports), expectedGrants)
						}
						expectedPermits := 0
						if mode == "RETRIEVE" && expectedGrants > 0 && !strings.Contains(want, "|T|2.5") && !strings.Contains(want, "|OTHER|") {
							expectedPermits = expectedGrants
						}
						if len(body.Permits) != expectedPermits {
							t.Errorf("retrieval grants=%d want=%d", len(body.Permits), expectedPermits)
						}
					}
					if !bytes.Equal(raw, []byte(want)) {
						t.Error("original order changed")
					}
					code := "AR"
					if arrival == 2 {
						code = "AA"
					}
					w.Header().Set("Content-Type", "application/hl7-v2")
					w.Write(cloudACK(code))
				}))
				defer cloud.Close()
				setRetrievalCloud(cfg, cloud.URL)
				saveRetrievalTestConfig(t, cfg)
				listener, e := net.Listen("tcp4", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				defer listener.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				provider := testProvider(t, testCredential('A'))
				status := newRuntimeStatus(cfg.configPath)
				done := make(chan struct{})
				go func() {
					defer close(done)
					conn, e := listener.Accept()
					if e != nil {
						return
					}
					defer conn.Close()
					serveHL7(ctx, conn, cfg, cloud.Client(), provider, status)
				}()
				clinic, e := net.Dial("tcp4", listener.Addr().String())
				if e != nil {
					t.Fatal(e)
				}
				defer func() { clinic.Close(); <-done }()
				clinic.SetDeadline(time.Now().Add(10 * time.Second))
				reader := bufio.NewReader(clinic)
				for i, msg := range []string{tc.message, good} {
					if _, e = clinic.Write(append(append([]byte{mllpStart}, []byte(msg)...), mllpEnd, mllpCR)); e != nil {
						t.Fatal(e)
					}
					frame, e := readMLLPFrameFrom(reader, 65536)
					if e != nil {
						t.Fatal(e)
					}
					code := "AR"
					if i == 1 {
						code = "AA"
					}
					if !bytes.Equal(frame[1:len(frame)-2], cloudACK(code)) {
						t.Fatal("cloud ACK not preserved byte for byte")
					}
				}
				if submissions.Load() != 2 {
					t.Fatalf("submissions=%d", submissions.Load())
				}
			})
		}
	}
}
