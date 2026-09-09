package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/synthetic"
)

// The same reader survives many frames, including coalesced messages. Parallel
// workers own their readers and frames; no message-sized buffer is retained idle.
func BenchmarkHL7PersistentMixed(b *testing.B) {
	for _, limit := range []int{1024 * 1024, 8 * 1024 * 1024} {
		b.Run(benchmarkSizeName(limit), func(b *testing.B) {
			var exchange []byte
			for _, size := range []int{1024, 1024, 64 * 1024, limit} {
				exchange = append(exchange, synthetic.Frame(synthetic.HL7("perf-frame", size))...)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(exchange) - 12))
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				var source bytes.Reader
				r := bufio.NewReaderSize(&source, 32*1024)
				for pb.Next() {
					source.Reset(exchange)
					r.Reset(&source)
					for range 4 {
						frame, err := readMLLPFrameFrom(r, int64(limit))
						if err != nil {
							b.Error(err)
							return
						}
						if _, err := hl7ControlID(frame[1 : len(frame)-2]); err != nil {
							b.Error(err)
							return
						}
					}
				}
			})
		})
	}
}

// Each parallel worker keeps its MLLP connection across complete HTTPS/ACK
// exchanges. Parser-only measurements above separate framing from transport cost.
func BenchmarkHL7Stream(b *testing.B) {
	for _, limit := range []int{1024 * 1024, 8 * 1024 * 1024} {
		b.Run(benchmarkSizeName(limit), func(b *testing.B) {
			var frames, acks [][]byte
			payloads := map[string][]byte{}
			responses := map[string][]byte{}
			var size int64
			for i, n := range []int{1024, 64 * 1024, limit, 1024} {
				id := fmt.Sprintf("perf-exchange-%d", i)
				payload := synthetic.HL7(id, n)
				payloads[id] = payload
				frames = append(frames, synthetic.Frame(payload))
				acks = append(acks, synthetic.ACK("AA", id, "perf-cloud-ack"))
				responses[id] = acks[i]
				size += int64(len(payload))
			}
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
				id, idErr := hl7ControlID(body)
				if err != nil || idErr != nil || !bytes.Equal(body, payloads[id]) {
					b.Error("cloud received a changed HL7 payload")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/hl7-v2")
				_, _ = w.Write(responses[id])
			}))
			defer cloud.Close()
			cfg := defaultConfig()
			cfg.HL7URL, cfg.HL7MaxBytes = cloud.URL, int64(limit)
			path := filepath.Join(b.TempDir(), "credential.json")
			if err := commitCredential(path, credentialFile{SchemaVersion: 1, Credential: testCredential('H')}); err != nil {
				b.Fatal(err)
			}
			provider, err := newCredentialProvider(path, time.Now())
			if err != nil {
				b.Fatal(err)
			}
			status := newRuntimeStatus(filepath.Join(b.TempDir(), "relay.json"))
			b.ReportAllocs()
			b.SetBytes(size)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				clinic, relay := net.Pipe()
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer relay.Close()
					serveHL7(ctx, relay, cfg, cloud.Client(), provider, status)
				}()
				defer func() { _ = clinic.Close(); cancel(); <-done }()
				reader := bufio.NewReader(clinic)
				for pb.Next() {
					for i, frame := range frames {
						_ = clinic.SetDeadline(time.Now().Add(time.Minute))
						if _, err := clinic.Write(frame); err != nil {
							b.Error(err)
							return
						}
						ack, err := synthetic.ReadFrame(reader, 64*1024)
						if err != nil || !bytes.Equal(ack, acks[i]) {
							b.Error("missing or changed correlated HL7 ACK")
							return
						}
					}
				}
			})
		})
	}
}

