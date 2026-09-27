package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reportPort stands in for Telrad's report listener: the test writes framed
// reports on the accepted pickup connection and reads Relay's acknowledgements.
type reportPort struct {
	listener net.Listener
	conns    chan net.Conn
}

func newReportPort(t *testing.T, pki *testPKI) *reportPort {
	t.Helper()
	port := &reportPort{listener: pki.mutualTLSListener(t), conns: make(chan net.Conn, 4)}
	go func() {
		for {
			conn, err := port.listener.Accept()
			if err != nil {
				return
			}
			// Complete the handshake as Telrad would at accept, so the
			// relay's dial finishes without the test writing first.
			go func() {
				tlsConn := conn.(*tls.Conn)
				_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					_ = conn.Close()
					return
				}
				_ = tlsConn.SetDeadline(time.Time{})
				port.conns <- conn
			}()
		}
	}()
	return port
}

func (port *reportPort) accept(t *testing.T) net.Conn {
	t.Helper()
	select {
	case conn := <-port.conns:
		t.Cleanup(func() { conn.Close() })
		return conn
	case <-time.After(10 * time.Second):
		t.Fatal("relay did not connect to the report port")
		return nil
	}
}

// receiver is the clinic RIS report listener.
func startReceiver(t *testing.T, respond func([]byte) []byte) (net.Listener, *messageLog) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener, hl7Responder(t, listener, respond)
}

func startPickup(t *testing.T, r *testRelay) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.runPickup(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func TestPickupDeliversAuthorisedReportAndEchoesReceiverAck(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })
	r.cfg.ReportPort = listenerPort(t, receiver)
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	startPickup(t, r)
	telrad := port.accept(t)
	reader := bufio.NewReader(telrad)

	ack := mllpExchange(t, telrad, reader, []byte(testReport))
	if !bytes.Equal(ack, ackFor([]byte(testReport), "AA")) {
		t.Fatalf("receiver ACK not echoed byte for byte: %q", ack)
	}
	if !bytes.Equal(received.last(), []byte(testReport)) {
		t.Fatal("report altered on the way to the receiver")
	}
	status := r.status.snapshot()
	if status.Reports.Delivered != 1 || !status.Pickup.Connected {
		t.Fatalf("status=%+v", status.Reports)
	}

	// A receiver rejection is also passed through unchanged.
	receiver2, _ := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AR") })
	r.cfg.ReportPort = listenerPort(t, receiver2)
	if ack := mllpExchange(t, telrad, reader, []byte(testReport)); !bytes.Equal(ack, ackFor([]byte(testReport), "AR")) {
		t.Fatalf("receiver AR not echoed: %q", ack)
	}
	// So is a receiver application error, including its own MSA-3 text.
	receiverAE := func(message []byte) []byte {
		return append(ackFor(message, "AE"), []byte("ERR|||207^Application internal error^HL70357\r")...)
	}
	receiver3, _ := startReceiver(t, receiverAE)
	r.cfg.ReportPort = listenerPort(t, receiver3)
	if ack := mllpExchange(t, telrad, reader, []byte(testReport)); !bytes.Equal(ack, receiverAE([]byte(testReport))) {
		t.Fatalf("receiver AE not echoed: %q", ack)
	}
}

