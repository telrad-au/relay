package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

type protocolClients struct {
	transport *http.Transport
	secure    *http.Client
	updates   *http.Client
}

func newProtocolClients(cfg *config) protocolClients {
	transport := &http.Transport{
		Proxy:                  proxyResolver,
		DialContext:            (&net.Dialer{Timeout: time.Duration(cfg.ConnectTimeoutSeconds) * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxConnsPerHost:        cfg.MaxConnections,
		MaxIdleConns:           cfg.MaxConnections,
		MaxIdleConnsPerHost:    cfg.MaxConnections,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    time.Duration(cfg.TLSHandshakeTimeoutSeconds) * time.Second,
		ResponseHeaderTimeout:  time.Duration(cfg.ResponseHeaderTimeoutSecs) * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxCloudResponseBytes,
	}
	noRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return protocolClients{
		transport: transport,
		secure:    &http.Client{Transport: transport, CheckRedirect: noRedirect},
		updates:   &http.Client{Transport: transport},
	}
}

func addRelayHeaders(req *http.Request, provider *credentialProvider, contentType, key string) {
	req.Header.Set("Authorization", "Bearer "+provider.Current())
	req.Header.Set("X-Telrad-Protocol-Version", protocolVersion)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
}

func mediaTypeEquals(value, expected string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, expected) {
		return false
	}
	if expected == "application/json" && len(parameters) == 1 && strings.EqualFold(parameters["charset"], "utf-8") {
		return true
	}
	return len(parameters) == 0
}

func decodeBoundedJSON(reader io.Reader, limit int64, value any) error {
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if limited.N <= 0 {
		return errors.New("response exceeds limit")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("response contains multiple JSON values")
	}
	if limited.N <= 0 {
		return errors.New("response exceeds limit")
	}
	return nil
}

func safeNetworkError(err error) error {
	if err == nil {
		return nil
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return errors.New("network_timeout")
	}
	return errors.New("network_error")
}

func validOpaqueID(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for index := range len(value) {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}
