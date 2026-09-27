package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The status endpoint says whether reportHost is still the installer
// placeholder, and names the receiver only when it is real.
func TestStatusReportsReportReceiver(t *testing.T) {
	for _, test := range []struct {
		host       string
		configured bool
		receiver   string
	}{
		{"192.0.2.20", true, "192.0.2.20:2576"},
		{"ris.clinic.example", true, "ris.clinic.example:2576"},
		{"2001:db8::20", true, "[2001:db8::20]:2576"},
		{reportHostPlaceholder, false, ""},
		{"Report-Receiver.Invalid.", false, ""},
		{"", false, ""},
	} {
		cfg := testConfig(t, nil)
		cfg.ReportHost = test.host
		recorder := httptest.NewRecorder()
		newStatusServer(cfg, nil, nil).handleStatus(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
		var generic map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &generic); err != nil {
			t.Fatal(err)
		}
		if configured, present := generic["reportReceiverConfigured"]; !present || configured != test.configured {
			t.Fatalf("host %q: reportReceiverConfigured=%v present=%t", test.host, configured, present)
		}
		receiver, present := generic["reportReceiver"]
		if test.receiver == "" && present || test.receiver != "" && receiver != test.receiver {
			t.Fatalf("host %q: reportReceiver=%v", test.host, receiver)
		}
	}
}

func TestPrintStatusShowsReportReceiver(t *testing.T) {
	const notConfigured = "report receiver: NOT CONFIGURED - edit reportHost in /etc/telrad-relay/relay.json and run telrad restart\n"
	for _, test := range []struct {
		name   string
		report statusReport
		want   string
	}{
		{"configured", statusReport{State: "ready", Paired: true, ReportReceiverConfigured: true, ReportReceiver: "192.0.2.20:2576"}, "report receiver: 192.0.2.20:2576\n"},
		{"placeholder while paired", statusReport{State: "ready", Paired: true}, notConfigured},
		{"placeholder while pairing", statusReport{State: "pairing", PairingLink: "https://app.example.invalid/approve/x"}, notConfigured},
	} {
		var out bytes.Buffer
		printStatus(&out, &test.report, "/etc/telrad-relay/relay.json")
		if !strings.Contains(out.String(), test.want) {
			t.Fatalf("%s: status output missing %q:\n%s", test.name, test.want, out.String())
		}
	}

	previous := distribution
	distribution = "docker"
	t.Cleanup(func() { distribution = previous })
	var out bytes.Buffer
	printStatus(&out, &statusReport{State: "pairing"}, "/etc/telrad-relay/relay.json")
	if !strings.Contains(out.String(), "report receiver: NOT CONFIGURED - set TELRAD_RELAY_REPORT_HOST and recreate the container\n") {
		t.Fatalf("container status output:\n%s", out.String())
	}
}
