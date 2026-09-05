package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	mllpStart = byte(0x0b)
	mllpEnd   = byte(0x1c)
	mllpCR    = byte(0x0d)
)

func serveHL7(ctx context.Context, connection net.Conn, cfg *config, client *http.Client, provider *credentialProvider, status *runtimeStatusManager) {
	policy := connectionPolicyFor(cfg, "hl7")
	clinic, _ := withStreamDeadlines(connection, nil, policy.idle, policy.lifetime)
	reader := bufio.NewReaderSize(clinic, 32*1024)
	for ctx.Err() == nil {
		frame, err := readMLLPFrameFrom(reader, cfg.HL7MaxBytes)
		if err != nil {
			return
		}
		controlID, err := hl7ControlID(frame[1 : len(frame)-2])
		if err != nil {
			return
		}
		ack, err := ingestHL7(ctx, cfg.HL7URL, client, provider, status, frame[1:len(frame)-2], controlID)
		if err != nil {
			return
		}
		response := make([]byte, 0, len(ack)+3)
		response = append(response, mllpStart)
		response = append(response, ack...)
		response = append(response, mllpEnd, mllpCR)
		if _, err := clinic.Write(response); err != nil {
			return
		}
	}
}

func readMLLPFrame(reader io.Reader, limit int64) ([]byte, error) {
	return readMLLPFrameFrom(bufio.NewReader(reader), limit)
}

func readMLLPFrameFrom(reader *bufio.Reader, limit int64) ([]byte, error) {
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
	frame := make([]byte, 1, minInt64(limit+3, 32*1024))
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
		frame = appendMLLPChunk(frame, chunk, limit)
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

func appendMLLPChunk(frame []byte, chunk []byte, limit int64) []byte {
	required := len(frame) + len(chunk)
	if required <= cap(frame) {
		return append(frame, chunk...)
	}
	capacity := cap(frame) * 2
	if capacity < required {
		capacity = required
	}
	if maximum := int(limit) + 3; capacity > maximum {
		capacity = maximum
	}
	grown := make([]byte, len(frame), capacity)
	copy(grown, frame)
	return append(grown, chunk...)
}

func minInt64(first int64, second int) int {
	if first < int64(second) {
		return int(first)
	}
	return second
}

func hl7ControlID(message []byte) (string, error) {
	if !utf8.Valid(message) {
		return "", errors.New("HL7 message is not valid UTF-8")
	}
	for remaining := message; len(remaining) > 0; {
		segment, rest := nextHL7Segment(remaining)
		remaining = rest
		if !isHL7Segment(segment, 'M', 'S', 'H') || len(segment) < 4 {
			continue
		}
		controlID, present := hl7Field(segment, segment[3], 9)
		controlID = bytes.TrimSpace(controlID)
		if !present || len(controlID) == 0 {
			return "", errors.New("HL7 message has no MSH-10")
		}
		return string(controlID), nil
	}
	return "", errors.New("HL7 message has no MSH segment")
}

func parseHL7Acknowledgement(message []byte) (string, string, error) {
	if !utf8.Valid(message) {
		return "", "", errors.New("ACK is not valid UTF-8")
	}
	separator := byte('|')
	for remaining := message; len(remaining) > 0; {
		segment, rest := nextHL7Segment(remaining)
		remaining = rest
		if isHL7Segment(segment, 'M', 'S', 'H') && len(segment) >= 4 {
			separator = segment[3]
			break
		}
	}
	for remaining := message; len(remaining) > 0; {
		segment, rest := nextHL7Segment(remaining)
		remaining = rest
		if !isHL7Segment(segment, 'M', 'S', 'A') || len(segment) < 4 || segment[3] != separator {
			continue
		}
		code, haveCode := hl7Field(segment, separator, 1)
		controlID, haveControlID := hl7Field(segment, separator, 2)
		controlID = bytes.TrimSpace(controlID)
		if !haveCode || !haveControlID || (!bytes.Equal(code, []byte("AA")) && !bytes.Equal(code, []byte("AE")) && !bytes.Equal(code, []byte("AR"))) || len(controlID) == 0 {
			return "", "", errors.New("ACK MSA segment is invalid")
		}
		return string(code), string(controlID), nil
	}
	return "", "", errors.New("ACK has no MSA segment")
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

func isHL7Segment(segment []byte, first byte, second byte, third byte) bool {
	return len(segment) >= 3 && segment[0] == first && segment[1] == second && segment[2] == third
}

func hl7Field(segment []byte, separator byte, wanted int) ([]byte, bool) {
	start := 0
	for field := 0; ; field++ {
		end := bytes.IndexByte(segment[start:], separator)
		if field == wanted {
			if end < 0 {
				return segment[start:], true
			}
			return segment[start : start+end], true
		}
		if end < 0 {
			return nil, false
		}
		start += end + 1
	}
}

func ingestHL7(parent context.Context, address string, client *http.Client, provider *credentialProvider, status *runtimeStatusManager, message []byte, controlID string) ([]byte, error) {
	key, err := randomHL7IdempotencyKey()
	if err != nil {
		return nil, errors.New("create ingest key")
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(message))
		if err != nil {
			return nil, errors.New("create HL7 ingest request")
		}
		req.GetBody = nil
		addRelayHeaders(req, provider, "application/hl7-v2", key)
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			if attempt < 2 && waitHL7Retry(ctx, time.Duration(attempt+1)*time.Second) == nil {
				continue
			}
			return nil, errors.New("HL7 ingest network failure")
		}
		ack, responseErr := consumeHL7Response(resp, controlID, status)
		if responseErr == nil {
			return ack, nil
		}
		if retryableHL7Status(resp.StatusCode) && attempt < 2 {
			delay := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now())
			if delay <= 0 {
				delay = time.Duration(attempt+1) * time.Second
			}
			if waitHL7Retry(ctx, delay) == nil {
				continue
			}
		}
		return nil, responseErr
	}
	return nil, errors.New("HL7 ingest attempts exhausted")
}

func consumeHL7Response(resp *http.Response, controlID string, status *runtimeStatusManager) ([]byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		status.SetAuthenticationAttention(true)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCloudResponseBytes))
		return nil, errors.New("HL7 ingest authentication failed")
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxCloudResponseBytes))
		return nil, fmt.Errorf("HL7 ingest returned http_%d", resp.StatusCode)
	}
	if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/hl7-v2") {
		return nil, errors.New("HL7 ingest returned invalid content type")
	}
	ack, err := io.ReadAll(io.LimitReader(resp.Body, maxCloudResponseBytes+1))
	if err != nil || len(ack) > maxCloudResponseBytes {
		return nil, errors.New("HL7 ingest returned an invalid body")
	}
	code, receivedControlID, err := parseHL7Acknowledgement(ack)
	if err != nil || receivedControlID != controlID {
		return nil, errors.New("HL7 ingest returned an invalid ACK")
	}
	if code != "AA" && code != "AE" && code != "AR" {
		return nil, errors.New("HL7 ingest returned an unsupported ACK")
	}
	return ack, nil
}

func retryableHL7Status(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func retryAfterDelay(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func waitHL7Retry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
