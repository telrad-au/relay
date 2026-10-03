package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDICOMPassThroughIsByteExactAndMutuallyAuthenticated(t *testing.T) {
	pki := newTestPKI(t)
	dicom := pki.mutualTLSListener(t)
	echoServer(t, dicom)
	r := newTestRelay(t, pki, dicom, nil, nil)
	clinic := serveClinicListener(t, r.relay, r.serveDICOM)

	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := make([]byte, 300*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = conn.Write(payload)
		closeWrite(conn)
	}()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var echoed bytes.Buffer
	buffer := make([]byte, 64*1024)
	for echoed.Len() < len(payload) {
		count, err := conn.Read(buffer)
		echoed.Write(buffer[:count])
		if err != nil {
			break
		}
	}
	if !bytes.Equal(echoed.Bytes(), payload) {
		t.Fatalf("echoed %d bytes, want %d identical", echoed.Len(), len(payload))
	}
	// Half close propagated: the echo server closes after EOF, so we read EOF.
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("expected EOF after half close")
	}
	report := r.status.snapshot()
	if report.Telrad.LastDicom == nil {
		t.Fatal("status did not record the Telrad DICOM connection")
	}
}

func TestDICOMWithoutUpstreamClosesClinicConnection(t *testing.T) {
	pki := newTestPKI(t)
	r := newTestRelay(t, pki, nil, nil, nil)
	clinic := serveClinicListener(t, r.relay, r.serveDICOM)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("clinic connection stayed open without an upstream")
	}
	if !r.status.snapshot().LastTelradFailure["dicom"] {
		t.Fatal("status did not record the failure")
	}
}

func TestUnauthenticatedClientIsRefusedByTelrad(t *testing.T) {
	pki := newTestPKI(t)
	dicom := pki.mutualTLSListener(t)
	echoServer(t, dicom)
	r := newTestRelay(t, pki, dicom, nil, nil)
	// Break the identity: a certificate from an unrelated CA.
	other := newTestPKI(t)
	cfg := testConfig(t, pki)
	r.store = pairedStore(t, cfg, other, r.store.endpoints())
	clinic := serveClinicListener(t, r.relay, r.serveDICOM)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hello"))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("Telrad accepted a certificate from another authority")
	}
}

func TestHL7OrderRecordsLedgerOnlyAfterAA(t *testing.T) {
	pki := newTestPKI(t)
	hl7 := pki.mutualTLSListener(t)
	received := hl7Responder(t, hl7, func(message []byte) []byte {
		parsed, _ := parseHL7(message)
		switch parsed.controlID() {
		case "REJECT":
			return ackFor(message, "AR")
		case "ERROR":
			return ackFor(message, "AE")
		}
		return ackFor(message, "AA")
	})
	r := newTestRelay(t, pki, nil, hl7, nil)
	clinic := serveClinicListener(t, r.relay, r.serveHL7)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	ack := mllpExchange(t, conn, reader, []byte(testOrder))
	if !bytes.Equal(ack, ackFor([]byte(testOrder), "AA")) {
		t.Fatalf("ACK altered: %q", ack)
	}
	if !bytes.Equal(received.last(), []byte(testOrder)) {
		t.Fatal("order altered on the way to Telrad")
	}
	if !r.ledger.contains("ACC0001") {
		t.Fatal("accepted NW order not recorded")
	}

	rejected := withControlID(testOrder, "REJECT")
	rejected = replaceOnce(rejected, "ACC0001", "ACC-REJ")
	if ack := mllpExchange(t, conn, reader, []byte(rejected)); !bytes.HasSuffix(ack, []byte("MSA|AR|REJECT\r")) {
		t.Fatalf("AR not forwarded: %q", ack)
	}
	if r.ledger.contains("ACC-REJ") {
		t.Fatal("rejected order recorded")
	}

	errored := replaceOnce(withControlID(testOrder, "ERROR"), "ACC0001", "ACC-ERR")
	mllpExchange(t, conn, reader, []byte(errored))
	if r.ledger.contains("ACC-ERR") {
		t.Fatal("AE order recorded")
	}

	cancel := replaceOnce(replaceOnce(withControlID(testOrder, "CANCEL"), "ORC|NW|", "ORC|CA|"), "ACC0001", "ACC-CA")
	mllpExchange(t, conn, reader, []byte(cancel))
	if r.ledger.contains("ACC-CA") {
		t.Fatal("cancellation recorded")
	}
	cancelExisting := replaceOnce(withControlID(testOrder, "CANCEL2"), "ORC|NW|", "ORC|CA|")
	mllpExchange(t, conn, reader, []byte(cancelExisting))
	if !r.ledger.contains("ACC0001") {
		t.Fatal("cancellation removed a ledger entry")
	}

	multi := withControlID(testOrder, "MULTI") + "OBR|2|PLACER0001|FILLER0002|CT002^CT Neck||20260927101500||||||||||||ACC0002\r"
	multi = replaceOnce(replaceOnce(multi, "ORC|NW|", "ORC|XO|"), "ACC0001", "ACC0001B")
	mllpExchange(t, conn, reader, []byte(multi))
	if !r.ledger.contains("ACC0001B") || !r.ledger.contains("ACC0002") {
		t.Fatal("multi-OBR XO order not fully recorded")
	}
	if received.count() != 6 {
		t.Fatalf("Telrad saw %d messages, want 6", received.count())
	}
	data, _ := os.ReadFile(filepath.Join(r.cfg.DataDir, ledgerFileName))
	if string(data) != "ACC0001\nACC0001B\nACC0002\n" {
		t.Fatalf("ledger file=%q", data)
	}
}

