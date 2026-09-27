package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

const (
	mllpStart = byte(0x0b)
	mllpEnd   = byte(0x1c)
	mllpCR    = byte(0x0d)
)

// readMLLPFrame reads one complete frame including its framing bytes. The
// payload is bounded by limit; anything else on the stream is an error.
func readMLLPFrame(reader *bufio.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, errors.New("HL7 message limit is invalid")
	}
	start, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if start != mllpStart {
		return nil, errors.New("invalid MLLP start byte")
	}
	frame := make([]byte, 1, min(limit+3, 32*1024))
	frame[0] = start
	for {
		chunk, readErr := reader.ReadSlice(mllpEnd)
		payloadBytes := len(chunk)
		if readErr == nil {
			payloadBytes--
		}
		if int64(payloadBytes) > limit-int64(len(frame)-1) {
			return nil, errors.New("HL7 message exceeds configured limit")
		}
		frame = append(frame, chunk...)
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		terminator, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		frame = append(frame, terminator)
		if terminator != mllpCR {
			return nil, errors.New("invalid MLLP terminator")
		}
		return frame, nil
	}
}

// readMLLPFrameWithDeadlines waits up to idle for a frame to start and then
// requires the started frame to complete within frameTimeout. A zero idle
// waits indefinitely for the first byte.
func readMLLPFrameWithDeadlines(conn net.Conn, reader *bufio.Reader, limit int64, idle, frameTimeout time.Duration) ([]byte, error) {
	if reader.Buffered() == 0 {
		if idle > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(idle))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}
		if _, err := reader.Peek(1); err != nil {
			return nil, err
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(frameTimeout))
	frame, err := readMLLPFrame(reader, limit)
	_ = conn.SetReadDeadline(time.Time{})
	return frame, err
}

func frameMessage(message []byte) []byte {
	framed := make([]byte, 0, len(message)+3)
	framed = append(framed, mllpStart)
	framed = append(framed, message...)
	return append(framed, mllpEnd, mllpCR)
}

func unframe(frame []byte) []byte { return frame[1 : len(frame)-2] }

// hl7Message is a minimal ER7 reader. Relay reads a handful of fields and
// never rewrites a message, so it keeps only the separator and the raw
// segments.
type hl7Message struct {
	fieldSeparator     byte
	componentSeparator byte
	encodingCharacters string
	segments           [][]byte
}

// parseHL7 reads only ASCII delimiters, so it accepts any character set a
// clinic uses (ISO 8859-1 is common); Relay never decodes field text.
func parseHL7(message []byte) (*hl7Message, error) {
	parsed := &hl7Message{}
	for remaining := message; len(remaining) > 0; {
		segment, rest := nextHL7Segment(remaining)
		remaining = rest
		if len(segment) == 0 {
			continue
		}
		parsed.segments = append(parsed.segments, segment)
	}
	if len(parsed.segments) == 0 || !bytes.HasPrefix(parsed.segments[0], []byte("MSH")) || len(parsed.segments[0]) < 8 {
		return nil, errors.New("HL7 message has no MSH segment")
	}
	msh := parsed.segments[0]
	parsed.fieldSeparator = msh[3]
	encoding := msh[4:]
	if end := bytes.IndexByte(encoding, parsed.fieldSeparator); end >= 0 {
		encoding = encoding[:end]
	}
	if len(encoding) < 1 {
		return nil, errors.New("HL7 message has no encoding characters")
	}
	parsed.encodingCharacters = string(encoding)
	parsed.componentSeparator = encoding[0]
	return parsed, nil
}

func nextHL7Segment(message []byte) ([]byte, []byte) {
	for len(message) > 0 && (message[0] == '\r' || message[0] == '\n') {
		message = message[1:]
	}
	for index, value := range message {
		if value == '\r' || value == '\n' {
			return message[:index], message[index+1:]
		}
	}
	return message, nil
}