func BenchmarkDICOMParsing(b *testing.B) {
	for _, size := range []int{16 * 1024, 64 * 1024} {
		for _, count := range []int{1, 4} {
			b.Run(fmt.Sprintf("%s/%d-PDVs", benchmarkSizeName(size), count), func(b *testing.B) {
				var body, wire bytes.Buffer
				for range count {
					body.Write(synthetic.PDV(make([]byte, size/count-6), 2))
				}
				_ = synthetic.WritePDU(&wire, 4, body.Bytes())
				var reader bytes.Reader
				b.ReportAllocs()
				b.SetBytes(int64(body.Len()))
				b.ResetTimer()
				for range b.N {
					reader.Reset(wire.Bytes())
					_, data, err := readDICOMPDU(&reader)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := parsePDVs(data); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
	b.Run("fragmented-command", func(b *testing.B) {
		cmd := synthetic.Command(1, "1.2.826.0.1.3680043.10.543.99.1")
		parts := [][]byte{synthetic.PDV(cmd[:7], 1), synthetic.PDV(cmd[7:31], 1), synthetic.PDV(cmd[31:], 3)}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			var command bytes.Buffer
			for _, part := range parts {
				pdvs, err := parsePDVs(part)
				if err != nil {
					b.Fatal(err)
				}
				for _, pdv := range pdvs {
					command.Write(pdv.data)
				}
			}
			if _, err := parseDIMSECommand(command.Bytes()); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkDICOMStream(b *testing.B) {
	for _, pdu := range []int{16 * 1024, 64 * 1024} {
		for _, instances := range []int{1, 100} {
			b.Run(fmt.Sprintf("%s/%d-instances", benchmarkSizeName(pdu), instances), func(b *testing.B) {
				uid := "1.2.826.0.1.3680043.10.543.99.1"
				file := synthetic.DICOM(uid, uid+".1", 512, 1024)
				r := bufio.NewReader(bytes.NewReader(file))
				_, err := synthetic.Part10(r)
				if err != nil {
					b.Fatal(err)
				}
				dataset, _ := io.ReadAll(r)
				cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					_, _ = io.WriteString(w, `{"status":"accepted","receiptId":"benchmark-receipt"}`)
				}))
				defer cloud.Close()
				cfg := defaultConfig()
				cfg.DicomURL = cloud.URL
				path := filepath.Join(b.TempDir(), "credential.json")
				if err := commitCredential(path, credentialFile{SchemaVersion: 1, Credential: testCredential('B')}); err != nil {
					b.Fatal(err)
				}
				provider, err := newCredentialProvider(path, time.Now())
				if err != nil {
					b.Fatal(err)
				}
				status := newRuntimeStatus(filepath.Join(b.TempDir(), "relay.json"))
				b.ReportAllocs()
				b.SetBytes(int64(len(dataset) * instances))
				b.ResetTimer()
				for range b.N {
					ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
					clinic, relay := net.Pipe()
					done := make(chan struct{})
					go func() {
						defer close(done)
						defer relay.Close()
						serveDICOM(ctx, relay, cfg, cloud.Client(), provider, status)
					}()
					_ = clinic.SetDeadline(time.Now().Add(time.Minute))
					if err := synthetic.Associate(clinic, synthetic.ExplicitVRLittleEndian); err != nil {
						b.Fatal(err)
					}
					for i := range instances {
						id := uint16(i + 1)
						if err := synthetic.SendStore(clinic, uid, id, bytes.NewReader(dataset), int64(len(dataset)), pdu, true, true); err != nil {
							b.Fatal(err)
						}
						code, err := synthetic.StoreResponse(clinic, id)
						if err != nil || code != 0 {
							b.Fatalf("store: %x %v", code, err)
						}
					}
					_ = synthetic.WritePDU(clinic, 5, make([]byte, 4))
					kind, _, err := synthetic.ReadPDU(clinic)
					if err != nil || kind != 6 {
						b.Fatalf("release: %x %v", kind, err)
					}
					clinic.Close()
					cancel()
					<-done
				}
			})
		}
	}
}