func TestPickupRefusesUnknownAccessionWithoutContactingReceiver(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })
	r.cfg.ReportPort = listenerPort(t, receiver)
	startPickup(t, r)
	telrad := port.accept(t)
	reader := bufio.NewReader(telrad)

	ack := mllpExchange(t, telrad, reader, []byte(testReport))
	parsed, err := parseHL7(ack)
	if err != nil {
		t.Fatal(err)
	}
	code, controlID, _ := parsed.acknowledgement()
	text, _ := parsed.field("MSA", 3)
	if code != "AR" || controlID != "RPT0001" || text != refusalText {
		t.Fatalf("ack=%q", ack)
	}
	// Partially authorised multi-OBR reports are refused too.
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	multi := testReport + "OBR|2|PLACER0001|FILLER0002|CT002^CT Neck||||||||||||||ACC-UNKNOWN|||||||F\r"
	if ack := mllpExchange(t, telrad, reader, []byte(multi)); !strings.Contains(string(ack), "MSA|AR|RPT0001|"+refusalText) {
		t.Fatalf("partial authorisation accepted: %q", ack)
	}
	noAccession := strings.Replace(testReport, "|ACC0001|", "||", 1)
	if ack := mllpExchange(t, telrad, reader, []byte(noAccession)); !strings.Contains(string(ack), "MSA|AR|") {
		t.Fatalf("report without accession accepted: %q", ack)
	}
	if received.count() != 0 {
		t.Fatal("receiver was contacted for a refused report")
	}
	if r.status.snapshot().Reports.Refused != 3 {
		t.Fatalf("refused=%d", r.status.snapshot().Reports.Refused)
	}
}

func TestPickupAnswersAEWhenReceiverUnavailableOrInvalid(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	closed, _ := startReceiver(t, nil)
	r.cfg.ReportPort = listenerPort(t, closed)
	closed.Close()
	startPickup(t, r)
	telrad := port.accept(t)
	reader := bufio.NewReader(telrad)

	ack := mllpExchange(t, telrad, reader, []byte(testReport))
	if !strings.Contains(string(ack), "MSA|AE|RPT0001|"+receiverFailureText) {
		t.Fatalf("ack=%q", ack)
	}
	// A receiver that answers with the wrong control ID is not trusted.
	wrong, _ := startReceiver(t, func(message []byte) []byte { return ackFor([]byte(withControlID(testReport, "OTHER")), "AA") })
	r.cfg.ReportPort = listenerPort(t, wrong)
	if ack := mllpExchange(t, telrad, reader, []byte(testReport)); !strings.Contains(string(ack), "MSA|AE|RPT0001|"+receiverFailureText) {
		t.Fatalf("ack=%q", ack)
	}
	if r.status.snapshot().Reports.Failed != 2 {
		t.Fatalf("failed=%d", r.status.snapshot().Reports.Failed)
	}
}

func TestPickupReconnectsAfterDropAndAfterRenewal(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	startPickup(t, r)
	first := port.accept(t)
	waitFor(t, func() bool { return r.status.snapshot().Pickup.Connected })
	first.Close()
	waitFor(t, func() bool { return !r.status.snapshot().Pickup.Connected })
	second := port.accept(t)
	waitFor(t, func() bool { return r.status.snapshot().Pickup.Connected })
	// A renewal closes the connection so the new certificate is presented.
	key, csr, _ := generateKeyAndCSR()
	certificate := pki.issueClient(t, csr, "relay-renewed", time.Hour)
	_, leaf, _ := parseCertificateChain(certificate, key)
	if err := r.store.install(key, &issuedIdentity{RelayID: "relay-renewed", Certificate: certificate, Telrad: r.store.endpoints(), leaf: leaf}); err != nil {
		t.Fatal(err)
	}
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatal("old pickup connection stayed open after renewal")
	}
	third, ok := port.accept(t).(*tls.Conn)
	if !ok {
		t.Fatal("report port did not accept a TLS connection")
	}
	_ = third.SetDeadline(time.Now().Add(5 * time.Second))
	if err := third.Handshake(); err != nil {
		t.Fatal(err)
	}
	if cn := third.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "relay-renewed" {
		t.Fatalf("new connection presented %q", cn)
	}
}

func TestPickupProtocolErrorClosesConnection(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	startPickup(t, r)
	telrad := port.accept(t)
	if _, err := telrad.Write(frameMessage([]byte("not hl7"))); err != nil {
		t.Fatal(err)
	}
	_ = telrad.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := telrad.Read(make([]byte, 1)); err == nil {
		t.Fatal("unparseable report did not close the connection")
	}
	port.accept(t) // it reconnects
}

