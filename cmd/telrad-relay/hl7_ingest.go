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
	start, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}
	if start != mllpStart {
		return nil, errors.New("invalid MLLP start byte")
	}
	frame := make([]byte, 1, minInt64(limit+3, 32*1024))
	frame[0] = start
	for int64(len(frame)-1) <= limit {
		value, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		frame = append(frame, value)
		if value != mllpEnd {
			continue
		}
		terminator, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		frame = append(frame, terminator)
		if terminator != mllpCR {
			return nil, errors.New("invalid MLLP terminator")
		}
		if int64(len(frame)-3) > limit {
			return nil, errors.New("HL7 message exceeds configured limit")
		}
		return frame, nil
	}
	return nil, errors.New("HL7 message exceeds configured limit")
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
	for _, segment := range strings.FieldsFunc(string(message), func(r rune) bool { return r == '\r' || r == '\n' }) {
		if !strings.HasPrefix(segment, "MSH") || len(segment) < 4 {
			continue
		}
		separator := string(segment[3])
		fields := strings.Split(segment, separator)
		if len(fields) <= 9 || strings.TrimSpace(fields[9]) == "" {
			return "", errors.New("HL7 message has no MSH-10")
		}
		return strings.TrimSpace(fields[9]), nil
	}
	return "", errors.New("HL7 message has no MSH segment")
}

func parseHL7Acknowledgement(message []byte) (string, string, error) {
	if !utf8.Valid(message) {
		return "", "", errors.New("ACK is not valid UTF-8")
	}
	separator := byte('|')
	segments := strings.FieldsFunc(string(message), func(r rune) bool { return r == '\r' || r == '\n' })
	for _, segment := range segments {
		if strings.HasPrefix(segment, "MSH") && len(segment) >= 4 {
			separator = segment[3]
			break
		}
	}
	for _, segment := range segments {
		prefix := "MSA" + string(separator)
		if !strings.HasPrefix(segment, prefix) {
			continue
		}
		fields := strings.Split(segment, string(separator))
		if len(fields) < 3 || (fields[1] != "AA" && fields[1] != "AE" && fields[1] != "AR") || strings.TrimSpace(fields[2]) == "" {
			return "", "", errors.New("ACK MSA segment is invalid")
		}
		return fields[1], strings.TrimSpace(fields[2]), nil
	}
	return "", "", errors.New("ACK has no MSA segment")
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
