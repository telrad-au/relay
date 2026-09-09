package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/telrad-au/relay/internal/retrieval"
)

type dicomJSONAttribute struct {
	VR           string            `json:"vr"`
	Value        []json.RawMessage `json:"Value"`
	BulkDataURI  string            `json:"BulkDataURI,omitempty"`
	InlineBinary string            `json:"InlineBinary,omitempty"`
}
type dicomJSON map[string]dicomJSONAttribute

func dicomJSONString(obj dicomJSON, tag, vr string) (string, error) {
	a, ok := obj[tag]
	if !ok || a.VR != vr || len(a.Value) != 1 || a.BulkDataURI != "" || a.InlineBinary != "" {
		return "", retrieval.ErrIdentity
	}
	var value string
	if json.Unmarshal(a.Value[0], &value) != nil || !retrieval.Identifier(value) {
		return "", retrieval.ErrIdentity
	}
	return value, nil
}
func dicomJSONIdentity(obj dicomJSON, p retrieval.Permit) (string, error) {
	expectedTags := map[string]string{"00080050": p.Examination.Accession}
	for tag, expected := range expectedTags {
		vr := "LO"
		if tag == "00080050" {
			vr = "SH"
		}
		s, e := dicomJSONString(obj, tag, vr)
		if e != nil || s != expected {
			return "", retrieval.ErrIdentity
		}
	}
	issuer := obj["00080051"]
	if issuer.VR != "SQ" || len(issuer.Value) != 1 {
		return "", retrieval.ErrIdentity
	}
	var item dicomJSON
	if retrieval.StrictJSON(issuer.Value[0], &item) != nil {
		return "", retrieval.ErrIdentity
	}
	s, e := dicomJSONString(item, "00400031", "UT")
	if e != nil || s != p.Examination.Issuer {
		return "", retrieval.ErrIdentity
	}
	study, e := dicomJSONString(obj, "0020000D", "UI")
	if e != nil || !retrieval.UID(study) {
		return "", retrieval.ErrIdentity
	}
	return study, nil
}
func pacsRequest(ctx context.Context, client *http.Client, address, accept string) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if e != nil {
		return nil, retrieval.ErrPolicy
	}
	req.Header.Set("Accept", accept)
	// The PACS client is separate from cloud bearer authentication and rejects
	// redirects. No response URL, BulkDataURI or Content-Location is followed.
	resp, e := client.Do(req)
	if e != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("network_timeout")
		}
		return nil, errors.New("pacs_unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == 408 || resp.StatusCode == 504 {
			return nil, errors.New("network_timeout")
		}
		if resp.StatusCode >= 500 {
			return nil, errors.New("pacs_unavailable")
		}
		return nil, errors.New("pacs_rejected")
	}
	return resp, nil
}
func queryAccession(ctx context.Context, client *http.Client, pacs retrievalPACS, p retrieval.Permit) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(pacs.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	q := url.Values{"00080050": {p.Examination.Accession}, "00080051.00400031": {p.Examination.Issuer}, "includefield": {"00080050", "00080051", "0020000D"}, "limit": {"65"}}
	resp, e := pacsRequest(ctx, client, pacs.DICOMwebURL+"/studies?"+q.Encode(), "application/dicom+json")
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	// Warnings indicate an incomplete QIDO set. Never accept a server-truncated
	// selection; the 65th result is only an overflow sentinel, never discarded.
	if resp.Header.Get("Warning") != "" || resp.Header.Get("Link") != "" {
		return nil, retrieval.ErrPolicy
	}
	if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/dicom+json") {
		return nil, errors.New("pacs_rejected")
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if e != nil || len(body) > 1024*1024 {
		return nil, retrieval.ErrPolicy
	}
	var studies []dicomJSON
	if retrieval.StrictJSON(body, &studies) != nil || strings.TrimSpace(string(body)) == "null" {
		return nil, errors.New("pacs_rejected")
	}
	if len(studies) > 64 {
		return nil, retrieval.ErrPolicy
	}
	seen := map[string]bool{}
	uids := make([]string, 0, len(studies))
	for _, obj := range studies {
		uid, e := dicomJSONIdentity(obj, p)
		if e != nil {
			return nil, e
		}
		if seen[uid] {
			return nil, errors.New("ambiguous_identity")
		}
		seen[uid] = true
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return nil, errors.New("not_found")
	}
	slices.Sort(uids)
	return uids, nil
}

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

