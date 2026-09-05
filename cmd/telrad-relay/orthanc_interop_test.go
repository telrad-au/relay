package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	orthancInteropEnvironment = "TELRAD_ORTHANC_INTEROP_TEST"
	orthancInteropImage       = "jodogne/orthanc-plugins:1.12.11@sha256:e7bffe0351cd391eacab8e78098e236efe6cafed987830e9b462b2050a0eae4a"

	secondaryCaptureSOPClassUID = "1.2.840.10008.5.1.4.1.1.7"
	explicitVRLittleEndianUID   = "1.2.840.10008.1.2.1"
	jpegLosslessSV1UID          = "1.2.840.10008.1.2.4.70"
)

var orthancContainerSequence atomic.Uint64

type orthancFixture struct {
	name                  string
	instanceID            string
	instanceUID           string
	transferSyntax        string
	source                []byte
	expectedDatasetSHA256 string
	expectedPixelSHA256   string
}

type orthancCloudUpload struct {
	body           []byte
	releaseReceipt chan struct{}
}

type dicomWireCapture struct {
	dataset        []byte
	dataPDVs       int
	transferSyntax string
}

type recordingConn struct {
	net.Conn
	mu       sync.Mutex
	received bytes.Buffer
	sent     bytes.Buffer
}

func (connection *recordingConn) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	if count > 0 {
		connection.mu.Lock()
		_, _ = connection.received.Write(buffer[:count])
		connection.mu.Unlock()
	}
	return count, err
}

func (connection *recordingConn) Write(buffer []byte) (int, error) {
	count, err := connection.Conn.Write(buffer)
	if count > 0 {
		connection.mu.Lock()
		_, _ = connection.sent.Write(buffer[:count])
		connection.mu.Unlock()
	}
	return count, err
}

func (connection *recordingConn) streams() ([]byte, []byte) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	return bytes.Clone(connection.received.Bytes()), bytes.Clone(connection.sent.Bytes())
}