// Relay keeps no delivery record: a report Telrad sends again is delivered again.
func TestPickupDeliversDuplicateReportAgain(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })
	r.cfg.ReportPort = listenerPort(t, receiver)
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	startPickup(t, r)
	telrad := port.accept(t)
	reader := bufio.NewReader(telrad)
	for range 2 {
		if ack := mllpExchange(t, telrad, reader, []byte(testReport)); !bytes.Equal(ack, ackFor([]byte(testReport), "AA")) {
			t.Fatalf("ack=%q", ack)
		}
	}
	if received.count() != 2 {
		t.Fatalf("receiver saw %d deliveries, want 2", received.count())
	}
}

// Failed dials back off exponentially from one second.
func TestPickupReconnectBacksOff(t *testing.T) {
	pki := newTestPKI(t)
	refusing, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refusing.Close() })
	attempts := make(chan time.Time, 16)
	go func() {
		for {
			conn, err := refusing.Accept()
			if err != nil {
				return
			}
			attempts <- time.Now()
			conn.Close() // the TLS handshake fails, so the dial fails
		}
	}()
	r := newTestRelay(t, pki, nil, nil, refusing)
	startPickup(t, r)
	var times []time.Time
	for len(times) < 3 {
		select {
		case at := <-attempts:
			times = append(times, at)
		case <-time.After(6 * time.Second):
			t.Fatalf("only %d attempts", len(times))
		}
	}
	first, second := times[1].Sub(times[0]), times[2].Sub(times[1])
	if first < 900*time.Millisecond || second < first+700*time.Millisecond {
		t.Fatalf("retry gaps %v then %v; want about 1s then 2s", first, second)
	}
	if r.status.snapshot().Pickup.Connected {
		t.Fatal("pickup reported connected")
	}
}

// Telrad must not write a second report before it has the acknowledgement.
func TestPickupInterleavedReportsCloseConnection(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })
	r.cfg.ReportPort = listenerPort(t, receiver)
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	startPickup(t, r)
	telrad := port.accept(t)
	if _, err := telrad.Write(append(frameMessage([]byte(testReport)), frameMessage([]byte(testReport))...)); err != nil {
		t.Fatal(err)
	}
	_ = telrad.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := telrad.Read(make([]byte, 1)); err == nil {
		t.Fatal("interleaved reports were answered")
	}
	if received.count() != 0 {
		t.Fatal("interleaved report delivered")
	}
}

// At shutdown a report whose receiver exchange has started is finished and
// acknowledged before the pickup connection closes.
func TestPickupShutdownFinishesReportInFlight(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte {
		time.Sleep(500 * time.Millisecond)
		return ackFor(message, "AA")
	})
	r.cfg.ReportPort = listenerPort(t, receiver)
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.runPickup(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	telrad := port.accept(t)
	if _, err := telrad.Write(frameMessage([]byte(testReport))); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return received.count() == 1 })
	cancel()
	_ = telrad.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(telrad)
	frame, err := readMLLPFrame(reader, 1024*1024)
	if err != nil {
		t.Fatalf("in-flight report not acknowledged at shutdown: %v", err)
	}
	if !bytes.Equal(unframe(frame), ackFor([]byte(testReport), "AA")) {
		t.Fatalf("ack=%q", unframe(frame))
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pickup did not stop after the report")
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("pickup connection stayed open after shutdown")
	}
}

