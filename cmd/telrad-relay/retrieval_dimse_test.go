package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

const syntheticStorageClass = "1.2.840.10008.5.1.4.1.1.7"

func dimseFixture(syntax, accession, study, sop string) []byte {
	b := buildPart10Header(syntheticStorageClass, sop, syntax)
	for _, e := range []struct {
		tag       uint32
		vr, value string
	}{
		{0x00080016, "UI", syntheticStorageClass}, {0x00080018, "UI", sop}, {0x00080050, "SH", accession},
		{0x0020000d, "UI", study}, {0x0020000e, "UI", study + ".1"},
	} {
		b = append(b, retrievalElement(e.tag, e.vr, e.value, syntax)...)
	}
	// Neither patient identifiers nor issuer tags are required by this fixture.
	if syntax == implicitLittleEndian {
		b = append(b, []byte{0xe0, 0x7f, 0x10, 0, 4, 0, 0, 0, 1, 2, 3, 4}...)
	} else {
		b = append(b, retrievalTestElement(0x7fe00010, "OW", []byte{1, 2, 3, 4})...)
	}
	return b
}
func dimseFixtureDataset(object []byte) []byte {
	offset := 132
	for binary.LittleEndian.Uint16(object[offset:]) == 2 {
		n := int(binary.LittleEndian.Uint16(object[offset+6:]))
		header := 8
		if string(object[offset+4:offset+6]) == "OB" {
			header = 12
			n = int(binary.LittleEndian.Uint32(object[offset+8:]))
		}
		offset += header + n
	}
	return object[offset:]
}

