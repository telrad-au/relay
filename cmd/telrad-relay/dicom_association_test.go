package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestDICOMCalledAETitles(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		accepted bool
	}{
		{name: "documented example", title: "TELRAD", accepted: true},
		{name: "alternative", title: "TELERAD", accepted: true},
		{name: "lowercase", title: "clinic_archive", accepted: true},
		{name: "single character", title: "A", accepted: true},
		{name: "maximum length", title: "1234567890123456", accepted: true},
		{name: "space padding", title: "  CLINIC  ", accepted: true},
		{name: "embedded space and punctuation", title: "Clinic PACS-1!", accepted: true},
		{name: "blank", title: ""},
		{name: "backslash", title: "CLINIC\\ARCHIVE"},
		{name: "null", title: "CLINIC\x00"},
		{name: "tab padding", title: "\tTELRAD"},
		{name: "newline padding", title: "TELRAD\n"},
		{name: "delete", title: "CLINIC\x7f"},
		{name: "non ASCII", title: "CLINIC\x80"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clinic, relay := net.Pipe()
			defer clinic.Close()
			defer relay.Close()
			if err := clinic.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go serveDICOM(ctx, relay, defaultConfig(), http.DefaultClient, testProvider(t, testCredential('A')), newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json")))
			request := associationRequest("1.2.840.10008.5.1.4.1.1.2")
			copy(request[4:20], padAE(test.title))
			if err := writeDICOMPDU(clinic, 0x01, request); err != nil {
				t.Fatal(err)
			}
			pduType, response, err := readDICOMPDU(clinic)
			if err != nil {
				t.Fatal(err)
			}
			if !test.accepted {
				if pduType != 0x03 || !bytes.Equal(response, []byte{0, 1, 1, 7}) {
					t.Fatalf("association rejection type=%x body=%x", pduType, response)
				}
				return
			}
			if pduType != 0x02 || len(response) < 68 {
				t.Fatalf("association response type=%x length=%d", pduType, len(response))
			}
			if !bytes.Equal(response[4:36], request[4:36]) {
				t.Fatal("association response did not preserve the called and calling AE titles")
			}
			if err := writeDICOMPDU(clinic, 0x04, commandPDV(1, echoRequestCommand(1))); err != nil {
				t.Fatal(err)
			}
			if pduType, body, err := readDICOMPDU(clinic); err != nil || pduType != 0x04 || dimseResponseStatus(body) != 0 {
				t.Fatalf("echo response type=%x body=%x error=%v", pduType, body, err)
			}
		})
	}
}
