package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	credentialRequestTimeout = 60 * time.Second
	credentialRetryMaximum   = 60 * time.Second
)

var credentialLifecycleMutex sync.Mutex

type credentialLifecycleMaterial struct {
	CredentialVersion   int       `json:"credentialVersion"`
	FamilyID            string    `json:"familyId"`
	Generation          int       `json:"generation"`
	AccessCredential    string    `json:"accessCredential"`
	AccessExpiresAt     time.Time `json:"accessExpiresAt"`
	RenewableCredential string    `json:"renewableCredential"`
	RenewableExpiresAt  time.Time `json:"renewableExpiresAt"`
}

type credentialLifecycleResponse struct {
	credentialLifecycleMaterial
	RenewalURL              string     `json:"renewalUrl,omitempty"`
	OldCredentialValidUntil *time.Time `json:"oldCredentialValidUntil,omitempty"`
}

func credentialEndpoint(pairingURL, path string) (string, error) {
	endpoints, err := deriveProtocolEndpoints(pairingURL)
	if err != nil {
		return "", err
	}
	endpoint, err := url.Parse(endpoints.PairingURL)
	if err != nil {
		return "", err
	}
	endpoint.Path = path
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	return endpoint.String(), nil
}

func renewalEndpoint(pairingURL string) (string, error) {
	return credentialEndpoint(pairingURL, "/v1/relay/credentials/renew")
}

func newCredentialOperation(kind string) (*credentialOperation, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return nil, errors.New("generate credential operation")
	}
	return &credentialOperation{Kind: kind, ID: base64.RawURLEncoding.EncodeToString(value[:])}, nil
}

func lifecycleRecord(material credentialLifecycleMaterial, renewalURL string, obtainedAt time.Time) credentialFile {
	return credentialFile{
		SchemaVersion:       credentialLifecycleSchemaVersion,
		CredentialVersion:   2,
		FamilyID:            material.FamilyID,
		Generation:          material.Generation,
		AccessCredential:    material.AccessCredential,
		AccessObtainedAt:    obtainedAt.UTC(),
		AccessExpiresAt:     material.AccessExpiresAt,
		RenewableCredential: material.RenewableCredential,
		RenewableExpiresAt:  material.RenewableExpiresAt,
		RenewalURL:          renewalURL,
	}
}

func validateLifecycleMaterial(material credentialLifecycleMaterial, previous credentialFile, migrating bool, now time.Time) error {
	if material.CredentialVersion != 2 || !validOpaqueID(material.FamilyID) || !accessCredentialPattern.MatchString(material.AccessCredential) || !renewableCredentialPattern.MatchString(material.RenewableCredential) {
		return errors.New("credential lifecycle response is invalid")
	}
	if !material.AccessExpiresAt.After(now) || !material.RenewableExpiresAt.After(material.AccessExpiresAt) {
		return errors.New("credential lifecycle response expiry is invalid")
	}
	if migrating {
		if material.Generation != 1 {
			return errors.New("credential migration generation is invalid")
		}
		return nil
	}
	if material.FamilyID != previous.FamilyID || material.Generation != previous.Generation+1 {
		return errors.New("credential renewal generation is invalid")
	}
	return nil
}

func credentialRenewalTime(record credentialFile) time.Time {
	if record.SchemaVersion != credentialLifecycleSchemaVersion || record.PendingOperation != nil {
		return time.Time{}
	}
	lifetime := record.AccessExpiresAt.Sub(record.AccessObtainedAt)
	if lifetime <= 0 {
		return time.Time{}
	}
	digest := sha256.Sum256([]byte(record.FamilyID))
	// Spread renewal deterministically between 75% and 85% of the observed
	// access lifetime so restarts do not discard the fleet-wide jitter.
	perTenThousand := int64(7500 + int(digest[0])*1000/256)
	return record.AccessObtainedAt.Add(time.Duration(int64(lifetime) * perTenThousand / 10000))
}

func credentialLifecycleDue(record credentialFile, now time.Time) bool {
	if record.SchemaVersion == credentialSchemaVersion || record.PendingOperation != nil {
		return true
	}
	return !credentialRenewalTime(record).After(now)
}

