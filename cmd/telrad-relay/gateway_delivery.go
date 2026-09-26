package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// A clinic Relay is outbound-only, so it polls for reports. The gateway runs
// on Telrad's own gateway host, where the platform can reach it over a private
// path. The platform therefore pushes each report delivery to the gateway as
// one synchronous HTTP request, and the gateway never initiates report work.
const (
	gatewayDeliveryPath            = "/deliveries"
	defaultMaxConcurrentDeliveries = 16
	maxConcurrentDeliveriesLimit   = 256
	maxDeliveryRequestBytes        = 2 << 20
	gatewayDeliveryTimeout         = 30 * time.Second
	maxDeliveryTokenFileBytes      = 1024
	minDeliveryTokenLength         = 32
	maxDeliveryTokenLength         = 256
)

type reportDestination struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type gatewayDeliveryRequest struct {
	DeliveryID       string             `json:"deliveryId"`
	Destination      *reportDestination `json:"destination"`
	MessageControlID string             `json:"messageControlId"`
	Payload          string             `json:"payload"`
	PayloadSHA256    string             `json:"payloadSha256"`
}

type gatewayDeliveryResult struct {
	DeliveryID string `json:"deliveryId"`
	Outcome    string `json:"outcome"`
	AckCode    string `json:"ackCode,omitempty"`
	AckPayload string `json:"ackPayload,omitempty"`
	Error      string `json:"error,omitempty"`
}

func hasGatewayDeliverySettings(cfg *config) bool {
	return cfg.DeliveryListenAddress != "" || cfg.DeliveryPort != 0 || cfg.DeliveryTokenPath != "" || cfg.MaxConcurrentDeliveries != 0 || cfg.DeliveryTLSCertPath != "" || cfg.DeliveryTLSKeyPath != ""
}

func validateGatewayDeliveryConfig(cfg *config) error {
	address, err := netip.ParseAddr(cfg.DeliveryListenAddress)
	switch {
	case err != nil || !address.Is4() || address.String() != cfg.DeliveryListenAddress:
		return errors.New("deliveryListenAddress must be a canonical IPv4 address")
	case address.IsUnspecified():
		return errors.New("deliveryListenAddress must name the private interface, not 0.0.0.0")
	case cfg.DeliveryPort < 1 || cfg.DeliveryPort > 65535:
		return errors.New("deliveryPort must be an integer from 1 to 65535")
	case cfg.DeliveryPort == cfg.DicomPort || cfg.DeliveryPort == cfg.HL7Port:
		return errors.New("deliveryPort must differ from dicomPort and hl7Port")
	case strings.TrimSpace(cfg.DeliveryTokenPath) == "":
		return errors.New("deliveryTokenPath is required")
	case cfg.MaxConcurrentDeliveries < 1 || cfg.MaxConcurrentDeliveries > maxConcurrentDeliveriesLimit:
		return fmt.Errorf("maxConcurrentDeliveries must be from 1 through %d", maxConcurrentDeliveriesLimit)
	case (cfg.DeliveryTLSCertPath == "") != (cfg.DeliveryTLSKeyPath == ""):
		return errors.New("deliveryTlsCertPath and deliveryTlsKeyPath must be configured together")
	}
	token := filepath.Clean(gatewayConfigRelative(cfg, cfg.DeliveryTokenPath))
	if token == filepath.Clean(cfg.CredentialPath) || cfg.configPath != "" && token == filepath.Clean(cfg.configPath) {
		return errors.New("deliveryTokenPath must differ from the configuration and credential paths")
	}
	return nil
}

// Delivery file paths are resolved at use so that the configured values are
// never rewritten.
func gatewayConfigRelative(cfg *config, path string) string {
	return absolute(filepath.Dir(cfg.configPath), path)
}