func TestOrthancDICOMPayloadIntegrity(t *testing.T) {
	if os.Getenv(orthancInteropEnvironment) != "1" {
		t.Skip("set TELRAD_ORTHANC_INTEROP_TEST=1 to run the Docker-backed Orthanc interoperability test")
	}

	testContext, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	uploads := make(chan orthancCloudUpload, 2)
	var uploadSequence atomic.Uint64
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/dicom" {
			http.Error(writer, "unexpected DICOM request", http.StatusBadRequest)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+testCredential('O') {
			http.Error(writer, "missing Relay credential", http.StatusUnauthorized)
			return
		}
		if request.Header.Get("Idempotency-Key") != "" {
			http.Error(writer, "unexpected DICOM idempotency key", http.StatusBadRequest)
			return
		}
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			http.Error(writer, "read DICOM request", http.StatusBadRequest)
			return
		}
		upload := orthancCloudUpload{body: body, releaseReceipt: make(chan struct{})}
		select {
		case uploads <- upload:
		case <-request.Context().Done():
			return
		}
		select {
		case <-upload.releaseReceipt:
		case <-request.Context().Done():
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"status":"accepted","receiptId":"orthanc-receipt-%d"}`, uploadSequence.Add(1))
	}))
	defer cloud.Close()

	cfg := defaultConfig()
	cfg.DicomURL = cloud.URL
	provider := testProvider(t, testCredential('O'))
	status := newRuntimeStatus(filepath.Join(t.TempDir(), "relay.json"))
	wireCaptures := make(chan dicomWireCapture, 2)
	acceptErrors := make(chan error, 1)
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				if testContext.Err() == nil && !errors.Is(acceptErr, net.ErrClosed) {
					select {
					case acceptErrors <- acceptErr:
					default:
					}
				}
				return
			}
			recorded := &recordingConn{Conn: connection}
			serveDICOM(testContext, recorded, cfg, cloud.Client(), provider, status)
			_ = recorded.Close()
			received, sent := recorded.streams()
			capture, captureErr := parseDICOMWireCapture(received, sent)
			if captureErr != nil {
				select {
				case acceptErrors <- captureErr:
				default:
				}
				continue
			}
			select {
			case wireCaptures <- capture:
			case <-testContext.Done():
				return
			}
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	sender := startOrthancContainer(t, testContext, "sender", map[string]any{
		"relay": map[string]any{
			"AET":              "TELRAD",
			"Host":             "host.docker.internal",
			"Port":             port,
			"AllowStore":       true,
			"AllowTranscoding": false,
			"Timeout":          30,
		},
	})

	fixtures := createOrthancFixtures(t, testContext, sender)
	capturedBodies := make(map[string][]byte, len(fixtures))
	for _, fixture := range fixtures {
		storeFinished := make(chan error, 1)
		go func(instanceID string) {
			_, _, requestErr := sender.request(testContext, http.MethodPost, "/modalities/relay/store", "application/json", strings.NewReader(fmt.Sprintf(`{"Resources":[%q],"Synchronous":true,"Timeout":30}`, instanceID)))
			storeFinished <- requestErr
		}(fixture.instanceID)

		var upload orthancCloudUpload
		select {
		case upload = <-uploads:
		case acceptErr := <-acceptErrors:
			t.Fatalf("Relay DICOM listener failed: %v", acceptErr)
		case <-testContext.Done():
			t.Fatal("timed out waiting for Relay's DICOM upload")
		}
		select {
		case storeErr := <-storeFinished:
			t.Fatalf("Orthanc C-STORE for %s completed before the cloud receipt: %v", fixture.name, storeErr)
		case <-time.After(100 * time.Millisecond):
		}
		close(upload.releaseReceipt)
		select {
		case storeErr := <-storeFinished:
			if storeErr != nil {
				t.Fatalf("Orthanc C-STORE for %s failed: %v", fixture.name, storeErr)
			}
		case <-testContext.Done():
			t.Fatalf("timed out waiting for Orthanc C-STORE success for %s", fixture.name)
		}

		var wire dicomWireCapture
		select {
		case wire = <-wireCaptures:
		case acceptErr := <-acceptErrors:
			t.Fatalf("could not inspect %s DICOM association: %v", fixture.name, acceptErr)
		case <-testContext.Done():
			t.Fatalf("timed out waiting for %s DICOM association to close", fixture.name)
		}
		if wire.dataPDVs < 2 {
			t.Fatalf("%s used %d dataset PDVs, want at least 2", fixture.name, wire.dataPDVs)
		}
		if wire.transferSyntax != fixture.transferSyntax {
			t.Fatalf("%s negotiated transfer syntax=%q, want %q", fixture.name, wire.transferSyntax, fixture.transferSyntax)
		}
		if err := compareDICOMPayload(wire.dataset, upload.body); err != nil {
			t.Fatalf("%s payload integrity: %v", fixture.name, err)
		}
		assertOrthancFixtureBytes(t, fixture, wire.dataset)
		capturedBodies[fixture.name] = upload.body
	}
	validator := startOrthancContainer(t, testContext, "validator", nil)
	for _, fixture := range fixtures {
		captured := capturedBodies[fixture.name]
		metadata, _, metadataErr := parseDICOMPart10(captured)
		if metadataErr != nil {
			t.Fatalf("parse %s Relay Part 10 object: %v", fixture.name, metadataErr)
		}
		if metadata.mediaStorageSOPClassUID != secondaryCaptureSOPClassUID || metadata.mediaStorageSOPInstanceUID != fixture.instanceUID || metadata.transferSyntaxUID != fixture.transferSyntax {
			t.Fatalf("%s Relay Part 10 metadata = %#v", fixture.name, metadata)
		}

		validatedID := validator.importInstance(t, testContext, captured)
		body, _, requestErr := validator.request(testContext, http.MethodGet, "/instances/"+validatedID+"/simplified-tags", "", nil)
		if requestErr != nil {
			t.Fatalf("query %s validated tags: %v", fixture.name, requestErr)
		}
		var tags map[string]string
		if err := json.Unmarshal(body, &tags); err != nil {
			t.Fatalf("decode %s validated tags: %v", fixture.name, err)
		}
		for tag, want := range map[string]string{
			"SOPClassUID":       secondaryCaptureSOPClassUID,
			"SOPInstanceUID":    fixture.instanceUID,
			"PatientName":       "SYNTHETIC^RELAY",
			"PatientID":         "RELAY-INTEROP",
			"AccessionNumber":   "SYNTH0001",
			"StudyDescription":  "Synthetic Relay interoperability",
			"SeriesDescription": "Deterministic payload integrity",
		} {
			if tags[tag] != want {
				t.Errorf("%s validated %s=%q, want %q", fixture.name, tag, tags[tag], want)
			}
		}
		transferSyntaxBody, _, requestErr := validator.request(testContext, http.MethodGet, "/instances/"+validatedID+"/metadata/TransferSyntax", "", nil)
		if requestErr != nil {
			t.Fatalf("query %s validated transfer syntax: %v", fixture.name, requestErr)
		}
		if got := strings.TrimSpace(string(transferSyntaxBody)); got != fixture.transferSyntax {
			t.Errorf("%s validator transfer syntax=%q, want %q", fixture.name, got, fixture.transferSyntax)
		}
	}
}

func TestDICOMPayloadComparisonRejectsMutation(t *testing.T) {
	source := buildSyntheticDICOM("1.2.826.0.1.3680043.10.543.80.1", "1.2.826.0.1.3680043.10.543.81.1")
	_, offset, err := parseDICOMPart10(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareDICOMPayload(source[offset:], source); err != nil {
		t.Fatalf("identical payload rejected: %v", err)
	}
	mutated := bytes.Clone(source)
	mutated[len(mutated)-1] ^= 0x01
	if err := compareDICOMPayload(source[offset:], mutated); err == nil {
		t.Fatal("one-byte Pixel Data mutation passed the integrity comparison")
	}
}

type dicomPart10Metadata struct {
	mediaStorageSOPClassUID    string
	mediaStorageSOPInstanceUID string
	transferSyntaxUID          string
}

func parseDICOMPart10(data []byte) (dicomPart10Metadata, int, error) {
	if len(data) < 132 || string(data[128:132]) != "DICM" {
		return dicomPart10Metadata{}, 0, errors.New("missing Part 10 preamble")
	}
	metadata := dicomPart10Metadata{}
	offset := 132
	for offset < len(data) {
		if offset+8 > len(data) {
			return dicomPart10Metadata{}, 0, errors.New("truncated Part 10 metadata")
		}
		group := binary.LittleEndian.Uint16(data[offset:])
		if group != 0x0002 {
			if metadata.mediaStorageSOPClassUID == "" || metadata.mediaStorageSOPInstanceUID == "" || metadata.transferSyntaxUID == "" {
				return dicomPart10Metadata{}, 0, errors.New("incomplete Part 10 metadata")
			}
			return metadata, offset, nil
		}
		element := binary.LittleEndian.Uint16(data[offset+2:])
		vr := string(data[offset+4 : offset+6])
		headerBytes := 8
		var valueBytes uint32
		if dicomLongVR(vr) {
			if offset+12 > len(data) {
				return dicomPart10Metadata{}, 0, errors.New("truncated long Part 10 metadata")
			}
			headerBytes = 12
			valueBytes = binary.LittleEndian.Uint32(data[offset+8:])
		} else {
			valueBytes = uint32(binary.LittleEndian.Uint16(data[offset+6:]))
		}
		end := offset + headerBytes + int(valueBytes)
		if valueBytes == 0xffffffff || end < offset || end > len(data) {
			return dicomPart10Metadata{}, 0, errors.New("invalid Part 10 metadata length")
		}
		value := strings.TrimRight(string(data[offset+headerBytes:end]), "\x00 ")
		switch element {
		case 0x0002:
			metadata.mediaStorageSOPClassUID = value
		case 0x0003:
			metadata.mediaStorageSOPInstanceUID = value
		case 0x0010:
			metadata.transferSyntaxUID = value
		}
		offset = end
	}
	return dicomPart10Metadata{}, 0, errors.New("Part 10 object has no dataset")
}

func compareDICOMPayload(wireDataset, part10 []byte) error {
	_, offset, err := parseDICOMPart10(part10)
	if err != nil {
		return err
	}
	capturedDataset := part10[offset:]
	if bytes.Equal(wireDataset, capturedDataset) {
		return nil
	}
	return fmt.Errorf("wire dataset sha256=%s, HTTPS dataset sha256=%s", sha256Hex(wireDataset), sha256Hex(capturedDataset))
}

func assertOrthancFixtureBytes(t *testing.T, fixture orthancFixture, wireDataset []byte) {
	t.Helper()
	_, sourceOffset, err := parseDICOMPart10(fixture.source)
	if err != nil {
		t.Fatalf("parse %s source fixture: %v", fixture.name, err)
	}
	sourceDataset := fixture.source[sourceOffset:]
	if !bytes.Equal(wireDataset, sourceDataset) {
		t.Fatalf("Orthanc changed %s dataset before Relay: source sha256=%s, wire sha256=%s", fixture.name, sha256Hex(sourceDataset), sha256Hex(wireDataset))
	}
	if digest := sha256Hex(sourceDataset); digest != fixture.expectedDatasetSHA256 {
		t.Fatalf("%s dataset sha256=%s, want recorded %s", fixture.name, digest, fixture.expectedDatasetSHA256)
	}
	pixelData, err := dicomPixelDataValue(sourceDataset)
	if err != nil {
		t.Fatalf("read %s Pixel Data: %v", fixture.name, err)
	}
	if digest := sha256Hex(pixelData); digest != fixture.expectedPixelSHA256 {
		t.Fatalf("%s Pixel Data sha256=%s, want recorded %s", fixture.name, digest, fixture.expectedPixelSHA256)
	}
}

func parseDICOMWireCapture(received, sent []byte) (dicomWireCapture, error) {
	capture := dicomWireCapture{}
	var dataContextID byte
	for offset := 0; offset < len(received); {
		if offset+6 > len(received) {
			return dicomWireCapture{}, errors.New("truncated recorded DICOM PDU")
		}
		pduType := received[offset]
		bodyBytes := int(binary.BigEndian.Uint32(received[offset+2:]))
		bodyStart := offset + 6
		bodyEnd := bodyStart + bodyBytes
		if bodyBytes < 0 || bodyEnd < bodyStart || bodyEnd > len(received) {
			return dicomWireCapture{}, errors.New("invalid recorded DICOM PDU length")
		}
		if pduType == 0x04 {
			pdvs, err := parsePDVs(received[bodyStart:bodyEnd])
			if err != nil {
				return dicomWireCapture{}, fmt.Errorf("parse recorded DICOM PDVs: %w", err)
			}
			for _, pdv := range pdvs {
				if !pdv.command {
					if dataContextID == 0 {
						dataContextID = pdv.contextID
					} else if dataContextID != pdv.contextID {
						return dicomWireCapture{}, errors.New("recorded dataset changed presentation context")
					}
					capture.dataPDVs++
					capture.dataset = append(capture.dataset, pdv.data...)
				}
			}
		}
		offset = bodyEnd
	}
	if len(capture.dataset) == 0 {
		return dicomWireCapture{}, errors.New("recorded association contained no dataset PDVs")
	}
	transferSyntax, err := acceptedDICOMTransferSyntax(sent, dataContextID)
	if err != nil {
		return dicomWireCapture{}, err
	}
	capture.transferSyntax = transferSyntax
	return capture, nil
}

func acceptedDICOMTransferSyntax(sent []byte, contextID byte) (string, error) {
	for offset := 0; offset < len(sent); {
		if offset+6 > len(sent) {
			return "", errors.New("truncated recorded DICOM response PDU")
		}
		pduType := sent[offset]
		bodyBytes := int(binary.BigEndian.Uint32(sent[offset+2:]))
		bodyStart := offset + 6
		bodyEnd := bodyStart + bodyBytes
		if bodyBytes < 0 || bodyEnd < bodyStart || bodyEnd > len(sent) {
			return "", errors.New("invalid recorded DICOM response PDU length")
		}
		if pduType != 0x02 {
			offset = bodyEnd
			continue
		}
		body := sent[bodyStart:bodyEnd]
		if len(body) < 68 {
			return "", errors.New("truncated association acceptance")
		}
		for itemOffset := 68; itemOffset < len(body); {
			if itemOffset+4 > len(body) {
				return "", errors.New("truncated association acceptance item")
			}
			itemType := body[itemOffset]
			itemBytes := int(binary.BigEndian.Uint16(body[itemOffset+2:]))
			valueStart := itemOffset + 4
			valueEnd := valueStart + itemBytes
			if valueEnd < valueStart || valueEnd > len(body) {
				return "", errors.New("invalid association acceptance item length")
			}
			value := body[valueStart:valueEnd]
			if itemType == 0x21 && len(value) >= 4 && value[0] == contextID && value[2] == 0 {
				for syntaxOffset := 4; syntaxOffset < len(value); {
					if syntaxOffset+4 > len(value) {
						return "", errors.New("truncated accepted transfer syntax item")
					}
					syntaxType := value[syntaxOffset]
					syntaxBytes := int(binary.BigEndian.Uint16(value[syntaxOffset+2:]))
					syntaxStart := syntaxOffset + 4
					syntaxEnd := syntaxStart + syntaxBytes
					if syntaxEnd < syntaxStart || syntaxEnd > len(value) {
						return "", errors.New("invalid accepted transfer syntax length")
					}
					if syntaxType == 0x40 {
						return string(value[syntaxStart:syntaxEnd]), nil
					}
					syntaxOffset = syntaxEnd
				}
			}
			itemOffset = valueEnd
		}
		return "", errors.New("dataset presentation context was not accepted")
	}
	return "", errors.New("association acceptance was not recorded")
}

func createOrthancFixtures(t *testing.T, ctx context.Context, sender *orthancTestContainer) []orthancFixture {
	t.Helper()
	explicitInstanceUID := "1.2.826.0.1.3680043.10.543.80.1"
	compressedInstanceUID := "1.2.826.0.1.3680043.10.543.80.2"
	explicit := buildSyntheticDICOM(explicitInstanceUID, "1.2.826.0.1.3680043.10.543.81.1")
	explicitID := sender.importInstance(t, ctx, explicit)
	explicitStored := sender.instanceFile(t, ctx, explicitID)

	compressedSource := buildSyntheticDICOM(compressedInstanceUID, "1.2.826.0.1.3680043.10.543.81.2")
	compressedSourceID := sender.importInstance(t, ctx, compressedSource)
	modifyBody := strings.NewReader(`{"Force":true,"Keep":["StudyInstanceUID","SeriesInstanceUID","SOPInstanceUID"],"Transcode":"1.2.840.10008.1.2.4.70"}`)
	compressed, _, requestErr := sender.request(ctx, http.MethodPost, "/instances/"+compressedSourceID+"/modify", "application/json", modifyBody)
	if requestErr != nil {
		t.Fatalf("transcode JPEG Lossless fixture: %v", requestErr)
	}
	if _, _, err := parseDICOMPart10(compressed); err != nil {
		t.Fatalf("Orthanc returned invalid JPEG Lossless fixture: %v", err)
	}
	if _, _, requestErr := sender.request(ctx, http.MethodDelete, "/instances/"+compressedSourceID, "", nil); requestErr != nil {
		t.Fatalf("remove uncompressed source fixture: %v", requestErr)
	}
	compressedID := sender.importInstance(t, ctx, compressed)
	compressedStored := sender.instanceFile(t, ctx, compressedID)

	fixtures := []orthancFixture{
		{
			name:                  "explicit VR little endian",
			instanceID:            explicitID,
			instanceUID:           explicitInstanceUID,
			transferSyntax:        explicitVRLittleEndianUID,
			source:                explicitStored,
			expectedDatasetSHA256: "442cacbba86f644d89b6e694e5e74b84e69dd097cf37207eb9cbb0d4d2c36fe6",
			expectedPixelSHA256:   "4397501c0da648ddd36e664efc8dae7ba448c6ae7b60aa17e57d0a00000f25e7",
		},
		{
			name:                  "JPEG Lossless SV1",
			instanceID:            compressedID,
			instanceUID:           compressedInstanceUID,
			transferSyntax:        jpegLosslessSV1UID,
			source:                compressedStored,
			expectedDatasetSHA256: "f60a90c90104aba43c8956cdba52c0d9009382479a762460c99529968d1f7dd0",
			expectedPixelSHA256:   "aafca1f21aecddb6d5bb6af7ab1c5bdf17a84214a314bc0f08f8fdd681c70747",
		},
	}
	for _, fixture := range fixtures {
		metadata, datasetOffset, err := parseDICOMPart10(fixture.source)
		if err != nil {
			t.Fatalf("parse %s source metadata: %v", fixture.name, err)
		}
		if metadata.mediaStorageSOPClassUID != secondaryCaptureSOPClassUID || metadata.mediaStorageSOPInstanceUID != fixture.instanceUID || metadata.transferSyntaxUID != fixture.transferSyntax {
			t.Fatalf("%s source metadata = %#v", fixture.name, metadata)
		}
		if len(fixture.source)-datasetOffset <= 64*1024 {
			t.Fatalf("%s dataset has %d bytes, not enough to force fragmentation below Relay's negotiated 64 KiB maximum PDU", fixture.name, len(fixture.source)-datasetOffset)
		}
	}
	return fixtures
}

// buildSyntheticDICOM generates the complete fixture from fixed tags and a
// deterministic pixel formula; it never reads patient data or an external file.
func buildSyntheticDICOM(instanceUID, seriesUID string) []byte {
	text := func(value string, padding byte) []byte {
		data := []byte(value)
		if len(data)%2 != 0 {
			data = append(data, padding)
		}
		return data
	}
	ui := func(value string) []byte { return text(value, 0) }
	us := func(value uint16) []byte {
		data := make([]byte, 2)
		binary.LittleEndian.PutUint16(data, value)
		return data
	}

	var metadata []byte
	metadata = appendDICOMExplicitElement(metadata, 0x0002, 0x0001, "OB", []byte{0, 1})
	metadata = appendDICOMExplicitElement(metadata, 0x0002, 0x0002, "UI", ui(secondaryCaptureSOPClassUID))
	metadata = appendDICOMExplicitElement(metadata, 0x0002, 0x0003, "UI", ui(instanceUID))
	metadata = appendDICOMExplicitElement(metadata, 0x0002, 0x0010, "UI", ui(explicitVRLittleEndianUID))
	metadata = appendDICOMExplicitElement(metadata, 0x0002, 0x0012, "UI", ui("1.2.826.0.1.3680043.10.543.89"))

	groupLength := make([]byte, 4)
	binary.LittleEndian.PutUint32(groupLength, uint32(len(metadata)))
	var data []byte
	data = append(data, make([]byte, 128)...)
	data = append(data, "DICM"...)
	data = appendDICOMExplicitElement(data, 0x0002, 0x0000, "UL", groupLength)
	data = append(data, metadata...)

	data = appendDICOMExplicitElement(data, 0x0008, 0x0008, "CS", text("ORIGINAL\\PRIMARY", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0016, "UI", ui(secondaryCaptureSOPClassUID))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0018, "UI", ui(instanceUID))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0020, "DA", text("20260101", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0023, "DA", text("20260101", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0030, "TM", text("120000", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0033, "TM", text("120000", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0050, "SH", text("SYNTH0001", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0060, "CS", text("OT", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0064, "CS", text("WSD", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x0070, "LO", text("Telrad test fixture", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x1030, "LO", text("Synthetic Relay interoperability", ' '))
	data = appendDICOMExplicitElement(data, 0x0008, 0x103e, "LO", text("Deterministic payload integrity", ' '))
	data = appendDICOMExplicitElement(data, 0x0010, 0x0010, "PN", text("SYNTHETIC^RELAY", ' '))
	data = appendDICOMExplicitElement(data, 0x0010, 0x0020, "LO", text("RELAY-INTEROP", ' '))
	data = appendDICOMExplicitElement(data, 0x0010, 0x0030, "DA", text("19800101", ' '))
	data = appendDICOMExplicitElement(data, 0x0010, 0x0040, "CS", text("O", ' '))
	data = appendDICOMExplicitElement(data, 0x0020, 0x000d, "UI", ui("1.2.826.0.1.3680043.10.543.82.1"))
	data = appendDICOMExplicitElement(data, 0x0020, 0x000e, "UI", ui(seriesUID))
	data = appendDICOMExplicitElement(data, 0x0020, 0x0010, "SH", text("SYNTHETIC", ' '))
	data = appendDICOMExplicitElement(data, 0x0020, 0x0011, "IS", text("1", ' '))
	data = appendDICOMExplicitElement(data, 0x0020, 0x0013, "IS", text("1", ' '))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0002, "US", us(1))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0004, "CS", text("MONOCHROME2", ' '))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0010, "US", us(512))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0011, "US", us(512))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0100, "US", us(8))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0101, "US", us(8))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0102, "US", us(7))
	data = appendDICOMExplicitElement(data, 0x0028, 0x0103, "US", us(0))

	pixels := make([]byte, 512*512)
	for row := 0; row < 512; row++ {
		for column := 0; column < 512; column++ {
			pixels[row*512+column] = byte((row*17 + column*31 + (row*column)%251) % 256)
		}
	}
	return appendDICOMExplicitElement(data, 0x7fe0, 0x0010, "OB", pixels)
}

func appendDICOMExplicitElement(destination []byte, group, element uint16, vr string, value []byte) []byte {
	headerBytes := 8
	if dicomLongVR(vr) {
		headerBytes = 12
	}
	header := make([]byte, headerBytes)
	binary.LittleEndian.PutUint16(header[0:2], group)
	binary.LittleEndian.PutUint16(header[2:4], element)
	copy(header[4:6], vr)
	if headerBytes == 12 {
		binary.LittleEndian.PutUint32(header[8:12], uint32(len(value)))
	} else {
		binary.LittleEndian.PutUint16(header[6:8], uint16(len(value)))
	}
	destination = append(destination, header...)
	return append(destination, value...)
}

func dicomLongVR(vr string) bool {
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "SQ", "SV", "UC", "UN", "UR", "UT", "UV":
		return true
	default:
		return false
	}
}

func dicomPixelDataValue(dataset []byte) ([]byte, error) {
	marker := []byte{0xe0, 0x7f, 0x10, 0x00}
	offset := bytes.LastIndex(dataset, marker)
	if offset < 0 || offset+12 > len(dataset) {
		return nil, errors.New("Pixel Data element not found")
	}
	vr := string(dataset[offset+4 : offset+6])
	if vr != "OB" && vr != "OW" {
		return nil, fmt.Errorf("unexpected Pixel Data VR %q", vr)
	}
	valueBytes := binary.LittleEndian.Uint32(dataset[offset+8:])
	valueStart := offset + 12
	if valueBytes != 0xffffffff {
		valueEnd := valueStart + int(valueBytes)
		if valueEnd < valueStart || valueEnd > len(dataset) {
			return nil, errors.New("invalid Pixel Data length")
		}
		return dataset[valueStart:valueEnd], nil
	}
	for itemOffset := valueStart; itemOffset+8 <= len(dataset); {
		group := binary.LittleEndian.Uint16(dataset[itemOffset:])
		element := binary.LittleEndian.Uint16(dataset[itemOffset+2:])
		itemBytes := binary.LittleEndian.Uint32(dataset[itemOffset+4:])
		if group != 0xfffe {
			return nil, errors.New("invalid encapsulated Pixel Data item")
		}
		if element == 0xe0dd {
			if itemBytes != 0 {
				return nil, errors.New("invalid Pixel Data sequence delimiter")
			}
			return dataset[valueStart:itemOffset], nil
		}
		if element != 0xe000 || itemBytes == 0xffffffff {
			return nil, errors.New("invalid encapsulated Pixel Data fragment")
		}
		next := itemOffset + 8 + int(itemBytes)
		if next < itemOffset || next > len(dataset) {
			return nil, errors.New("truncated encapsulated Pixel Data fragment")
		}
		itemOffset = next
	}
	return nil, errors.New("unterminated encapsulated Pixel Data")
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

type orthancTestContainer struct {
	t      *testing.T
	name   string
	url    string
	closed atomic.Bool
}

func startOrthancContainer(t *testing.T, ctx context.Context, role string, modalities map[string]any) *orthancTestContainer {
	t.Helper()
	if modalities == nil {
		modalities = map[string]any{}
	}
	configuration := map[string]any{
		"Name":                       "Relay interoperability " + role,
		"RemoteAccessAllowed":        true,
		"AuthenticationEnabled":      false,
		"DicomServerEnabled":         false,
		"HttpServerEnabled":          true,
		"HttpServerPort":             8042,
		"StorageDirectory":           "/var/lib/orthanc/db",
		"IndexDirectory":             "/var/lib/orthanc/db",
		"Plugins":                    []string{"/usr/local/share/orthanc/plugins/libOrthancGdcm.so"},
		"DicomAssociationCloseDelay": 0,
		"TranscodeDicomProtocol":     false,
		"DicomModalities":            modalities,
	}
	contents, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "orthanc.json")
	if err := os.WriteFile(configPath, append(contents, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	realConfigPath, err := filepath.EvalSymlinks(configPath)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("relay-orthanc-%s-%d-%d", role, os.Getpid(), orthancContainerSequence.Add(1))
	arguments := []string{
		"run", "--detach", "--name", name,
		"--add-host", "host.docker.internal:host-gateway",
		"--publish", "127.0.0.1::8042",
		"--mount", "type=bind,source=" + realConfigPath + ",target=/etc/orthanc/orthanc.json,readonly",
		orthancInteropImage,
	}
	if output, err := exec.CommandContext(ctx, "docker", arguments...).CombinedOutput(); err != nil {
		t.Fatalf("start Orthanc %s: %v\n%s", role, err, output)
	}
	container := &orthancTestContainer{t: t, name: name}
	t.Cleanup(container.close)

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		portOutput, portErr := exec.CommandContext(ctx, "docker", "port", name, "8042/tcp").CombinedOutput()
		if portErr == nil {
			port := strings.TrimSpace(string(portOutput))
			if index := strings.LastIndex(port, ":"); index >= 0 {
				if _, conversionErr := strconv.Atoi(port[index+1:]); conversionErr == nil {
					container.url = "http://127.0.0.1:" + port[index+1:]
					requestContext, cancel := context.WithTimeout(ctx, time.Second)
					request, _ := http.NewRequestWithContext(requestContext, http.MethodGet, container.url+"/system", nil)
					response, requestErr := http.DefaultClient.Do(request)
					cancel()
					if requestErr == nil {
						_ = response.Body.Close()
						if response.StatusCode == http.StatusOK {
							return container
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			container.close()
			t.Fatalf("start Orthanc %s: %v", role, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	container.close()
	t.Fatalf("Orthanc %s did not become ready\n%s", role, logs)
	return nil
}

func (container *orthancTestContainer) close() {
	if container == nil || container.closed.Swap(true) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if container.t.Failed() {
		if logs, err := exec.CommandContext(ctx, "docker", "logs", container.name).CombinedOutput(); err == nil && len(logs) > 0 {
			container.t.Logf("Orthanc %s logs:\n%s", container.name, logs)
		}
	}
	if output, err := exec.CommandContext(ctx, "docker", "rm", "--force", container.name).CombinedOutput(); err != nil && ctx.Err() == nil {
		container.t.Logf("remove Orthanc %s: %v\n%s", container.name, err, output)
	}
}

func (container *orthancTestContainer) request(ctx context.Context, method, path, contentType string, body io.Reader) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, container.url+path, body)
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
	if readErr != nil {
		return nil, response.StatusCode, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseBody, response.StatusCode, fmt.Errorf("Orthanc %s %s returned HTTP %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, response.StatusCode, nil
}

func (container *orthancTestContainer) importInstance(t *testing.T, ctx context.Context, dicom []byte) string {
	t.Helper()
	body, _, err := container.request(ctx, http.MethodPost, "/instances", "application/dicom", bytes.NewReader(dicom))
	if err != nil {
		t.Fatalf("import DICOM into %s: %v", container.name, err)
	}
	var response struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	if err := json.Unmarshal(body, &response); err != nil || response.ID == "" || (response.Status != "Success" && response.Status != "AlreadyStored") {
		t.Fatalf("invalid Orthanc import response from %s: %s (%v)", container.name, body, err)
	}
	return response.ID
}

func (container *orthancTestContainer) instanceFile(t *testing.T, ctx context.Context, instanceID string) []byte {
	t.Helper()
	body, _, err := container.request(ctx, http.MethodGet, "/instances/"+instanceID+"/file", "", nil)
	if err != nil {
		t.Fatalf("download DICOM from %s: %v", container.name, err)
	}
	return body
}