func TestHL7PipelinedMessagesCorrelateByControlID(t *testing.T) {
	pki := newTestPKI(t)
	hl7 := pki.mutualTLSListener(t)
	// Answer in reverse order to prove correlation is by MSA-2, not arrival.
	var batch [][]byte
	hl7Responder(t, hl7, func(message []byte) []byte {
		batch = append(batch, message)
		if len(batch) < 2 {
			return nil
		}
		first, second := batch[0], batch[1]
		batch = nil
		go func() {}()
		return append(append([]byte{}, ackFor(second, "AA")...), append([]byte{mllpEnd, mllpCR, mllpStart}, ackFor(first, "AA")...)...)
	})
	r := newTestRelay(t, pki, nil, hl7, nil)
	clinic := serveClinicListener(t, r.relay, r.serveHL7)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	one := replaceOnce(withControlID(testOrder, "ONE"), "ACC0001", "ACC-ONE")
	two := replaceOnce(withControlID(testOrder, "TWO"), "ACC0001", "ACC-TWO")
	if _, err := conn.Write(append(frameMessage([]byte(one)), frameMessage([]byte(two))...)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for range 2 {
		if _, err := readMLLPFrame(reader, 1024*1024); err != nil {
			t.Fatal(err)
		}
	}
	if !r.ledger.contains("ACC-ONE") || !r.ledger.contains("ACC-TWO") {
		t.Fatal("out-of-order acknowledgements not correlated")
	}
}

func TestHL7LedgerWriteFailureClosesWithoutForwardingAA(t *testing.T) {
	pki := newTestPKI(t)
	hl7 := pki.mutualTLSListener(t)
	hl7Responder(t, hl7, func(message []byte) []byte { return ackFor(message, "AA") })
	r := newTestRelay(t, pki, nil, hl7, nil)
	r.ledger.close() // every append now fails
	clinic := serveClinicListener(t, r.relay, r.serveHL7)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(frameMessage([]byte(testOrder))); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := readMLLPFrame(bufio.NewReader(conn), 1024*1024); err == nil {
		t.Fatal("AA forwarded although the ledger could not record it")
	}
	if r.status.snapshot().LedgerError == "" {
		t.Fatal("ledger failure not reported")
	}
}

func TestHL7MalformedFrameClosesBothSides(t *testing.T) {
	pki := newTestPKI(t)
	hl7 := pki.mutualTLSListener(t)
	log := hl7Responder(t, hl7, func(message []byte) []byte { return ackFor(message, "AA") })
	r := newTestRelay(t, pki, nil, hl7, nil)
	r.cfg.HL7MaxBytes = 64
	clinic := serveClinicListener(t, r.relay, r.serveHL7)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write(frameMessage(bytes.Repeat([]byte{'x'}, 65))); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("oversize frame did not close the connection")
	}
	if log.count() != 0 {
		t.Fatal("oversize frame reached Telrad")
	}
}

