package synthetic

import (
	"bufio"
	"bytes"
	"io"
	"testing"
)

func TestSharedFixtureStreamingMetadata(t *testing.T) {
	for _, dimensions := range [][2]int{{512, 512}, {512, 1024}, {128, 256}} {
		file := DICOM("1.2.3.4", "1.2.3.5", dimensions[0], dimensions[1])
		r := bufio.NewReader(bytes.NewReader(file))
		m, err := Part10(r)
		if err != nil || m.Instance != "1.2.3.4" || m.Class != SecondaryCapture || m.Syntax != ExplicitVRLittleEndian {
			t.Fatalf("metadata: %+v %v", m, err)
		}
		dataset, err := io.ReadAll(r)
		if err != nil || !bytes.HasSuffix(dataset, Pixels(dimensions[0], dimensions[1])) {
			t.Fatal("streamed dataset changed")
		}
	}
}

func TestMLLPLongLivedFramingAndLimits(t *testing.T) {
	first, second := HL7("one", 1024), HL7("two", 64*1024)
	r := bufio.NewReaderSize(bytes.NewReader(append(Frame(first), Frame(second)...)), 64)
	for _, expected := range [][]byte{first, second} {
		got, err := ReadFrame(r, len(expected))
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatal("coalesced MLLP exchange changed")
		}
	}
	if _, err := ReadFrame(bufio.NewReader(bytes.NewReader(Frame(second))), len(second)-1); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
