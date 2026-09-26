package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Gateway mode runs the Relay on Telrad's IPsec gateway host for every
// site-to-site VPN. The tunnel vouches for the clinic, so the gateway neither
// signs orders nor verifies report permits. It translates protocols and tells
// Telrad which tunnel address each native connection came from.
const (
	relayModeClinic              = "clinic"
	relayModeGateway             = "gateway"
	gatewayPeerHeader            = "X-Telrad-Peer-Ip"
	defaultMaxConnectionsPerPeer = 64
)

func (cfg *config) gatewayMode() bool { return cfg.Mode == relayModeGateway }

func validateRelayMode(cfg *config, command string) error {
	switch cfg.Mode {
	case "", relayModeClinic:
		if cfg.MaxConnectionsPerPeer != 0 {
			return errors.New("maxConnectionsPerPeer is valid only in gateway mode")
		}
		return nil
	case relayModeGateway:
	default:
		return errors.New(`mode must be "clinic" or "gateway"`)
	}
	switch {
	case command == "auth" || command == "enroll":
		return errors.New("gateway mode does not pair; provision the gateway credential file")
	case managedConfig(cfg.configPath):
		return errors.New("native installations do not support gateway mode")
	case cfg.Retrieval != nil:
		return errors.New("gateway mode does not support retrieval")
	case cfg.DisableDICOMListener:
		return errors.New("gateway mode requires the DICOM listener")
	case cfg.ReportHost != "" || cfg.ReportPort != 0:
		return errors.New("gateway mode takes each report destination from its control claim; remove reportHost and reportPort")
	case cfg.UpdateManifestURL != "" || cfg.UpdatePublicKey != "":
		return errors.New("gateway mode is updated by replacing its container image; remove updateManifestUrl and updatePublicKey")
	case strings.TrimSpace(cfg.CredentialPath) == "":
		return errors.New("credentialPath is required")
	case cfg.MaxConnectionsPerPeer < 1 || cfg.MaxConnectionsPerPeer > cfg.MaxConnections:
		return errors.New("maxConnectionsPerPeer must be from 1 through maxConnections")
	}
	return nil
}

// A gateway has no configured report destination. Clear the clinic defaults
// so that only an explicit setting survives loading, which validation rejects.
func clearGatewayReportDefaults(cfg *config, data []byte) error {
	if !cfg.gatewayMode() {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode relay configuration: %w", err)
	}
	host, port := false, false
	for name := range fields {
		host = host || strings.EqualFold(name, "reportHost")
		port = port || strings.EqualFold(name, "reportPort")
	}
	if !host {
		cfg.ReportHost = ""
	}
	if !port {
		cfg.ReportPort = 0
	}
	return nil
}

// connectionAttribution adapts a protocol handler to the accept loop. A clinic
// Relay serves every connection with the shared client. A gateway serves each
// IPv4 peer with a client that attributes its HTTPS requests to that peer.
type connectionAttribution struct {
	client       *http.Client
	gateway      bool
	limit        int
	mu           sync.Mutex
	active       map[netip.Addr]int
	rejected     int
	lastReported time.Time
}

func newConnectionAttribution(cfg *config, client *http.Client) *connectionAttribution {
	return &connectionAttribution{client: client, gateway: cfg.gatewayMode(), limit: cfg.MaxConnectionsPerPeer, active: make(map[netip.Addr]int)}
}

func (attribution *connectionAttribution) serve(handler func(net.Conn, *http.Client)) func(net.Conn) {
	return func(conn net.Conn) {
		if !attribution.gateway {
			handler(conn, attribution.client)
			return
		}
		// The accept loop closes the connection when this returns.
		peer, ok := gatewayPeer(conn.RemoteAddr())
		if !ok || !attribution.acquire(peer) {
			return
		}
		defer attribution.release(peer)
		handler(conn, peerClient(attribution.client, peer))
	}
}

func (attribution *connectionAttribution) acquire(peer netip.Addr) bool {
	attribution.mu.Lock()
	defer attribution.mu.Unlock()
	if attribution.active[peer] < attribution.limit {
		attribution.active[peer]++
		return true
	}
	attribution.rejected++
	if now := time.Now(); now.Sub(attribution.lastReported) >= time.Minute {
		slog.Warn("gateway peer connection limit reached", "peer", peer.String(), "rejectedSinceLastReport", attribution.rejected)
		attribution.rejected = 0
		attribution.lastReported = now
	}
	return false
}

func (attribution *connectionAttribution) release(peer netip.Addr) {
	attribution.mu.Lock()
	defer attribution.mu.Unlock()
	if attribution.active[peer] <= 1 {
		delete(attribution.active, peer)
		return
	}
	attribution.active[peer]--
}

// gatewayPeer returns the canonical IPv4 address of a TCP peer. A dual-stack
// listener reports IPv4 peers as IPv4-mapped IPv6 addresses.
func gatewayPeer(address net.Addr) (netip.Addr, bool) {
	tcp, ok := address.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	peer := tcp.AddrPort().Addr().Unmap()
	return peer, peer.Is4() && !peer.IsUnspecified()
}

func peerClient(base *http.Client, peer netip.Addr) *http.Client {
	client := *base
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = peerTransport{base: transport, peer: peer.String()}
	return &client
}

type peerTransport struct {
	base http.RoundTripper
	peer string
}

func (transport peerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set(gatewayPeerHeader, transport.peer)
	return transport.base.RoundTrip(clone)
}

// A gateway dials the destination the claim names. The payload restriction
// still applies: a report channel must never return an order.
func gatewayReportDestination(report reportMessage) (string, int, bool) {
	destination := report.Destination
	if destination == nil || destination.Port < 1 || destination.Port > 65535 {
		return "", 0, false
	}
	address, err := netip.ParseAddr(destination.Host)
	if err != nil || !address.Is4() || address.IsUnspecified() || address.String() != destination.Host {
		return "", 0, false
	}
	if !safeRetrievalReport(report.Payload) {
		return "", 0, false
	}
	return destination.Host, destination.Port, true
}

func gatewayDoctor(cfg *config, output io.Writer) error {
	if _, err := readCredentialFile(cfg.CredentialPath, time.Now()); err != nil {
		return errors.New("stored credential is invalid")
	}
	fmt.Fprintf(output, "gateway configuration and credential ok; DICOM %s:%d, HL7 %s:%d, reports to each claimed destination\n", cfg.ListenAddress, cfg.DicomPort, cfg.ListenAddress, cfg.HL7Port)
	return nil
}

// The VPN ingress firewall rejects loopback probes, so readiness proves that
// the running Relay holds each configured socket instead of dialling it.
func gatewayListenersHeld(cfg *config) error {
	for _, port := range []int{cfg.DicomPort, cfg.HL7Port} {
		probe, err := net.Listen("tcp", net.JoinHostPort(cfg.ListenAddress, strconv.Itoa(port)))
		if err == nil {
			_ = probe.Close()
			return errors.New("gateway listener is unavailable")
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return errors.New("gateway listener is unavailable")
		}
	}
	return nil
}