func TestListenerLimitRefusesExcessConnections(t *testing.T) {
	pki := newTestPKI(t)
	r := newTestRelay(t, pki, nil, nil, nil)
	release := make(chan struct{})
	handler := func(ctx context.Context, conn net.Conn) { <-release }
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.serveListener(ctx, listener, 2, "hl7", handler)
	var conns []net.Conn
	for range 3 {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		defer conn.Close()
	}
	waitFor(t, func() bool { return r.status.snapshot().RefusedConnections["hl7"] == 1 })
	if r.status.snapshot().Connections["hl7"] != 2 {
		t.Fatalf("active=%d", r.status.snapshot().Connections["hl7"])
	}
	close(release)
	waitFor(t, func() bool { return r.status.snapshot().Connections["hl7"] == 0 })
}

// dialHL7 starts an HL7 relay handler against a fake Telrad listener that
// answers with respond, after letting the caller adjust the configuration.
func dialHL7(t *testing.T, respond func([]byte) []byte, configure func(*config)) (net.Conn, *testRelay, *messageLog) {
	t.Helper()
	pki := newTestPKI(t)
	hl7 := pki.mutualTLSListener(t)
	log := hl7Responder(t, hl7, respond)
	r := newTestRelay(t, pki, nil, hl7, nil)
	configure(r.cfg)
	clinic := serveClinicListener(t, r.relay, r.serveHL7)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, r, log
}

// waitClosed reads until the relay closes conn and returns how long that took.
func waitClosed(t *testing.T, conn net.Conn, within time.Duration) time.Duration {
	t.Helper()
	started := time.Now()
	_ = conn.SetReadDeadline(started.Add(within))
	buffer := make([]byte, 256)
	for {
		if _, err := conn.Read(buffer); err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				t.Fatalf("connection still open after %v", within)
			}
			return time.Since(started)
		}
	}
}

func TestHL7IdleConnectionIsClosed(t *testing.T) {
	conn, _, _ := dialHL7(t, func(message []byte) []byte { return ackFor(message, "AA") }, func(cfg *config) { cfg.IdleTimeoutSeconds = 1 })
	mllpExchange(t, conn, bufio.NewReader(conn), []byte(testOrder))
	if elapsed := waitClosed(t, conn, 5*time.Second); elapsed < 900*time.Millisecond {
		t.Fatalf("closed after %v, before the idle timeout", elapsed)
	}
}

func TestHL7IncompleteFrameClosesAtFrameDeadline(t *testing.T) {
	conn, _, log := dialHL7(t, func(message []byte) []byte { return ackFor(message, "AA") }, func(cfg *config) {
		cfg.HL7FrameSeconds = 1
		cfg.IdleTimeoutSeconds = 30
	})
	if _, err := conn.Write([]byte{mllpStart, 'M', 'S', 'H', '|'}); err != nil {
		t.Fatal(err)
	}
	if elapsed := waitClosed(t, conn, 5*time.Second); elapsed < 900*time.Millisecond {
		t.Fatalf("closed after %v, before the frame deadline", elapsed)
	}
	if log.count() != 0 {
		t.Fatal("incomplete frame reached Telrad")
	}
}

func TestHL7UnansweredMessageClosesAtAckDeadline(t *testing.T) {
	conn, r, _ := dialHL7(t, func([]byte) []byte { return nil }, func(cfg *config) {
		cfg.TelradAckSeconds = 1
		cfg.IdleTimeoutSeconds = 30
	})
	if _, err := conn.Write(frameMessage([]byte(testOrder))); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, conn, 5*time.Second)
	if r.ledger.contains("ACC0001") {
		t.Fatal("unanswered order recorded")
	}
}