func dimseTestPeer(t *testing.T, syntax string, role bool, run func(*retrievalAssociation, []byte), repeat ...bool) retrievalPACS {
	t.Helper()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(60 * time.Second))
				kind, rq, e := readDICOMPDU(c)
				if e != nil || kind != 1 {
					t.Error("missing association request")
					return
				}
				ac := append([]byte{}, rq[:68]...)
				contexts := map[byte]presentationContext{}
				var user []byte
				e = associationItems(rq[68:], func(k byte, v []byte) error {
					switch k {
					case 0x10:
						ac = append(ac, associationItem(k, v)...)
					case 0x20:
						pc, err := parsePresentationContext(v)
						if err != nil {
							return err
						}
						matched := false
						_ = associationItems(v[4:], func(k byte, value []byte) error {
							if k == 0x40 && string(value) == syntax {
								matched = true
							}
							return nil
						})
						if !matched {
							ac = append(ac, associationItem(0x21, []byte{pc.ID, 0, 4, 0})...)
							return nil
						}
						pc.TransferSyntax = syntax
						pc.Accepted = true
						contexts[pc.ID] = pc
						accepted := append([]byte{pc.ID, 0, 0, 0}, associationItem(0x40, []byte(syntax))...)
						ac = append(ac, associationItem(0x21, accepted)...)
					case 0x50:
						return associationItems(v, func(k byte, value []byte) error {
							if k == 0x54 {
								if len(value) < 4 || value[len(value)-2] != 0 || value[len(value)-1] != 1 {
									return errors.New("wrong storage role")
								}
								if role {
									user = append(user, associationItem(k, value)...)
								}
							}
							return nil
						})
					}
					return nil
				})
				if e != nil {
					t.Error(e)
					return
				}
				user = append(user, associationItem(0x51, []byte{0, 0, 16, 0})...)
				ac = append(ac, associationItem(0x50, user)...)
				if e = writeDICOMPDU(c, 2, ac); e != nil {
					t.Error(e)
					return
				}
				a := &retrievalAssociation{conn: c, contexts: contexts, maxSend: 4096, stop: func() bool { return true }}
				run(a, rq)
			}()
			if len(repeat) == 0 || !repeat[0] {
				return
			}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(11 * time.Second):
			t.Error("DIMSE peer did not stop")
		}
	})
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	return retrievalPACS{ID: "pacs", Adapter: "dimse-find-get-v1", Host: "127.0.0.1", Port: n, CalledAETitle: "PACS", CallingAETitle: "RELAY", StorageSOPClasses: []string{syntheticStorageClass}, AccessionIssuer: "CLINIC", MaxInstanceBytes: 1024 * 1024, MaxStudyBytes: 2 * 1024 * 1024, MaxInstances: 100, RequestTimeoutSeconds: 5}
}
func dimseTestRequest(t *testing.T, a *retrievalAssociation, field uint16) []byte {
	t.Helper()
	id, f, e := a.command()
	if e != nil {
		t.Error(e)
		return nil
	}
	actual, _ := commandUS(f, 0x0100)
	if id != 1 || actual != field {
		t.Error("incorrect DIMSE operation")
	}
	b, e := io.ReadAll(&retrievalDatasetReader{a: a, id: 1, limit: 1024 * 1024})
	if e != nil {
		t.Error(e)
	}
	return b
}
func dimseResponse(class string, field, status uint16, dataset bool, completed int) []byte {
	data := buildDIMSEStoreResponse(dicomCommand{SOPClassUID: class, MessageID: 1}, field, status)
	if dataset {
		i := bytes.Index(data, []byte{0, 0, 0, 8, 2, 0, 0, 0})
		if i >= 0 {
			binary.LittleEndian.PutUint16(data[i+8:], 0x0102)
		}
	}
	if completed >= 0 {
		var b bytes.Buffer
		b.Write(data)
		if status == 0xff00 {
			writeCommandUS(&b, 0x1020, 1)
		}
		writeCommandUS(&b, 0x1021, uint16(completed))
		writeCommandUS(&b, 0x1022, 0)
		writeCommandUS(&b, 0x1023, 0)
		data = b.Bytes()
		binary.LittleEndian.PutUint32(data[8:], uint32(len(data)-12))
	}
	return data
}
func dimseTestRelease(t *testing.T, a *retrievalAssociation) {
	t.Helper()
	kind, body, e := readDICOMPDU(a.conn)
	if e != nil || kind != 5 || len(body) != 4 {
		t.Error("missing release")
		return
	}
	writeDICOMPDU(a.conn, 6, make([]byte, 4))
}
func dimseStore(t *testing.T, a *retrievalAssociation, object []byte, fragment bool, instance ...string) {
	t.Helper()
	var b bytes.Buffer
	writeCommandUI(&b, 2, syntheticStorageClass)
	writeCommandUS(&b, 0x0100, 1)
	writeCommandUS(&b, 0x0110, 7)
	writeCommandUS(&b, 0x0700, 0)
	writeCommandUS(&b, 0x0800, 1)
	sop := "1.2.3.4"
	if len(instance) > 0 {
		sop = instance[0]
	}
	writeCommandUI(&b, 0x1000, sop)
	h := make([]byte, 12)
	binary.LittleEndian.PutUint32(h[4:], 4)
	binary.LittleEndian.PutUint32(h[8:], uint32(b.Len()))
	id := byte(3)
	if !a.contexts[id].Accepted {
		id = 5
	}
	if e := a.send(id, true, append(h, b.Bytes()...)); e != nil {
		t.Error(e)
		return
	}
	if fragment {
		a.maxSend = 23
	}
	if e := a.send(id, false, dimseFixtureDataset(object)); e != nil {
		t.Error(e)
	}
}
func TestDIMSEAccessionFind(t *testing.T) {
	cfg := retrievalTestConfig(t)
	p, _, _ := authorizeRetrieval(cfg, testSignedReferral(t, cfg))
	for _, syntax := range []string{explicitLittleEndian, implicitLittleEndian} {
		for _, scenario := range []string{"multiple", "none", "wrong accession", "duplicate", "overflow", "failed final"} {
			t.Run(syntax+"/"+scenario, func(t *testing.T) {
				pacs := dimseTestPeer(t, syntax, true, func(a *retrievalAssociation, rq []byte) {
					data := dimseTestRequest(t, a, 0x0020)
					if !bytes.Contains(data, []byte("ACC")) || bytes.Contains(data, []byte{0x10, 0, 0x20, 0}) {
						t.Error("query contains patient or omits accession")
					}
					if strings.TrimSpace(string(rq[4:20])) != "PACS" || strings.TrimSpace(string(rq[20:36])) != "RELAY" {
						t.Error("wrong AE titles")
					}
					count := 2
					if scenario == "none" {
						count = 0
					}
					if scenario == "overflow" {
						count = 65
					}
					for i := 0; i < count; i++ {
						uid := "1.2." + strconv.Itoa(i+1)
						if scenario == "duplicate" {
							uid = "1.2.1"
						}
						acc := "ACC"
						if scenario == "wrong accession" {
							acc = "OTHER"
						}
						response := append(retrievalElement(0x00080050, "SH", acc, syntax), retrievalElement(0x0020000d, "UI", uid, syntax)...)
						if a.send(1, true, dimseResponse(studyRootFind, 0x8020, 0xff01, true, -1)) != nil {
							return
						}
						if a.send(1, false, response) != nil {
							return
						}
					}
					if scenario != "multiple" && scenario != "none" && scenario != "failed final" {
						return
					}
					final := uint16(0)
					if scenario == "failed final" {
						final = 0xa700
					}
					a.send(1, true, dimseResponse(studyRootFind, 0x8020, final, false, -1))
					if final == 0 {
						dimseTestRelease(t, a)
					}
				})
				selected, e := queryDIMSEAccession(context.Background(), pacs, p)
				if scenario == "multiple" {
					if e != nil || len(selected) != 2 {
						t.Fatalf("find: %v %v", selected, e)
					}
				} else if e == nil {
					t.Fatal("incomplete/foreign query accepted")
				}
			})
		}
	}
}
func TestDIMSEGetReceiptsAndOriginalBytes(t *testing.T) {
	for _, syntax := range []string{explicitLittleEndian, implicitLittleEndian} {
		t.Run(syntax, func(t *testing.T) {
			cfg := retrievalTestConfig(t)
			p, _, _ := authorizeRetrieval(cfg, testSignedReferral(t, cfg))
			object := dimseFixture(syntax, "ACC", "1.2.3", "1.2.3.4")
			arrived := make(chan struct{}, 2)
			allow := make(chan struct{})
			var uploads atomic.Int32
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, e := io.ReadAll(r.Body)
				if e != nil || !bytes.Equal(b, object) || r.Header.Get("X-Telrad-Retrieval-Attempt") != "attempt" {
					t.Error("changed bytes or wrong correlation")
				}
				uploads.Add(1)
				arrived <- struct{}{}
				<-allow
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				io.WriteString(w, `{"status":"accepted","receiptId":"receipt"}`)
			}))
			defer cloud.Close()
			cfg.DicomURL = cloud.URL
			pacs := dimseTestPeer(t, syntax, true, func(a *retrievalAssociation, _ []byte) {
				data := dimseTestRequest(t, a, 0x0010)
				if !bytes.Contains(data, []byte("1.2.3")) || bytes.Contains(data, []byte("ACC")) {
					t.Error("C-GET did not use selected study UID")
				}
				for i := 0; i < 2; i++ {
					a.send(1, true, dimseResponse(studyRootGet, 0x8010, 0xff00, false, i))
					dimseStore(t, a, object, true)
					<-arrived
					if i == 0 {
						a.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
						var one [1]byte
						n, e := a.conn.Read(one[:])
						if n != 0 || e == nil {
							t.Error("acknowledged before cloud receipt")
						}
						a.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
						close(allow)
					}
					_, f, e := a.command()
					status, ok := commandUS(f, 0x0900)
					message, _ := commandUS(f, 0x0120)
					if e != nil || !ok || status != 0 || message != 7 {
						t.Error("missing correlated store success", e)
						return
					}
				}
				a.send(1, true, dimseResponse(studyRootGet, 0x8010, 0, false, 2))
				dimseTestRelease(t, a)
			})
			var progress retrievalProgress
			result, e := retrieveCGET(context.Background(), cloud.Client(), cfg, testProvider(t, testCredential('A')), newRuntimeStatus(cfg.configPath), pacs, p, "1.2.3", "attempt", func() error { return nil }, &progress)
			if e != nil || result.UniqueInstanceCount != 1 || uploads.Load() != 2 || progress.uploaded.Load() != 2 {
				t.Fatalf("get failed: %v, %+v, uploads=%d", e, result, uploads.Load())
			}
		})
	}
}