// While the clinic's backlog window is open, the running pickup accepts a
// report for accessions not in the ledger, records them durably first and
// delivers the report unchanged. Once the window ends the gate is normal again
// and the expired file is removed.
func TestPickupAcceptBacklogRecordsUnlistedAccessions(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })
	r.cfg.ReportPort = listenerPort(t, receiver)
	if err := r.ledger.append([]string{"ACC0001"}); err != nil {
		t.Fatal(err)
	}
	startPickup(t, r)
	telrad := port.accept(t)
	reader := bufio.NewReader(telrad)
	unlisted := withControlID(strings.Replace(testReport, "|ACC0001|", "|ACC0002|", 1), "RPT0002")
	if ack := mllpExchange(t, telrad, reader, []byte(unlisted)); !strings.Contains(string(ack), "MSA|AR|RPT0002|"+refusalText) {
		t.Fatalf("unlisted report accepted without a window: %q", ack)
	}

	// Opened while the service runs; no restart.
	backlog := filepath.Join(r.cfg.DataDir, acceptBacklogFileName)
	if _, err := openAcceptBacklog(backlog, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ack := mllpExchange(t, telrad, reader, []byte(unlisted)); !bytes.Equal(ack, ackFor([]byte(unlisted), "AA")) {
		t.Fatalf("backlog report not delivered: %q", ack)
	}
	if !bytes.Equal(received.last(), []byte(unlisted)) {
		t.Fatal("backlog report altered on the way to the receiver")
	}
	multi := withControlID(testReport+"OBR|2|PLACER0003|FILLER0003|CT003^CT Neck||||||||||||||ACC0003|||||||F\r", "RPT0003")
	if ack := mllpExchange(t, telrad, reader, []byte(multi)); !bytes.Equal(ack, ackFor([]byte(multi), "AA")) {
		t.Fatalf("partly listed backlog report not delivered: %q", ack)
	}
	noAccession := strings.Replace(testReport, "|ACC0001|", "||", 1)
	if ack := mllpExchange(t, telrad, reader, []byte(noAccession)); !strings.Contains(string(ack), "MSA|AR|") {
		t.Fatalf("report without accession accepted under backlog: %q", ack)
	}
	data, err := os.ReadFile(filepath.Join(r.cfg.DataDir, ledgerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ACC0001\nACC0002\nACC0003\n" || r.status.snapshot().LedgerEntries != 3 {
		t.Fatalf("ledger=%q", data)
	}

	// An expired window admits nothing and is removed when found.
	if err := os.WriteFile(backlog, []byte(`{"until":"`+time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	late := withControlID(strings.Replace(testReport, "|ACC0001|", "|ACC0004|", 1), "RPT0004")
	if ack := mllpExchange(t, telrad, reader, []byte(late)); !strings.Contains(string(ack), "MSA|AR|RPT0004|"+refusalText) {
		t.Fatalf("report accepted after the window: %q", ack)
	}
	if _, err := os.Stat(backlog); !os.IsNotExist(err) {
		t.Fatalf("expired window file kept: %v", err)
	}
	if received.count() != 2 || r.ledger.contains("ACC0004") {
		t.Fatalf("receiver saw %d reports", received.count())
	}
}

// A backlog accession that cannot be recorded is never delivered: the pickup
// connection closes without an acknowledgement and Telrad retries.
func TestPickupAcceptBacklogLedgerFailureClosesWithoutDelivery(t *testing.T) {
	pki := newTestPKI(t)
	port := newReportPort(t, pki)
	r := newTestRelay(t, pki, nil, nil, port.listener)
	receiver, received := startReceiver(t, func(message []byte) []byte { return ackFor(message, "AA") })
	r.cfg.ReportPort = listenerPort(t, receiver)
	if _, err := openAcceptBacklog(filepath.Join(r.cfg.DataDir, acceptBacklogFileName), 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.ledger.file.Close()
	startPickup(t, r)
	telrad := port.accept(t)
	if _, err := telrad.Write(frameMessage([]byte(testReport))); err != nil {
		t.Fatal(err)
	}
	_ = telrad.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := telrad.Read(make([]byte, 1)); err == nil {
		t.Fatal("report acknowledged although its accession was not recorded")
	}
	if received.count() != 0 || r.status.snapshot().LedgerError == "" {
		t.Fatal("report delivered or ledger failure not shown")
	}
}
