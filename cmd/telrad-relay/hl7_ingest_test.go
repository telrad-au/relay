package main

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"
)

func TestReadMLLPFrameBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		payloadSize int
	}{
		{name: "FS starts next reader buffer", payloadSize: 32*1024 - 1},
		{name: "CR starts next reader buffer", payloadSize: 32*1024 - 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{'x'}, test.payloadSize)
			input := append([]byte{mllpStart}, payload...)
			input = append(input, mllpEnd, mllpCR)
			reader := bufio.NewReaderSize(bytes.NewReader(input), 32*1024)
			frame, err := readMLLPFrameFrom(reader, int64(len(payload)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frame, input) {
				t.Fatal("MLLP frame changed across a reader-buffer boundary")
			}
		})
	}
}

func TestReadMLLPFrameLimitAndTerminatorErrors(t *testing.T) {
	exact := []byte{mllpStart, 'a', 'b', 'c', mllpEnd, mllpCR}
	if frame, err := readMLLPFrame(bytes.NewReader(exact), 3); err != nil || !bytes.Equal(frame, exact) {
		t.Fatalf("exact-limit frame=%q error=%v", frame, err)
	}

	tests := []struct {
		name  string
		input []byte
		limit int64
	}{
		{name: "one byte over limit", input: []byte{mllpStart, 'a', 'b', 'c', 'd', mllpEnd, mllpCR}, limit: 3},
		{name: "invalid start", input: []byte{'x', 'a', mllpEnd, mllpCR}, limit: 3},
		{name: "invalid byte after FS", input: []byte{mllpStart, 'a', mllpEnd, 'x'}, limit: 3},
		{name: "EOF after FS", input: []byte{mllpStart, 'a', mllpEnd}, limit: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readMLLPFrame(bytes.NewReader(test.input), test.limit); err == nil {
				t.Fatal("malformed MLLP frame was accepted")
			}
		})
	}
}

func TestReadMLLPFrameLeavesCoalescedFrameBuffered(t *testing.T) {
	first := []byte{mllpStart, 'a', mllpEnd, mllpCR}
	second := []byte{mllpStart, 'b', mllpEnd, mllpCR}
	reader := bufio.NewReader(bytes.NewReader(append(append([]byte{}, first...), second...)))
	for index, want := range [][]byte{first, second} {
		got, err := readMLLPFrameFrom(reader, 1)
		if err != nil {
			t.Fatalf("frame %d: %v", index+1, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d=%q, want %q", index+1, got, want)
		}
	}
}

func TestHL7FieldParsingPreservesSeparatorsAndLineEndings(t *testing.T) {
	message := []byte("MSH^~\\&^CLINIC^A^TELRAD^B^20260101000000^^ORM~O01^control-custom^P^2.5\nPID^1\r")
	controlID, err := hl7ControlID(message)
	if err != nil || controlID != "control-custom" {
		t.Fatalf("control ID=%q error=%v", controlID, err)
	}

	ack := []byte("MSH^~\\&^TELRAD^B^CLINIC^A^20260101000001^^ACK^ack-custom^P^2.5\nMSA^AE^control-custom\r")
	code, receivedControlID, err := parseHL7Acknowledgement(ack)
	if err != nil || code != "AE" || receivedControlID != "control-custom" {
		t.Fatalf("ACK code=%q control ID=%q error=%v", code, receivedControlID, err)
	}
}

func TestHL7FieldParsingRejectsMissingRequiredFields(t *testing.T) {
	for name, message := range map[string][]byte{
		"missing MSH":    []byte("PID|1||SYNTHETIC\r"),
		"missing MSH-10": []byte("MSH|^~\\&|CLINIC|A|TELRAD|B|20260101000000||ORM^O01\r"),
		"invalid UTF-8":  {'M', 'S', 'H', '|', 0xff},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hl7ControlID(message); err == nil {
				t.Fatal("malformed HL7 message was accepted")
			}
		})
	}

	for name, ack := range map[string][]byte{
		"missing MSA":    []byte("MSH|^~\\&|TELRAD|B|CLINIC|A|20260101000001||ACK|ack|P|2.5\r"),
		"invalid status": []byte("MSA|CA|control-1\r"),
		"missing MSA-2":  []byte("MSA|AA|\r"),
		"invalid UTF-8":  {'M', 'S', 'A', '|', 'A', 'A', '|', 0xff},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseHL7Acknowledgement(ack); err == nil {
				t.Fatal("malformed HL7 ACK was accepted")
			}
		})
	}
}

var benchmarkHL7Frame []byte
var benchmarkHL7String string
var benchmarkHL7Code string

func BenchmarkReadMLLPFrame(b *testing.B) {
	for _, size := range []int{1024, 64 * 1024, 1024 * 1024, 8 * 1024 * 1024} {
		b.Run(benchmarkSizeName(size), func(b *testing.B) {
			payload := bytes.Repeat([]byte{'x'}, size)
			frame := append([]byte{mllpStart}, payload...)
			frame = append(frame, mllpEnd, mllpCR)
			var source bytes.Reader
			reader := bufio.NewReaderSize(&source, 32*1024)
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for range b.N {
				source.Reset(frame)
				reader.Reset(&source)
				result, err := readMLLPFrameFrom(reader, int64(size))
				if err != nil {
					b.Fatal(err)
				}
				benchmarkHL7Frame = result
			}
		})
	}
}

func BenchmarkHL7ControlID(b *testing.B) {
	for _, size := range []int{1024, 64 * 1024, 1024 * 1024, 8 * 1024 * 1024} {
		b.Run(benchmarkSizeName(size), func(b *testing.B) {
			message := benchmarkHL7Message(size)
			b.ReportAllocs()
			b.SetBytes(int64(len(message)))
			b.ResetTimer()
			for range b.N {
				controlID, err := hl7ControlID(message)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkHL7String = controlID
			}
		})
	}
}

func BenchmarkHL7Acknowledgement(b *testing.B) {
	for _, size := range []int{1024, 64 * 1024} {
		b.Run(benchmarkSizeName(size), func(b *testing.B) {
			ack := benchmarkHL7Acknowledgement(size)
			b.ReportAllocs()
			b.SetBytes(int64(len(ack)))
			b.ResetTimer()
			for range b.N {
				code, controlID, err := parseHL7Acknowledgement(ack)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkHL7Code = code
				benchmarkHL7String = controlID
			}
		})
	}
}

func benchmarkHL7Message(size int) []byte {
	prefix := []byte("MSH|^~\\&|CLINIC|A|TELRAD|B|20260101000000||ORU^R01|benchmark-control|P|2.5\rOBX|1|TX|NOTE||")
	return appendSizedHL7(prefix, size)
}

func benchmarkHL7Acknowledgement(size int) []byte {
	prefix := []byte("MSH|^~\\&|TELRAD|B|CLINIC|A|20260101000001||ACK|benchmark-ack|P|2.5\rMSA|AA|benchmark-control\rNTE|1||")
	return appendSizedHL7(prefix, size)
}

func appendSizedHL7(prefix []byte, size int) []byte {
	if size < len(prefix)+1 {
		panic("HL7 benchmark size is too small")
	}
	message := make([]byte, size)
	copy(message, prefix)
	for index := len(prefix); index < len(message)-1; index++ {
		message[index] = 'x'
	}
	message[len(message)-1] = '\r'
	return message
}

func benchmarkSizeName(size int) string {
	if size >= 1024*1024 {
		return fmt.Sprintf("%dMiB", size/(1024*1024))
	}
	return fmt.Sprintf("%dKiB", size/1024)
}