func TestDIMSEGetRejectsIncompleteTransfers(t *testing.T) {
	for _, scenario := range []string{"receipt", "foreign accession", "warning", "failed count", "wrong message", "missing role", "truncated dataset"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := retrievalTestConfig(t)
			p, _, _ := authorizeRetrieval(cfg, testSignedReferral(t, cfg))
			var uploads atomic.Int32
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				uploads.Add(1)
				if err != nil {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario != "receipt" {
					w.WriteHeader(201)
				}
				io.WriteString(w, `{"status":"accepted","receiptId":"receipt"}`)
			}))
			defer cloud.Close()
			cfg.DicomURL = cloud.URL
			pacs := dimseTestPeer(t, explicitLittleEndian, scenario != "missing role", func(a *retrievalAssociation, _ []byte) {
				if scenario == "missing role" {
					return
				}
				dimseTestRequest(t, a, 0x0010)
				accession := "ACC"
				if scenario == "foreign accession" {
					accession = "BAD"
				}
				object := dimseFixture(explicitLittleEndian, accession, "1.2.3", "1.2.3.4")
				if scenario == "truncated dataset" {
					object = object[:len(object)-1]
				}
				dimseStore(t, a, object, false)
				_, fields, e := a.command()
				if scenario == "receipt" || scenario == "foreign accession" || scenario == "truncated dataset" {
					if e == nil {
						status, _ := commandUS(fields, 0x0900)
						if status == 0 {
							t.Error("failed object acknowledged as success")
						}
					}
					return
				}
				if e != nil {
					t.Error(e)
					return
				}
				status := uint16(0)
				if scenario == "warning" {
					status = 0xb000
				}
				response := dimseResponse(studyRootGet, 0x8010, status, false, 1)
				if scenario == "failed count" {
					response[len(response)-12] = 1
				} // Failed suboperation count.
				if scenario == "wrong message" {
					i := bytes.Index(response, []byte{0, 0, 0x20, 1, 2, 0, 0, 0})
					binary.LittleEndian.PutUint16(response[i+8:], 2)
				}
				a.send(1, true, response)
			})
			var progress retrievalProgress
			result, e := retrieveCGET(context.Background(), cloud.Client(), cfg, testProvider(t, testCredential('A')), newRuntimeStatus(cfg.configPath), pacs, p, "1.2.3", "attempt", func() error { return nil }, &progress)
			if e == nil || result.UniqueInstanceCount != 0 {
				t.Fatal("failed retrieval reported success")
			}
			if (scenario == "foreign accession" || scenario == "missing role") && uploads.Load() != 0 {
				t.Fatal("unauthorized object forwarded")
			}
		})
	}
}

