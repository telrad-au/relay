package main

import "net/url"

type protocolEndpoints struct {
	PairingURL string
	ControlURL string
	DicomURL   string
	HL7URL     string
}

func deriveProtocolEndpoints(pairingURL string) (protocolEndpoints, error) {
	if err := validateEndpointURL("pairingUrl", pairingURL, "https", "/v1/relay/pairing-enrollments"); err != nil {
		return protocolEndpoints{}, err
	}
	parsed, _ := url.Parse(pairingURL)
	build := func(scheme, path string) string {
		return (&url.URL{Scheme: scheme, Host: parsed.Host, Path: path}).String()
	}
	return protocolEndpoints{
		PairingURL: pairingURL,
		ControlURL: build("wss", "/v1/relay/control"),
		DicomURL:   build("https", "/v1/relay/ingest/dicom"),
		HL7URL:     build("https", "/v1/relay/ingest/hl7"),
	}, nil
}