// field returns HL7 field number wanted (1-based) of the first segment named
// name. MSH-1 is the field separator and MSH-2 the encoding characters.
func (message *hl7Message) field(name string, wanted int) (string, bool) {
	for _, segment := range message.segments {
		if !bytes.HasPrefix(segment, []byte(name)) || len(segment) <= 3 || segment[3] != message.fieldSeparator {
			continue
		}
		return message.segmentField(segment, wanted)
	}
	return "", false
}

// fields returns field wanted from every segment named name, in order.
func (message *hl7Message) fields(name string, wanted int) []string {
	var values []string
	for _, segment := range message.segments {
		if !bytes.HasPrefix(segment, []byte(name)) || len(segment) <= 3 || segment[3] != message.fieldSeparator {
			continue
		}
		value, ok := message.segmentField(segment, wanted)
		if ok {
			values = append(values, value)
		}
	}
	return values
}

func (message *hl7Message) segmentField(segment []byte, wanted int) (string, bool) {
	parts := bytes.Split(segment, []byte{message.fieldSeparator})
	if bytes.Equal(parts[0], []byte("MSH")) {
		if wanted == 1 {
			return string(message.fieldSeparator), true
		}
		wanted--
	}
	if wanted < 1 || wanted >= len(parts) {
		return "", false
	}
	return string(parts[wanted]), true
}

func (message *hl7Message) controlID() string {
	value, _ := message.field("MSH", 10)
	return strings.TrimSpace(value)
}

func (message *hl7Message) orderControl() string {
	value, _ := message.field("ORC", 1)
	return strings.TrimSpace(value)
}

// accessions returns every OBR-18 exactly as sent. Values containing a line
// break cannot be stored and are dropped; such a value is not valid HL7.
func (message *hl7Message) accessions() []string {
	var values []string
	for _, value := range message.fields("OBR", 18) {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\r\n") {
			continue
		}
		values = append(values, value)
	}
	return values
}

// acknowledgement returns MSA-1 and MSA-2 when the message is an ACK.
func (message *hl7Message) acknowledgement() (code string, controlID string, ok bool) {
	code, haveCode := message.field("MSA", 1)
	controlID, haveControl := message.field("MSA", 2)
	code, controlID = strings.TrimSpace(code), strings.TrimSpace(controlID)
	if !haveCode || !haveControl || controlID == "" || (code != "AA" && code != "AE" && code != "AR") {
		return "", "", false
	}
	return code, controlID, true
}

func isOrderPlacement(orderControl string) bool { return orderControl == "NW" || orderControl == "XO" }

// composeAck builds the acknowledgement Relay sends itself: a refusal or a
// receiver failure. It uses the original message's separators, swaps the
// sending and receiving applications and copies the processing ID.
func composeAck(original *hl7Message, code, text string) []byte {
	separator := string(original.fieldSeparator)
	component := string(original.componentSeparator)
	get := func(wanted int) string {
		value, _ := original.field("MSH", wanted)
		return value
	}
	messageType := "ACK"
	if parts := strings.SplitN(get(9), component, 3); len(parts) >= 2 && parts[1] != "" {
		messageType = "ACK" + component + parts[1] + component + "ACK"
	}
	versionID := get(12)
	if versionID == "" {
		versionID = "2.5.1"
	}
	msh := strings.Join([]string{
		"MSH" + separator + original.encodingCharacters, get(5), get(6), get(3), get(4),
		time.Now().UTC().Format("20060102150405"), "", messageType, ackControlID(), get(11), versionID,
	}, separator)
	msa := strings.Join([]string{"MSA", code, original.controlID(), sanitizeHL7Text(text, original)}, separator)
	return []byte(msh + "\r" + msa + "\r")
}

func ackControlID() string {
	suffix := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, suffix); err != nil {
		return "TR" + time.Now().UTC().Format("20060102150405")
	}
	return "TR" + hex.EncodeToString(suffix)
}

// sanitizeHL7Text removes any delimiter from text so a fixed status message
// can never start another field or segment.
func sanitizeHL7Text(text string, message *hl7Message) string {
	forbidden := string(message.fieldSeparator) + message.encodingCharacters + "\r\n"
	return strings.Map(func(character rune) rune {
		if strings.ContainsRune(forbidden, character) {
			return ' '
		}
		return character
	}, text)
}
