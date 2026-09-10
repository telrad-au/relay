package main

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

type retrievalCompletion struct {
	Status                  string `json:"status"`
	Source                  string `json:"source,omitempty"`
	ExpectedInstanceCount   int    `json:"expectedInstanceCount,omitempty"`
	ExpectedInventorySHA256 string `json:"expectedInventorySha256,omitempty"`
}
type retrievalStudyResult struct {
	StudyInstanceUID    string              `json:"studyInstanceUid"`
	RetrieveStatus      string              `json:"retrieveStatus"`
	UniqueInstanceCount int                 `json:"uniqueInstanceCount"`
	InventorySHA256     string              `json:"inventorySha256"`
	Completion          retrievalCompletion `json:"completion"`
}
type retrievalResult struct {
	Outcome            string                 `json:"outcome"`
	RetrievalMethod    string                 `json:"retrievalMethod,omitempty"`
	OutstandingUploads *int                   `json:"outstandingUploads,omitempty"`
	Studies            []retrievalStudyResult `json:"studies,omitempty"`
}

func inventoryDigest(uids map[string]bool) string {
	sorted := make([]string, 0, len(uids))
	for s := range uids {
		sorted = append(sorted, s)
	}
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}