func retrieveWADO(ctx context.Context, pacsClient, cloudClient *http.Client, cfg *config, provider *credentialProvider, status *runtimeStatusManager, pacs retrievalPACS, p retrieval.Permit, study, attempt string, recheck func() error, progress ...*retrievalProgress) (retrievalStudyResult, error) {
	result := retrievalStudyResult{StudyInstanceUID: study, RetrieveStatus: "success", Completion: retrievalCompletion{Status: "not_provided"}}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(pacs.RequestTimeoutSeconds)*time.Second)
	defer cancel()
	if e := recheck(); e != nil {
		return result, e
	}
	resp, e := pacsRequest(ctx, pacsClient, pacs.DICOMwebURL+"/studies/"+url.PathEscape(study), `multipart/related; type="application/dicom"; transfer-syntax=*`)
	if e != nil {
		return result, e
	}
	defer resp.Body.Close()
	media, params, e := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e != nil || media != "multipart/related" || params["type"] != "application/dicom" || params["boundary"] == "" || resp.Header.Get("Warning") != "" {
		return result, retrieval.ErrPartial
	}
	limited := &io.LimitedReader{R: resp.Body, N: pacs.MaxStudyBytes + 1}
	parts := multipart.NewReader(limited, params["boundary"])
	instances := map[string]bool{}
	arrivals := 0
	for {
		part, e := parts.NextRawPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			return result, retrieval.ErrPartial
		}
		arrivals++
		if arrivals > pacs.MaxInstances {
			part.Close()
			return result, retrieval.ErrPolicy
		}
		partType, partParams, partErr := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if partErr != nil || partType != "application/dicom" || len(partParams) > 1 || (len(partParams) == 1 && partParams["transfer-syntax"] == "") || part.Header.Get("Content-Transfer-Encoding") != "" {
			part.Close()
			return result, retrieval.ErrPartial
		}
		if e := recheck(); e != nil {
			part.Close()
			return result, e
		}
		if len(progress) > 0 {
			progress[0].received.Add(1)
		}
		stream, e := retrieval.OpenDICOM(part, p, study, pacs.MaxInstanceBytes)
		if e != nil {
			return result, e
		}
		if ts := partParams["transfer-syntax"]; ts != "" && ts != stream.TransferSyntax() {
			return result, retrieval.ErrPartial
		}
		reader, writer := io.Pipe()
		response := make(chan dicomIngestResult, 1)
		uploadCtx, stop := context.WithCancel(ctx)
		closePipe := context.AfterFunc(uploadCtx, func() { _ = reader.CloseWithError(context.Canceled); _ = writer.CloseWithError(context.Canceled) })
		go ingestDICOMWithAttempt(uploadCtx, cfg.DicomURL, cloudClient, provider, status, reader, response, attempt)
		_, copyErr := stream.WriteTo(writer)
		_ = writer.CloseWithError(copyErr)
		receipt := <-response
		closePipe()
		stop()
		part.Close()
		if copyErr != nil || receipt.status != 0 || ctx.Err() != nil {
			return result, retrieval.ErrPartial
		}
		if len(progress) > 0 {
			progress[0].uploaded.Add(1)
		}
		instances[stream.SOP] = true
	}
	// Consume the HTTP body too: a valid multipart terminator cannot mask a
	// truncated HTTP response. The body and all object uploads are bounded.
	_, e = io.Copy(io.Discard, limited)
	if e != nil || limited.N <= 0 || len(instances) == 0 || ctx.Err() != nil {
		return result, retrieval.ErrPartial
	}
	result.UniqueInstanceCount = len(instances)
	result.InventorySHA256 = inventoryDigest(instances)
	return result, nil
}
