package synthetic

import (
	_ "embed"
	"strings"
)

//go:embed hl7/oru-r01.hl7
var reportTemplate string

//go:embed hl7/ack.hl7
var ackTemplate string

// HL7 extends the validated OBX text field, keeping the original message shape.
// size is a minimum; zero returns the unpadded shared integrity fixture.
func HL7(id string, size int) []byte {
	s := message(strings.ReplaceAll(reportTemplate, "{{CONTROL_ID}}", id))
	if len(s) < size {
		marker := "café"
		s = strings.Replace(s, marker, marker+strings.Repeat("x", size-len(s)), 1)
	}
	return []byte(s)
}

func ACK(code, id, ackID string) []byte {
	return []byte(message(strings.NewReplacer("{{ACK_CODE}}", code, "{{CONTROL_ID}}", id, "{{ACK_CONTROL_ID}}", ackID).Replace(ackTemplate)))
}

func message(s string) string {
	return strings.ReplaceAll(strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), "\n", "\r") + "\r"
}

// Report applies the cloud report-return accession contract to the shared
// protocol fixture. Inbound HL7 integrity fixtures retain their original shape.
func Report(id string, size int) []byte {
	segments := strings.Split(string(HL7(id, 0)), "\r")
	for i, segment := range segments {
		fields := strings.Split(segment, "|")
		if fields[0] == "OBR" {
			fields[2] = ""
			fields[3] = ""
			fields[18] = "RELAY-REPORT"
			segments[i] = strings.Join(fields, "|")
		}
	}
	payload := strings.Join(segments, "\r")
	if len(payload) < size {
		payload = strings.Replace(payload, "café", "café"+strings.Repeat("x", size-len(payload)), 1)
	}
	return []byte(payload)
}
