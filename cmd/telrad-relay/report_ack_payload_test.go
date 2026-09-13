package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestReportResultPreservesValidatedRISACK(t *testing.T) {
	for _, code := range []string{"AA", "AE", "AR", "mismatched"} {
		t.Run(code, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			cfg := defaultConfig()
			cfg.ReportHost = "127.0.0.1"
			cfg.ReportPort = listener.Addr().(*net.TCPAddr).Port
			signer := reportTestSigner(t, cfg)
			payload := reportTestPayload("ACC")
			report := reportTestMessage(payload, signer(payload))
			ackCode := code
			id := report.MessageControlID
			if code == "mismatched" {
				ackCode = "AR"
				id = "other-message"
			}
			ack := "MSH|^~\\&|RIS|TEST|TELRAD|TEST|20260101000000||ACK|ack-1|P|2.5\rMSA|" + ackCode + "|" + id + "|Synthetic rejection café\rERR||OBR^1^18|101^Required field missing^HL70357|E||||Original RIS detail \\T\\\r"
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				frame, e := readMLLPFrame(conn, 1024*1024)
				if e != nil {
					return
				}
				if string(frame[1:len(frame)-2]) != payload {
					t.Error("report modified")
				}
				conn.Write(append(append([]byte{mllpStart}, []byte(ack)...), mllpEnd, mllpCR))
			}()
			result := deliverReport(context.Background(), cfg, report)
			<-done
			if code == "mismatched" {
				if result.AckPayload != "" || result.Outcome != "failed" {
					t.Fatal("uncorrelated ACK forwarded")
				}
				return
			}
			if result.AckPayload != ack || result.AckCode != code {
				t.Fatal("RIS ACK not preserved exactly")
			}
			if code == "AA" {
				if result.Outcome != "accepted" {
					t.Fatal("AA not accepted")
				}
			} else if result.Error != "clinic_rejected" || result.Outcome != "failed" {
				t.Fatal("rejection lost")
			}
			if strings.Contains(result.Error, "Original RIS detail") {
				t.Fatal("clinical ACK leaked into operational category")
			}
		})
	}
}
