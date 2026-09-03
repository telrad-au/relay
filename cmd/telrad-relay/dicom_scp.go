package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	maxDICOMPDUBytes     = 1024 * 1024
	maxDIMSECommandBytes = 64 * 1024
	maxDICOMRequestBytes = int64(1024 * 1024 * 1024)
	verificationSOPClass = "1.2.840.10008.1.1"
	implementationUID    = "2.25.326275436222435298054566996154181853250"
	implementationName   = "TELRAD_RELAY_V1"
)

var (
	dicomUIDPattern           = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)+$`)
	supportedTransferSyntaxes = map[string]struct{}{
		"1.2.840.10008.1.2": {}, "1.2.840.10008.1.2.1": {}, "1.2.840.10008.1.2.2": {},
		"1.2.840.10008.1.2.1.99": {}, "1.2.840.10008.1.2.1.98": {},
		"1.2.840.10008.1.2.4.50": {}, "1.2.840.10008.1.2.4.51": {},
		"1.2.840.10008.1.2.4.57": {}, "1.2.840.10008.1.2.4.70": {},
		"1.2.840.10008.1.2.4.80": {}, "1.2.840.10008.1.2.4.81": {},
		"1.2.840.10008.1.2.4.90": {}, "1.2.840.10008.1.2.4.91": {},
		"1.2.840.10008.1.2.4.92": {}, "1.2.840.10008.1.2.4.93": {},
		"1.2.840.10008.1.2.4.201": {}, "1.2.840.10008.1.2.4.202": {}, "1.2.840.10008.1.2.4.203": {},
		"1.2.840.10008.1.2.5": {},
	}
)

type presentationContext struct {
	ID             byte
	AbstractSyntax string
	TransferSyntax string
	Accepted       bool
}

type dicomCommand struct {
	Field       uint16
	MessageID   uint16
	SOPClassUID string
	SOPInstance string
	HasDataset  bool
}

type dicomIngestResult struct {
	status uint16
	abort  bool
}

var errAssociationRejected = errors.New("DICOM association rejected")

type pendingDICOMStore struct {
	context presentationContext
	command dicomCommand
	writer  *io.PipeWriter
	result  <-chan dicomIngestResult
	bytes   int64
	cancel  context.CancelFunc
}

func serveDICOM(ctx context.Context, connection net.Conn, cfg *config, client *http.Client, provider *credentialProvider, status *runtimeStatusManager) {
	policy := connectionPolicyFor(cfg, "dicom")
	clinic, _ := withStreamDeadlines(connection, nil, policy.idle, policy.lifetime)
	contexts, err := negotiateAssociation(clinic)
	if err != nil {
		if !errors.Is(err, errAssociationRejected) {
			_ = writeAbort(clinic, 2, 2)
		}
		return
	}
	var commandBuffer bytes.Buffer
	var commandContext byte
	var pending *pendingDICOMStore
	for ctx.Err() == nil {
		pduType, body, err := readDICOMPDU(clinic)
		if err != nil {
			if pending != nil && pending.writer != nil {
				pending.cancel()
				_ = pending.writer.CloseWithError(err)
			}
			return
		}
		switch pduType {
		case 0x04:
			pdvs, err := parsePDVs(body)
			if err != nil {
				_ = writeAbort(clinic, 2, 2)
				return
			}
			for _, pdv := range pdvs {
				contextValue, ok := contexts[pdv.contextID]
				if !ok || !contextValue.Accepted {
					_ = writeAbort(clinic, 2, 2)
					return
				}
				if pdv.command {
					if pending != nil || commandBuffer.Len()+len(pdv.data) > maxDIMSECommandBytes || (commandBuffer.Len() > 0 && commandContext != pdv.contextID) {
						_ = writeAbort(clinic, 2, 2)
						return
					}
					if commandBuffer.Len() == 0 {
						commandContext = pdv.contextID
					}
					commandBuffer.Write(pdv.data)
					if !pdv.last {
						continue
					}
					command, err := parseDIMSECommand(commandBuffer.Bytes())
					commandBuffer.Reset()
					if err != nil || command.SOPClassUID != contextValue.AbstractSyntax {
						_ = writeAbort(clinic, 2, 2)
						return
					}
					switch command.Field {
					case 0x0030:
						if command.HasDataset || contextValue.AbstractSyntax != verificationSOPClass {
							_ = writeAbort(clinic, 2, 2)
							return
						}
						if err := writeDIMSECommand(clinic, contextValue.ID, buildDIMSEStoreResponse(command, 0x8030, 0x0000)); err != nil {
							return
						}
					case 0x0001:
						if !command.HasDataset || command.SOPInstance == "" || !isStorageSOPClass(command.SOPClassUID) {
							_ = writeDIMSECommand(clinic, contextValue.ID, buildDIMSEStoreResponse(command, 0x8001, 0xA900))
							_ = writeAbort(clinic, 2, 2)
							return
						}
						pending = &pendingDICOMStore{context: contextValue, command: command}
					default:
						_ = writeAbort(clinic, 2, 2)
						return
					}
					continue
				}
				if pending == nil || contextValue.ID != pending.context.ID {
					_ = writeAbort(clinic, 2, 2)
					return
				}
				if pending.writer == nil {
					reader, writer := io.Pipe()
					pending.writer = writer
					result := make(chan dicomIngestResult, 1)
					pending.result = result
					requestCtx, cancel := context.WithCancel(ctx)
					pending.cancel = cancel
					header := buildPart10Header(pending.command.SOPClassUID, pending.command.SOPInstance, pending.context.TransferSyntax)
					pending.bytes = int64(len(header))
					go ingestDICOM(requestCtx, cfg.DicomURL, client, provider, status, reader, result)
					if _, err := writer.Write(header); err != nil {
						result := <-pending.result
						pending.cancel()
						_ = writeDIMSECommand(clinic, pending.context.ID, buildDIMSEStoreResponse(pending.command, 0x8001, result.status))
						if result.abort {
							_ = writeAbort(clinic, 2, 2)
						}
						return
					}
				}
				pending.bytes += int64(len(pdv.data))
				if pending.bytes > maxDICOMRequestBytes {
					pending.cancel()
					_ = pending.writer.CloseWithError(errors.New("DICOM request exceeds limit"))
					_ = writeDIMSECommand(clinic, pending.context.ID, buildDIMSEStoreResponse(pending.command, 0x8001, 0xA700))
					return
				}
				if _, err := pending.writer.Write(pdv.data); err != nil {
					result := <-pending.result
					pending.cancel()
					_ = writeDIMSECommand(clinic, pending.context.ID, buildDIMSEStoreResponse(pending.command, 0x8001, result.status))
					if result.abort {
						_ = writeAbort(clinic, 2, 2)
					}
					return
				}
				if pdv.last {
					_ = pending.writer.Close()
					result, disconnected, protocolCorruption := awaitDICOMIngest(clinic, pending.result, pending.cancel)
					pending.cancel()
					if protocolCorruption {
						_ = writeAbort(clinic, 2, 2)
						return
					}
					if disconnected {
						return
					}
					if err := writeDIMSECommand(clinic, pending.context.ID, buildDIMSEStoreResponse(pending.command, 0x8001, result.status)); err != nil {
						return
					}
					pending = nil
					if result.abort {
						_ = writeAbort(clinic, 2, 2)
						return
					}
				}
			}
		case 0x05:
			if pending != nil || len(body) != 4 {
				_ = writeAbort(clinic, 2, 2)
				return
			}
			_ = writeDICOMPDU(clinic, 0x06, make([]byte, 4))
			return
		case 0x07:
			return
		default:
			_ = writeAbort(clinic, 2, 2)
			return
		}
	}
}

type dicomReadProbe struct {
	count int
	err   error
}

// Once a dataset is complete, an association without an asynchronous-operations
// window must wait for C-STORE-RSP. A one-byte read can therefore detect clinic
// disconnects and cancel the in-flight HTTPS request without stealing a valid
// next DIMSE command.
func awaitDICOMIngest(connection net.Conn, result <-chan dicomIngestResult, cancel context.CancelFunc) (dicomIngestResult, bool, bool) {
	probe := make(chan dicomReadProbe, 1)
	go func() {
		buffer := make([]byte, 1)
		count, err := connection.Read(buffer)
		probe <- dicomReadProbe{count: count, err: err}
	}()
	select {
	case read := <-probe:
		cancel()
		response := <-result
		if read.count > 0 {
			return response, false, true
		}
		return response, true, false
	case response := <-result:
		_ = connection.SetReadDeadline(time.Now())
		read := <-probe
		_ = connection.SetReadDeadline(time.Time{})
		if read.count > 0 {
			cancel()
			return response, false, true
		}
		if read.err != nil {
			var networkError net.Error
			if !errors.As(read.err, &networkError) || !networkError.Timeout() {
				return response, true, false
			}
		}
		return response, false, false
	}
}

func readDICOMPDU(reader io.Reader) (byte, []byte, error) {
	header := make([]byte, 6)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[2:])
	if length > maxDICOMPDUBytes {
		return 0, nil, errors.New("DICOM PDU exceeds limit")
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(reader, body); err != nil {
		return 0, nil, err
	}
	return header[0], body, nil
}

func writeDICOMPDU(writer io.Writer, pduType byte, body []byte) error {
	header := []byte{pduType, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[2:], uint32(len(body)))
	_, err := writer.Write(append(header, body...))
	return err
}

func negotiateAssociation(connection net.Conn) (map[byte]presentationContext, error) {
	pduType, body, err := readDICOMPDU(connection)
	if err != nil || pduType != 0x01 || len(body) < 68 || binary.BigEndian.Uint16(body[:2]) != 1 {
		return nil, errors.New("invalid association request")
	}
	if strings.TrimSpace(string(body[4:20])) != "TELRAD" {
		_ = writeAssociationReject(connection, 1, 1, 7)
		return nil, errAssociationRejected
	}
	contexts := make(map[byte]presentationContext)
	applicationContext := ""
	for offset := 68; offset < len(body); {
		if offset+4 > len(body) {
			return nil, errors.New("truncated association item")
		}
		itemType := body[offset]
		length := int(binary.BigEndian.Uint16(body[offset+2 : offset+4]))
		offset += 4
		if length < 0 || offset+length > len(body) {
			return nil, errors.New("invalid association item length")
		}
		value := body[offset : offset+length]
		offset += length
		switch itemType {
		case 0x10:
			applicationContext = string(value)
		case 0x20:
			contextValue, err := parsePresentationContext(value)
			if err != nil {
				return nil, err
			}
			if _, exists := contexts[contextValue.ID]; exists {
				return nil, errors.New("duplicate presentation context")
			}
			contexts[contextValue.ID] = contextValue
		}
	}
	if applicationContext != "1.2.840.10008.3.1.1.1" || len(contexts) == 0 {
		_ = writeAssociationReject(connection, 1, 1, 2)
		return nil, errAssociationRejected
	}
	response := make([]byte, 68)
	binary.BigEndian.PutUint16(response[:2], 1)
	copy(response[4:20], padAE("TELRAD"))
	copy(response[20:36], body[20:36])
	response = append(response, associationItem(0x10, []byte(applicationContext))...)
	ids := make([]int, 0, len(contexts))
	for id := range contexts {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, numericID := range ids {
		id := byte(numericID)
		contextValue := contexts[id]
		result := byte(3)
		if contextValue.AbstractSyntax == verificationSOPClass || isStorageSOPClass(contextValue.AbstractSyntax) {
			result = 4
		}
		if contextValue.TransferSyntax != "" && result == 4 {
			result = 0
			contextValue.Accepted = true
			contexts[id] = contextValue
		}
		value := []byte{id, 0, result, 0}
		if result == 0 {
			value = append(value, associationItem(0x40, []byte(contextValue.TransferSyntax))...)
		}
		response = append(response, associationItem(0x21, value)...)
	}
	userInfo := associationItem(0x51, []byte{0, 1, 0, 0})
	userInfo = append(userInfo, associationItem(0x52, []byte(implementationUID))...)
	userInfo = append(userInfo, associationItem(0x55, []byte(implementationName))...)
	response = append(response, associationItem(0x50, userInfo)...)
	if err := writeDICOMPDU(connection, 0x02, response); err != nil {
		return nil, err
	}
	return contexts, nil
}

func parsePresentationContext(value []byte) (presentationContext, error) {
	if len(value) < 4 || value[0] == 0 || value[0]%2 == 0 {
		return presentationContext{}, errors.New("invalid presentation context ID")
	}
	result := presentationContext{ID: value[0]}
	abstractSyntaxes := 0
	for offset := 4; offset < len(value); {
		if offset+4 > len(value) {
			return presentationContext{}, errors.New("truncated presentation context")
		}
		itemType := value[offset]
		length := int(binary.BigEndian.Uint16(value[offset+2 : offset+4]))
		offset += 4
		if length == 0 || offset+length > len(value) {
			return presentationContext{}, errors.New("invalid presentation syntax length")
		}
		uid := string(value[offset : offset+length])
		offset += length
		if !validDICOMUID(uid) {
			continue
		}
		if itemType == 0x30 && result.AbstractSyntax == "" {
			result.AbstractSyntax = uid
			abstractSyntaxes++
		} else if itemType == 0x30 {
			abstractSyntaxes++
		}
		if itemType == 0x40 && result.TransferSyntax == "" {
			if _, ok := supportedTransferSyntaxes[uid]; ok {
				result.TransferSyntax = uid
			}
		}
	}
	if result.AbstractSyntax == "" || abstractSyntaxes != 1 {
		return presentationContext{}, errors.New("presentation context has no abstract syntax")
	}
	return result, nil
}

func associationItem(itemType byte, value []byte) []byte {
	item := []byte{itemType, 0, 0, 0}
	binary.BigEndian.PutUint16(item[2:], uint16(len(value)))
	return append(item, value...)
}

func padAE(value string) []byte {
	result := bytes.Repeat([]byte{' '}, 16)
	copy(result, value)
	return result
}

func writeAssociationReject(writer io.Writer, result, source, reason byte) error {
	return writeDICOMPDU(writer, 0x03, []byte{0, result, source, reason})
}

func writeAbort(writer io.Writer, source, reason byte) error {
	return writeDICOMPDU(writer, 0x07, []byte{0, 0, source, reason})
}

func validDICOMUID(value string) bool {
	if len(value) == 0 || len(value) > 64 || !dicomUIDPattern.MatchString(value) || strings.Contains(value, "..") {
		return false
	}
	for _, component := range strings.Split(value, ".") {
		if len(component) > 1 && component[0] == '0' {
			return false
		}
	}
	return true
}

func isStorageSOPClass(value string) bool {
	return validDICOMUID(value) && strings.HasPrefix(value, "1.2.840.10008.5.1.4.1.1.")
}

type presentationDataValue struct {
	contextID byte
	command   bool
	last      bool
	data      []byte
}

func parsePDVs(body []byte) ([]presentationDataValue, error) {
	var values []presentationDataValue
	for offset := 0; offset < len(body); {
		if offset+4 > len(body) {
			return nil, errors.New("truncated PDV")
		}
		length := int(binary.BigEndian.Uint32(body[offset : offset+4]))
		offset += 4
		if length < 2 || offset+length > len(body) {
			return nil, errors.New("invalid PDV length")
		}
		header := body[offset+1]
		if header&^byte(0x03) != 0 {
			return nil, errors.New("invalid PDV header")
		}
		values = append(values, presentationDataValue{contextID: body[offset], command: header&1 != 0, last: header&2 != 0, data: body[offset+2 : offset+length]})
		offset += length
	}
	return values, nil
}

func parseDIMSECommand(data []byte) (dicomCommand, error) {
	if len(data) > maxDIMSECommandBytes {
		return dicomCommand{}, errors.New("DIMSE command exceeds limit")
	}
	result := dicomCommand{}
	var haveField, haveMessage, haveDataset bool
	var declaredGroupLength int = -1
	for offset := 0; offset < len(data); {
		if offset+8 > len(data) {
			return dicomCommand{}, errors.New("truncated DIMSE command element")
		}
		group := binary.LittleEndian.Uint16(data[offset:])
		element := binary.LittleEndian.Uint16(data[offset+2:])
		length := int(binary.LittleEndian.Uint32(data[offset+4:]))
		offset += 8
		if group != 0 || length < 0 || offset+length > len(data) {
			return dicomCommand{}, errors.New("invalid DIMSE command element")
		}
		value := data[offset : offset+length]
		offset += length
		switch element {
		case 0x0000:
			if len(value) != 4 || declaredGroupLength != -1 {
				return dicomCommand{}, errors.New("invalid command group length")
			}
			declaredGroupLength = int(binary.LittleEndian.Uint32(value))
		case 0x0002:
			result.SOPClassUID = strings.TrimRight(string(value), "\x00 ")
		case 0x0100:
			if len(value) != 2 {
				return dicomCommand{}, errors.New("invalid command field")
			}
			result.Field, haveField = binary.LittleEndian.Uint16(value), true
		case 0x0110:
			if len(value) != 2 {
				return dicomCommand{}, errors.New("invalid message ID")
			}
			result.MessageID, haveMessage = binary.LittleEndian.Uint16(value), true
		case 0x0800:
			if len(value) != 2 {
				return dicomCommand{}, errors.New("invalid dataset type")
			}
			datasetType := binary.LittleEndian.Uint16(value)
			result.HasDataset, haveDataset = datasetType != 0x0101, true
		case 0x1000:
			result.SOPInstance = strings.TrimRight(string(value), "\x00 ")
		}
	}
	if declaredGroupLength != len(data)-12 || !haveField || !haveMessage || !haveDataset || !validDICOMUID(result.SOPClassUID) || (result.SOPInstance != "" && !validDICOMUID(result.SOPInstance)) {
		return dicomCommand{}, errors.New("DIMSE command is incomplete")
	}
	return result, nil
}

func buildDIMSEStoreResponse(command dicomCommand, responseField, status uint16) []byte {
	var body bytes.Buffer
	writeCommandUI(&body, 0x0002, command.SOPClassUID)
	writeCommandUS(&body, 0x0100, responseField)
	writeCommandUS(&body, 0x0120, command.MessageID)
	writeCommandUS(&body, 0x0800, 0x0101)
	writeCommandUS(&body, 0x0900, status)
	if command.SOPInstance != "" {
		writeCommandUI(&body, 0x1000, command.SOPInstance)
	}
	groupLength := make([]byte, 12)
	binary.LittleEndian.PutUint16(groupLength[0:], 0)
	binary.LittleEndian.PutUint16(groupLength[2:], 0)
	binary.LittleEndian.PutUint32(groupLength[4:], 4)
	binary.LittleEndian.PutUint32(groupLength[8:], uint32(body.Len()))
	return append(groupLength, body.Bytes()...)
}

func writeCommandUS(buffer *bytes.Buffer, element, value uint16) {
	_ = binary.Write(buffer, binary.LittleEndian, uint16(0))
	_ = binary.Write(buffer, binary.LittleEndian, element)
	_ = binary.Write(buffer, binary.LittleEndian, uint32(2))
	_ = binary.Write(buffer, binary.LittleEndian, value)
}

func writeCommandUI(buffer *bytes.Buffer, element uint16, value string) {
	encoded := []byte(value)
	if len(encoded)%2 != 0 {
		encoded = append(encoded, 0)
	}
	_ = binary.Write(buffer, binary.LittleEndian, uint16(0))
	_ = binary.Write(buffer, binary.LittleEndian, element)
	_ = binary.Write(buffer, binary.LittleEndian, uint32(len(encoded)))
	buffer.Write(encoded)
}

func writeDIMSECommand(writer io.Writer, contextID byte, command []byte) error {
	pdv := make([]byte, 6+len(command))
	binary.BigEndian.PutUint32(pdv[:4], uint32(len(command)+2))
	pdv[4] = contextID
	pdv[5] = 0x03
	copy(pdv[6:], command)
	return writeDICOMPDU(writer, 0x04, pdv)
}

func buildPart10Header(classUID, instanceUID, transferSyntax string) []byte {
	var meta bytes.Buffer
	writeMetaElement(&meta, 0x0001, "OB", []byte{0, 1})
	writeMetaElement(&meta, 0x0002, "UI", paddedDICOMString(classUID, 0))
	writeMetaElement(&meta, 0x0003, "UI", paddedDICOMString(instanceUID, 0))
	writeMetaElement(&meta, 0x0010, "UI", paddedDICOMString(transferSyntax, 0))
	writeMetaElement(&meta, 0x0012, "UI", paddedDICOMString(implementationUID, 0))
	writeMetaElement(&meta, 0x0013, "SH", paddedDICOMString(implementationName, ' '))
	result := make([]byte, 132)
	copy(result[128:], "DICM")
	var groupLength bytes.Buffer
	writeMetaElement(&groupLength, 0x0000, "UL", littleEndianUint32(uint32(meta.Len())))
	result = append(result, groupLength.Bytes()...)
	return append(result, meta.Bytes()...)
}

func writeMetaElement(buffer *bytes.Buffer, element uint16, vr string, value []byte) {
	_ = binary.Write(buffer, binary.LittleEndian, uint16(2))
	_ = binary.Write(buffer, binary.LittleEndian, element)
	buffer.WriteString(vr)
	if vr == "OB" {
		buffer.Write([]byte{0, 0})
		_ = binary.Write(buffer, binary.LittleEndian, uint32(len(value)))
	} else {
		_ = binary.Write(buffer, binary.LittleEndian, uint16(len(value)))
	}
	buffer.Write(value)
}

func paddedDICOMString(value string, padding byte) []byte {
	result := []byte(value)
	if len(result)%2 != 0 {
		result = append(result, padding)
	}
	return result
}

func littleEndianUint32(value uint32) []byte {
	result := make([]byte, 4)
	binary.LittleEndian.PutUint32(result, value)
	return result
}

func ingestDICOM(ctx context.Context, address string, client *http.Client, provider *credentialProvider, status *runtimeStatusManager, body *io.PipeReader, result chan<- dicomIngestResult) {
	defer body.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, body)
	if err != nil {
		result <- dicomIngestResult{status: 0xA700}
		return
	}
	req.GetBody = nil
	addRelayHeaders(req, provider, "application/dicom", "")
	resp, err := client.Do(req)
	if err != nil {
		result <- dicomIngestResult{status: 0xA700}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		status.SetAuthenticationAttention(true)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCloudResponseBytes))
		result <- dicomIngestResult{status: 0xA700, abort: true}
		return
	}
	if resp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCloudResponseBytes))
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode <= 599) {
			result <- dicomIngestResult{status: 0xA700}
		} else if resp.StatusCode >= 400 && resp.StatusCode <= 499 {
			result <- dicomIngestResult{status: 0xA900}
		} else {
			result <- dicomIngestResult{status: 0xC000}
		}
		return
	}
	var receipt struct {
		Status    string `json:"status"`
		ReceiptID string `json:"receiptId"`
		// The cloud may expose its business-level duplicate classification, but
		// Relay never uses it as transport identity or suppresses an arrival.
		Duplicate *bool `json:"duplicate,omitempty"`
	}
	validType := mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json")
	validBody := decodeBoundedJSON(resp.Body, maxCloudResponseBytes, &receipt) == nil
	validSemantics := receipt.Status == "accepted" && validOpaqueID(receipt.ReceiptID)
	if !validType || !validBody || !validSemantics {
		result <- dicomIngestResult{status: 0xC000}
		return
	}
	result <- dicomIngestResult{status: 0x0000}
}
