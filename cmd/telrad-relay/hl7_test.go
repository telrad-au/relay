package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestReadMLLPFrameBoundaries(t *testing.T) {
	for _, size := range []int{32*1024 - 1, 32*1024 - 2, 100000} {
		payload := bytes.Repeat([]byte{'x'}, size)
		input := frameMessage(payload)
		frame, err := readMLLPFrame(bufio.NewReaderSize(bytes.NewReader(input), 32*1024), int64(size))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(frame, input) {
			t.Fatal("frame changed across a reader-buffer boundary")
		}
	}
}

func TestReadMLLPFrameRejectsMalformedInput(t *testing.T) {
	tests := map[string][]byte{
		"over limit":            frameMessage([]byte("abcd")),
		"invalid start":         {'x', 'a', mllpEnd, mllpCR},
		"invalid byte after FS": {mllpStart, 'a', mllpEnd, 'x'},
		"EOF after FS":          {mllpStart, 'a', mllpEnd},
	}
	for name, input := range tests {
		if _, err := readMLLPFrame(bufio.NewReader(bytes.NewReader(input)), 3); err == nil {
			t.Fatalf("%s: malformed frame accepted", name)
		}
	}
	if frame, err := readMLLPFrame(bufio.NewReader(bytes.NewReader(frameMessage([]byte("abc")))), 3); err != nil || string(unframe(frame)) != "abc" {
		t.Fatalf("exact-limit frame rejected: %v", err)
	}
}

func TestParseHL7Fields(t *testing.T) {
	message, err := parseHL7([]byte(testOrder))
	if err != nil {
		t.Fatal(err)
	}
	if message.controlID() != "MSG0001" || message.orderControl() != "NW" {
		t.Fatalf("control=%q order=%q", message.controlID(), message.orderControl())
	}
	if got := message.accessions(); len(got) != 1 || got[0] != "ACC0001" {
		t.Fatalf("accessions=%v", got)
	}
	if value, _ := message.field("MSH", 1); value != "|" {
		t.Fatalf("MSH-1=%q", value)
	}
	if value, _ := message.field("MSH", 2); value != `^~\&` {
		t.Fatalf("MSH-2=%q", value)
	}
	if value, _ := message.field("MSH", 9); value != "ORM^O01^ORM_O01" {
		t.Fatalf("MSH-9=%q", value)
	}
	if value, _ := message.field("MSH", 11); value != "P" {
		t.Fatalf("MSH-11=%q", value)
	}
	if _, ok := message.field("ZZZ", 1); ok {
		t.Fatal("missing segment reported present")
	}
}

func TestParseHL7MultipleOBRAndCustomSeparators(t *testing.T) {
	custom := "MSH#^~\\&#RIS#CLINIC#TELRAD#TELRAD#20260927101500##ORM^O01#MSG0002#P#2.3\rORC#XO#P2\rOBR#1#P2#F2#X##############ACC-A\rOBR#2#P2#F3#X##############ACC-B \r"
	message, err := parseHL7([]byte(custom))
	if err != nil {
		t.Fatal(err)
	}
	if message.fieldSeparator != '#' || message.controlID() != "MSG0002" || message.orderControl() != "XO" {
		t.Fatalf("separator=%q control=%q order=%q", message.fieldSeparator, message.controlID(), message.orderControl())
	}
	if got := strings.Join(message.accessions(), ","); got != "ACC-A,ACC-B" {
		t.Fatalf("accessions=%q", got)
	}
}

func TestParseHL7Rejections(t *testing.T) {
	for name, input := range map[string]string{
		"no MSH":    "PID|1||X\r",
		"short MSH": "MSH|^~\r",
	} {
		if _, err := parseHL7([]byte(input)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// Clinics commonly send ISO 8859-1 text. Relay reads only ASCII delimiters,
// so such an order must still be tracked; only a non-UTF-8 accession value
// itself is unrecordable.
func TestParseHL7AcceptsLatin1AndFiltersUnrecordableAccessions(t *testing.T) {
	latin1 := strings.Replace(testOrder, "TEST^PATIENT", "M\xfcLLER^J\xd6RG", 1) + "OBR|2|P|F|X||||||||||||||ACC\xe9\r"
	message, err := parseHL7([]byte(latin1))
	if err != nil {
		t.Fatalf("ISO 8859-1 order rejected: %v", err)
	}
	if message.controlID() != "MSG0001" || message.orderControl() != "NW" {
		t.Fatalf("control=%q order=%q", message.controlID(), message.orderControl())
	}
	if got := strings.Join(recordableAccessions(message.accessions()), ","); got != "ACC0001" {
		t.Fatalf("recordable accessions=%q", got)
	}
}

func TestAcknowledgementParsing(t *testing.T) {
	ack, err := parseHL7(ackFor([]byte(testOrder), "AA"))
	if err != nil {
		t.Fatal(err)
	}
	code, controlID, ok := ack.acknowledgement()
	if !ok || code != "AA" || controlID != "MSG0001" {
		t.Fatalf("code=%q control=%q ok=%t", code, controlID, ok)
	}
	order, _ := parseHL7([]byte(testOrder))
	if _, _, ok := order.acknowledgement(); ok {
		t.Fatal("an order was treated as an acknowledgement")
	}
	bad, _ := parseHL7([]byte("MSH|^~\\&|A|B|C|D|1||ACK|X|P|2.5\rMSA|XX|MSG0001\r"))
	if _, _, ok := bad.acknowledgement(); ok {
		t.Fatal("unknown MSA-1 accepted")
	}
}

func TestComposeAck(t *testing.T) {
	report, _ := parseHL7([]byte(testReport))
	ack := composeAck(report, "AR", "bad|text^here\rZZZ")
	parsed, err := parseHL7(ack)
	if err != nil {
		t.Fatalf("composed ACK does not parse: %v", err)
	}
	if len(parsed.segments) != 2 {
		t.Fatalf("segments=%d: %q", len(parsed.segments), ack)
	}
	code, controlID, ok := parsed.acknowledgement()
	if !ok || code != "AR" || controlID != "RPT0001" {
		t.Fatalf("code=%q control=%q", code, controlID)
	}
	for wanted, expected := range map[int]string{3: "RIS", 4: "CLINIC", 5: "TELRAD", 6: "TELRAD", 9: "ACK^R01^ACK", 11: "P", 12: "2.5.1"} {
		if value, _ := parsed.field("MSH", wanted); value != expected {
			t.Fatalf("MSH-%d=%q want %q", wanted, value, expected)
		}
	}
	if text, _ := parsed.field("MSA", 3); text != "bad text here ZZZ" {
		t.Fatalf("MSA-3=%q", text)
	}
	if id := parsed.controlID(); id == "" || id == "RPT0001" {
		t.Fatalf("ACK control ID %q must be fresh", id)
	}
}