// Once every message is answered, a quiet connection is bounded by the idle
// timeout only, not by the acknowledgement deadline of an earlier message.
func TestHL7AnsweredConnectionOutlivesAckDeadline(t *testing.T) {
	conn, r, _ := dialHL7(t, func(message []byte) []byte { return ackFor(message, "AA") }, func(cfg *config) {
		cfg.TelradAckSeconds = 1
		cfg.IdleTimeoutSeconds = 30
	})
	reader := bufio.NewReader(conn)
	mllpExchange(t, conn, reader, []byte(testOrder))
	time.Sleep(1500 * time.Millisecond)
	second := replaceOnce(withControlID(testOrder, "MSG0002"), "ACC0001", "ACC0002")
	if ack := mllpExchange(t, conn, reader, []byte(second)); !bytes.HasSuffix(ack, []byte("MSA|AA|MSG0002\r")) {
		t.Fatalf("ack=%q", ack)
	}
	if !r.ledger.contains("ACC0002") {
		t.Fatal("second order not recorded")
	}
}

func TestHL7WithoutUpstreamClosesClinicConnection(t *testing.T) {
	pki := newTestPKI(t)
	r := newTestRelay(t, pki, nil, nil, nil)
	clinic := serveClinicListener(t, r.relay, r.serveHL7)
	conn, err := net.Dial("tcp", clinic.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitClosed(t, conn, 5*time.Second)
	if !r.status.snapshot().LastTelradFailure["hl7"] {
		t.Fatal("status did not record the failure")
	}
}

func TestHL7Latin1OrderIsForwardedUnchangedAndRecorded(t *testing.T) {
	conn, r, log := dialHL7(t, func(message []byte) []byte { return ackFor(message, "AA") }, func(*config) {})
	order := []byte(replaceOnce(testOrder, "TEST^PATIENT", "M\xfcLLER^J\xd6RG"))
	mllpExchange(t, conn, bufio.NewReader(conn), order)
	if !bytes.Equal(log.last(), order) {
		t.Fatal("ISO 8859-1 order altered")
	}
	if !r.ledger.contains("ACC0001") {
		t.Fatal("ISO 8859-1 order not recorded")
	}
}

// The data ports trust only the Telrad Relay CA pinned at pairing: a server
// the enrolment trust store accepts is refused when the pinned CA differs.
func TestDataPortsTrustOnlyThePinnedTelradCA(t *testing.T) {
	pki := newTestPKI(t)
	dicom := pki.mutualTLSListener(t)
	echoServer(t, dicom)
	cfg := testConfig(t, pki) // the enrolment roots trust the listener
	other := newTestPKI(t)
	endpoints := telradEndpoints{Host: "127.0.0.1", DicomPort: listenerPort(t, dicom), HL7Port: 1, ReportPort: 1, CACertificate: other.caPEM()}
	store := pairedStore(t, cfg, pki, endpoints)
	r := &relay{cfg: cfg, store: store, status: newStatusServer(cfg, store, nil)}
	conn, err := r.dialTelrad(context.Background(), endpoints.DicomPort, "dicom")
	if err == nil {
		conn.Close()
		t.Fatal("data port accepted a server outside the pinned CA")
	}
	if got := safeNetworkError(err).Error(); got != "tls_verification_failed" {
		t.Fatalf("err=%s", got)
	}

	cfg.rootCAs = x509.NewCertPool() // enrolment roots trust nothing; the pin suffices
	pinned := pairedStore(t, cfg, pki, telradEndpoints{Host: "127.0.0.1", DicomPort: endpoints.DicomPort, HL7Port: 1, ReportPort: 1})
	r = &relay{cfg: cfg, store: pinned, status: newStatusServer(cfg, pinned, nil)}
	conn, err = r.dialTelrad(context.Background(), endpoints.DicomPort, "dicom")
	if err != nil {
		t.Fatalf("pinned CA refused: %v", err)
	}
	conn.Close()
}