// The token file follows the credential file rules: mode 0600 in a 0700
// directory. It holds one bearer token and may end with a newline.
func readDeliveryToken(path string) ([]byte, error) {
	invalid := errors.New("delivery token file is invalid")
	if runtime.GOOS != "windows" {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, errors.New("delivery token file must be a regular file with mode 0600")
		}
		directory, err := os.Stat(filepath.Dir(path))
		if err != nil || directory.Mode().Perm() != 0700 {
			return nil, errors.New("delivery token directory must have mode 0700")
		}
	}
	data, err := safeReadFile(path, maxDeliveryTokenFileBytes)
	if err != nil {
		return nil, invalid
	}
	token := data
	if n := len(token); n > 0 && token[n-1] == '\n' {
		token = token[:n-1]
		if n := len(token); n > 0 && token[n-1] == '\r' {
			token = token[:n-1]
		}
	}
	valid := len(token) >= minDeliveryTokenLength && len(token) <= maxDeliveryTokenLength
	for _, character := range token {
		valid = valid && character >= 0x21 && character <= 0x7e
	}
	if !valid {
		zeroBytes(data)
		return nil, invalid
	}
	return token, nil
}

func loadDeliveryTLS(cfg *config) (*tls.Config, error) {
	if cfg.DeliveryTLSCertPath == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(gatewayConfigRelative(cfg, cfg.DeliveryTLSCertPath), gatewayConfigRelative(cfg, cfg.DeliveryTLSKeyPath))
	if err != nil {
		return nil, errors.New("delivery TLS certificate or key is invalid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}, nil
}

// gatewayDeliveryPrerequisites proves the token and TLS material before any
// listener is bound. doctor uses it too.
func gatewayDeliveryPrerequisites(cfg *config) ([sha256.Size]byte, *tls.Config, error) {
	token, err := readDeliveryToken(gatewayConfigRelative(cfg, cfg.DeliveryTokenPath))
	if err != nil {
		return [sha256.Size]byte{}, nil, err
	}
	digest := sha256.Sum256(token)
	zeroBytes(token)
	tlsConfig, err := loadDeliveryTLS(cfg)
	return digest, tlsConfig, err
}

type gatewayDeliveryServer struct {
	listener net.Listener
	server   *http.Server
}

// listenGatewayDeliveries binds the delivery listener. Requests share the
// Relay work drainer, so shutdown lets in-flight RIS exchanges finish and
// refuses new requests. workCtx is cancelled only when the drain deadline
// expires.
func listenGatewayDeliveries(workCtx context.Context, cfg *config, work *workDrainer) (*gatewayDeliveryServer, error) {
	tokenDigest, tlsConfig, err := gatewayDeliveryPrerequisites(cfg)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(cfg.DeliveryListenAddress, strconv.Itoa(cfg.DeliveryPort)))
	if err != nil {
		return nil, fmt.Errorf("listen for report deliveries: %w", err)
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	handler := &gatewayDeliveryHandler{tokenDigest: tokenDigest, slots: make(chan struct{}, cfg.MaxConcurrentDeliveries), work: work}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      gatewayDeliveryTimeout + 20*time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return workCtx },
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	return &gatewayDeliveryServer{listener: listener, server: server}, nil
}

func (deliveries *gatewayDeliveryServer) serve(ctx context.Context, errCh chan<- error) {
	err := deliveries.server.Serve(deliveries.listener)
	if ctx.Err() == nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		errCh <- fmt.Errorf("serve report deliveries: %w", err)
	}
}

type gatewayDeliveryHandler struct {
	tokenDigest [sha256.Size]byte
	slots       chan struct{}
	work        *workDrainer
}

