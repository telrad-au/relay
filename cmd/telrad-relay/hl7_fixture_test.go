package main

import (
	_ "embed"
	"strings"
)

//go:embed testdata/hl7/oru-r01.hl7
var syntheticHL7IntegrityTemplate string

//go:embed testdata/hl7/ack.hl7
var syntheticHL7AcknowledgementTemplate string

func syntheticHL7IntegrityMessage(controlID string) []byte {
	message := strings.ReplaceAll(syntheticHL7IntegrityTemplate, "{{CONTROL_ID}}", controlID)
	return hl7TemplateToMessage(message)
}

func syntheticHL7Acknowledgement(code, controlID, ackControlID string) []byte {
	message := strings.NewReplacer(
		"{{ACK_CODE}}", code,
		"{{CONTROL_ID}}", controlID,
		"{{ACK_CONTROL_ID}}", ackControlID,
	).Replace(syntheticHL7AcknowledgementTemplate)
	return hl7TemplateToMessage(message)
}

func hl7TemplateToMessage(template string) []byte {
	template = strings.ReplaceAll(template, "\r\n", "\n")
	template = strings.TrimSuffix(template, "\n")
	return []byte(strings.ReplaceAll(template, "\n", "\r") + "\r")
}