func TestDIMSEGetCancellationClosesBlockedUpload(t *testing.T) {
	cfg := retrievalTestConfig(t)
	p, _, _ := authorizeRetrieval(cfg, testSignedReferral(t, cfg))
	started := make(chan struct{})
	cloudDone := make(chan struct{})
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		close(cloudDone)
	}))
	defer cloud.Close()
	cfg.DicomURL = cloud.URL
	pacs := dimseTestPeer(t, implicitLittleEndian, true, func(a *retrievalAssociation, _ []byte) {
		dimseTestRequest(t, a, 0x0010)
		dimseStore(t, a, dimseFixture(implicitLittleEndian, "ACC", "1.2.3", "1.2.3.4"), false)
		_, fields, e := a.command()
		if e == nil {
			status, _ := commandUS(fields, 0x0900)
			if status == 0 {
				t.Error("cancelled upload acknowledged")
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var progress retrievalProgress
		_, e := retrieveCGET(ctx, cloud.Client(), cfg, testProvider(t, testCredential('A')), newRuntimeStatus(cfg.configPath), pacs, p, "1.2.3", "attempt", func() error { return nil }, &progress)
		done <- e
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	cancel()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("cancelled transfer succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop transfer")
	}
	select {
	case <-cloudDone:
	case <-time.After(time.Second):
		t.Fatal("upload request not cancelled")
	}
}

func TestDIMSELocalConfigurationAndIdentifierBounds(t *testing.T) {
	cfg := retrievalTestConfig(t)
	cfg.Retrieval.PACS[0] = retrievalPACS{ID: "pacs", Adapter: "dimse-find-get-v1", Host: "pacs.example.invalid", Port: 104, CalledAETitle: "PACS", CallingAETitle: "RELAY", StorageSOPClasses: []string{syntheticStorageClass}, AccessionIssuer: "CLINIC", MaxStudyBytes: 1024 * 1024, MaxInstanceBytes: 1024 * 1024, MaxInstances: 100, RequestTimeoutSeconds: 5}
	if e := validateRetrievalConfig(cfg); e != nil {
		t.Fatal(e)
	}
	original := cfg.Retrieval.PACS[0]
	for _, edit := range []func(*retrievalPACS){func(p *retrievalPACS) { p.Host = "https://pacs" }, func(p *retrievalPACS) { p.Port = 0 }, func(p *retrievalPACS) { p.CallingAETitle = strings.Repeat("A", 17) }, func(p *retrievalPACS) { p.StorageSOPClasses = []string{studyRootFind} }, func(p *retrievalPACS) { p.Adapter = "unsupported" }} {
		cfg.Retrieval.PACS[0] = original
		edit(&cfg.Retrieval.PACS[0])
		if validateRetrievalConfig(cfg) == nil {
			t.Fatal("invalid local DIMSE configuration accepted")
		}
	}
	p := retrieval.Permit{Examination: retrieval.Examination{Accession: strings.Repeat("A", 17)}}
	if _, e := queryDIMSEAccession(context.Background(), original, p); e != retrieval.ErrPolicy {
		t.Fatal("oversized accession initiated a query")
	}
}

// dimseWorkflowPACS serves repeated discovery and retrieval associations so
// orchestration tests exercise leases and restart over the actual DIMSE path.
func dimseWorkflowPACS(t *testing.T, discover func() []string, copies int) retrievalPACS {
	return dimseTestPeer(t, explicitLittleEndian, true, func(a *retrievalAssociation, _ []byte) {
		if a.contexts[1].AbstractSyntax == studyRootFind {
			data := dimseTestRequest(t, a, 0x0020)
			if !bytes.Contains(data, []byte("ACC")) {
				t.Error("missing accession query")
				return
			}
			for _, study := range discover() {
				identifier := retrievalElement(0x00080050, "SH", "ACC", explicitLittleEndian)
				identifier = append(identifier, retrievalElement(0x0020000d, "UI", study, explicitLittleEndian)...)
				a.send(1, true, dimseResponse(studyRootFind, 0x8020, 0xff00, true, 0))
				a.send(1, false, identifier)
			}
			a.send(1, true, dimseResponse(studyRootFind, 0x8020, 0, false, 0))
			dimseTestRelease(t, a)
			return
		}
		data := dimseTestRequest(t, a, 0x0010)
		var study string
		for len(data) >= 8 {
			n := int(binary.LittleEndian.Uint16(data[6:8]))
			if len(data) < 8+n {
				t.Error("truncated request")
				return
			}
			if binary.LittleEndian.Uint16(data) == 0x0020 && binary.LittleEndian.Uint16(data[2:]) == 0x000d {
				study = strings.TrimRight(string(data[8:8+n]), "\x00 ")
			}
			data = data[8+n:]
		}
		if !retrieval.UID(study) {
			t.Error("missing selected study")
			return
		}
		sop := study + ".1.1"
		for i := 0; i < copies; i++ {
			dimseStore(t, a, dimseFixture(explicitLittleEndian, "ACC", study, sop), false, sop)
			_, fields, err := a.command()
			status, ok := commandUS(fields, 0x0900)
			if err != nil || !ok || status != 0 {
				return
			} // Cancellation/lease expiry aborts the association.
		}
		a.send(1, true, dimseResponse(studyRootGet, 0x8010, 0, false, copies))
		dimseTestRelease(t, a)
	}, true)
}
