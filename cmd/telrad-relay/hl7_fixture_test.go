package main

import "github.com/telrad-au/relay/internal/synthetic"

func syntheticHL7IntegrityMessage(id string) []byte { return synthetic.HL7(id, 0) }
func syntheticHL7Acknowledgement(code, id, ackID string) []byte {
	return synthetic.ACK(code, id, ackID)
}