func (handler *gatewayDeliveryHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != gatewayDeliveryPath {
		writeDeliveryJSON(writer, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeDeliveryJSON(writer, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if !handler.authorized(request.Header.Values("Authorization")) {
		slog.Warn("gateway report delivery rejected", "reason", "unauthorized", "remote", request.RemoteAddr)
		writer.Header().Set("WWW-Authenticate", "Bearer")
		writeDeliveryJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if !handler.work.Start() {
		writer.Header().Set("Retry-After", "1")
		writeDeliveryJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "shutting_down"})
		return
	}
	defer handler.work.Done()
	select {
	case handler.slots <- struct{}{}:
		defer func() { <-handler.slots }()
	default:
		writer.Header().Set("Retry-After", "1")
		writeDeliveryJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": "busy"})
		return
	}
	var delivery gatewayDeliveryRequest
	if request.ContentLength > maxDeliveryRequestBytes || !mediaTypeEquals(request.Header.Get("Content-Type"), "application/json") ||
		decodeBoundedJSON(http.MaxBytesReader(writer, request.Body, maxDeliveryRequestBytes), maxDeliveryRequestBytes, &delivery) != nil || !delivery.complete() {
		slog.Warn("gateway report delivery rejected", "reason", "invalid_request", "remote", request.RemoteAddr)
		writeDeliveryJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(request.Context(), gatewayDeliveryTimeout)
	defer cancel()
	result := deliverGatewayReport(ctx, delivery)
	attributes := []any{"deliveryId", delivery.DeliveryID, "destination", net.JoinHostPort(delivery.Destination.Host, strconv.Itoa(delivery.Destination.Port)), "outcome", result.Outcome, "durationMs", time.Since(started).Milliseconds()}
	if result.Error != "" {
		attributes = append(attributes, "error", result.Error)
	}
	if result.AckCode != "" {
		attributes = append(attributes, "ackCode", result.AckCode)
	}
	slog.Info("gateway report delivery", attributes...)
	writeDeliveryJSON(writer, http.StatusOK, result)
}

// The bearer token is compared by digest, so neither its content nor its
// length is observable through timing.
func (handler *gatewayDeliveryHandler) authorized(values []string) bool {
	if len(values) != 1 {
		return false
	}
	scheme, credential, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	digest := sha256.Sum256([]byte(credential))
	return subtle.ConstantTimeCompare(digest[:], handler.tokenDigest[:]) == 1
}

func (delivery gatewayDeliveryRequest) complete() bool {
	return validOpaqueID(delivery.DeliveryID) && delivery.Destination != nil && delivery.MessageControlID != "" && delivery.Payload != "" && delivery.PayloadSHA256 != ""
}

func writeDeliveryJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

// deliverGatewayReport applies the checks a clinic Relay applies to a claim,
// with the request's destination in place of a permit-bound report host. A
// report that fails them never reaches the RIS.
func deliverGatewayReport(ctx context.Context, delivery gatewayDeliveryRequest) gatewayDeliveryResult {
	failure := func(code, ack string) gatewayDeliveryResult {
		return gatewayDeliveryResult{DeliveryID: delivery.DeliveryID, Outcome: "failed", Error: code, AckCode: ack}
	}
	host, port, ok := gatewayReportDestination(delivery.Destination)
	if !ok || !safeRetrievalReport(delivery.Payload) {
		return failure("invalid_report", "")
	}
	digest := sha256.Sum256([]byte(delivery.Payload))
	controlID, err := hl7MessageControlID(delivery.Payload)
	if hex.EncodeToString(digest[:]) != delivery.PayloadSHA256 || err != nil || controlID != delivery.MessageControlID {
		return failure("invalid_report", "")
	}
	ack, payload, err := sendMLLP(ctx, host, port, delivery.Payload)
	if err == nil && ack == "AA" {
		return gatewayDeliveryResult{DeliveryID: delivery.DeliveryID, Outcome: "accepted", AckCode: ack, AckPayload: payload}
	}
	if ack != "" {
		result := failure("clinic_rejected", ack)
		result.AckPayload = payload
		return result
	}
	return failure(safeNetworkError(err).Error(), "")
}

// A gateway dials the destination the platform names. It must be a canonical,
// specified IPv4 address and a valid port.
func gatewayReportDestination(destination *reportDestination) (string, int, bool) {
	if destination == nil || destination.Port < 1 || destination.Port > 65535 {
		return "", 0, false
	}
	address, err := netip.ParseAddr(destination.Host)
	if err != nil || !address.Is4() || address.IsUnspecified() || address.String() != destination.Host {
		return "", 0, false
	}
	return destination.Host, destination.Port, true
}