// advanceCredentialLifecycle serializes operation creation, the cloud request,
// and the atomic credential replacement. The pending operation reaches durable
// storage before the request, so a restart retries the same operation ID.
func advanceCredentialLifecycle(ctx context.Context, cfg *config, client *http.Client, force bool) (changed, terminal bool, returnErr error) {
	credentialLifecycleMutex.Lock()
	defer credentialLifecycleMutex.Unlock()
	fileLock, err := acquireCredentialOperationFileLock(cfg.CredentialPath)
	if err != nil {
		return false, false, errors.New("lock credential lifecycle state")
	}
	defer fileLock.Close()

	now := time.Now().UTC()
	record, err := readCredentialFile(cfg.CredentialPath, now)
	if err != nil {
		return false, false, errors.New("read credential lifecycle state")
	}
	migrating := record.SchemaVersion == credentialSchemaVersion
	if !migrating && !force && !credentialLifecycleDue(record, now) {
		return false, false, nil
	}
	if !migrating && !record.RenewableExpiresAt.After(now) {
		return false, true, errors.New("renewable credential expired")
	}
	kind := "renew"
	path := "/v1/relay/credentials/renew"
	authorization := record.RenewableCredential
	if migrating {
		kind = "migrate"
		path = "/v1/relay/credentials/migrate"
		authorization = record.Credential
	}
	if record.PendingOperation == nil {
		operation, err := newCredentialOperation(kind)
		if err != nil {
			return false, false, err
		}
		record.PendingOperation = operation
		if err := commitCredential(cfg.CredentialPath, record); err != nil {
			return false, false, errors.New("persist credential operation")
		}
	} else if record.PendingOperation.Kind != kind {
		return false, true, errors.New("credential operation state is invalid")
	}

	address, err := credentialEndpoint(cfg.PairingURL, path)
	if err != nil {
		return false, true, errors.New("credential lifecycle endpoint is invalid")
	}
	body, err := json.Marshal(map[string]string{"operationId": record.PendingOperation.ID})
	if err != nil {
		return false, false, errors.New("encode credential operation")
	}
	requestCtx, cancel := context.WithTimeout(ctx, credentialRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return false, false, errors.New("create credential lifecycle request")
	}
	req.Header.Set("Authorization", "Bearer "+authorization)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, false, errors.New("credential lifecycle network error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		terminal := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
		return false, terminal, errors.New("credential lifecycle request failed")
	}
	if !mediaTypeEquals(resp.Header.Get("Content-Type"), "application/json") {
		return false, false, errors.New("credential lifecycle response content type is invalid")
	}
	var result credentialLifecycleResponse
	if decodeBoundedJSON(resp.Body, maxCloudResponseBytes, &result) != nil {
		return false, false, errors.New("credential lifecycle response is invalid")
	}
	receivedAt := time.Now().UTC()
	if validateLifecycleMaterial(result.credentialLifecycleMaterial, record, migrating, receivedAt) != nil {
		return false, false, errors.New("credential lifecycle response is invalid")
	}
	renewalURL, err := renewalEndpoint(cfg.PairingURL)
	if err != nil {
		return false, true, errors.New("credential renewal endpoint is invalid")
	}
	if migrating {
		if result.RenewalURL != renewalURL || result.OldCredentialValidUntil == nil || !result.OldCredentialValidUntil.After(receivedAt) {
			return false, false, errors.New("credential migration response is invalid")
		}
	} else if result.RenewalURL != "" && result.RenewalURL != renewalURL {
		return false, false, errors.New("credential renewal response is invalid")
	}
	if err := commitCredential(cfg.CredentialPath, lifecycleRecord(result.credentialLifecycleMaterial, renewalURL, receivedAt)); err != nil {
		return false, false, errors.New("commit renewed credential")
	}
	return true, false, nil
}

func adoptCredentialLifecycle(provider *credentialProvider, changed chan<- struct{}, status *runtimeStatusManager) error {
	adopted, err := provider.Reload(time.Now())
	if err != nil {
		status.SetCredentialFileAttention(true)
		return err
	}
	status.SetCredentialFileAttention(false)
	if adopted {
		status.CredentialAdopted()
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	return nil
}

func maintainCredentialLifecycle(ctx context.Context, cfg *config, client *http.Client, provider *credentialProvider, changed chan<- struct{}, wake <-chan struct{}, status *runtimeStatusManager) {
	backoff := time.Second
	for ctx.Err() == nil {
		record := provider.Snapshot()
		now := time.Now()
		delay := time.Duration(0)
		if !credentialLifecycleDue(record, now) {
			delay = time.Until(credentialRenewalTime(record))
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
				backoff = time.Second
				continue
			case <-timer.C:
			}
		}
		advanced, terminal, err := advanceCredentialLifecycle(ctx, cfg, client, false)
		if advanced {
			if adoptCredentialLifecycle(provider, changed, status) == nil {
				backoff = time.Second
				continue
			}
			err = errors.New("adopt renewed credential")
		}
		latest := provider.Snapshot()
		if terminal || (latest.SchemaVersion == credentialLifecycleSchemaVersion && !latest.AccessExpiresAt.After(time.Now())) {
			status.SetAuthenticationAttention(true)
		}
		if err == nil {
			_ = adoptCredentialLifecycle(provider, changed, status)
			backoff = time.Second
			continue
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
			backoff = time.Second
			continue
		case <-timer.C:
		}
		backoff *= 2
		if backoff > credentialRetryMaximum {
			backoff = credentialRetryMaximum
		}
	}
}
