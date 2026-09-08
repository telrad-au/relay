//go:build !relay_container

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"time"
)

const maxUpdateBinaryBytes = 100 * 1024 * 1024

type updateTransferMetadata struct {
	Manifest updateManifest `json:"manifest"`
	Platform string         `json:"platform"`
	Size     int64          `json:"size"`
}

func validTransferID(id string) bool {
	data, err := hex.DecodeString(id)
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == id
}

func approvedPayloadReader(release updateRelease, artifact []byte) (io.Reader, error) {
	metadata, err := json.Marshal(updateTransferMetadata{Manifest: release.Manifest, Platform: release.Platform, Size: int64(len(artifact))})
	if err != nil {
		return nil, err
	}
	if len(metadata) > maxCloudResponseBytes || len(artifact) > maxUpdateBinaryBytes {
		return nil, errors.New("update payload exceeds limit")
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(metadata)))
	return io.MultiReader(bytes.NewReader(header), bytes.NewReader(metadata), bytes.NewReader(artifact)), nil
}

func verifyApprovedPayload(trust updateTrust, approved string, metadata updateTransferMetadata, artifact []byte) error {
	if metadata.Manifest.Channel != trust.Channel {
		return errors.New("update channel does not match protected trust")
	}
	if metadata.Platform != runtime.GOOS+"-"+runtime.GOARCH {
		return errors.New("update platform does not match this host")
	}
	if metadata.Manifest.Version != approved {
		return errors.New("update does not match the exact approved version")
	}
	if metadata.Size != int64(len(artifact)) || metadata.Size < 1 || metadata.Size > maxUpdateBinaryBytes {
		return errors.New("invalid update artifact size")
	}
	item, ok := metadata.Manifest.Artifacts[metadata.Platform]
	if !ok {
		return errors.New("approved artifact is missing")
	}
	key, signature, err := decodeUpdateSignature(trust.PublicKey, item.Signature)
	if err != nil {
		return err
	}
	return verifyUpdateArtifact(key, metadata.Manifest, metadata.Platform, item, signature, artifact)
}

func submitApprovedUpdate(release updateRelease, artifact []byte) error {
	payload, err := approvedPayloadReader(release, artifact)
	if err != nil {
		return err
	}
	return invokeNativeAction([]string{"update", release.Manifest.Version}, payload)
}

func installApprovedPayload(ctx context.Context, approved string, payload io.Reader) error {
	if payload == nil {
		return errors.New("approved update payload is required")
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(payload, header); err != nil {
		return errors.New("missing update header")
	}
	size := binary.BigEndian.Uint32(header)
	if size == 0 || size > maxCloudResponseBytes {
		return errors.New("invalid update metadata size")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(payload, data); err != nil {
		return err
	}
	var metadata updateTransferMetadata
	if err := strictJSON(data, &metadata); err != nil {
		return errors.New("invalid update metadata")
	}
	if metadata.Size < 1 || metadata.Size > maxUpdateBinaryBytes {
		return errors.New("invalid update artifact size")
	}
	artifact := make([]byte, metadata.Size)
	if _, err := io.ReadFull(payload, artifact); err != nil {
		return errors.New("incomplete update artifact")
	}
	trust, err := loadUpdateTrust(defaultConfig(), nativePaths().Config)
	if err != nil {
		return err
	}
	if err := verifyApprovedPayload(trust, approved, metadata, artifact); err != nil {
		return err
	}
	precedence, err := compareReleaseVersions(version, approved)
	if err != nil {
		return err
	}
	if precedence >= 0 {
		return errors.New("approved update must be newer than the installed version")
	}
	if err := checkRuntimeReady(nativePaths().Config, time.Now()); err != nil {
		return err
	}
	state, err := readRuntimeStatus(nativePaths().Config)
	if err != nil {
		return err
	}
	if state.Version != version {
		return errors.New("running Relay version does not match the installed updater")
	}
	paths := nativePaths()
	if _, err := os.Lstat(updateJournalPath(paths.Executable)); err == nil {
		return errors.New("an interrupted update requires repair with a reviewed native installer")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The trusted installed program owns the entire transaction. It never
	// executes a candidate as an administrator installer or detaches completion.
	return applyUpdateAt(paths.Executable, paths.Config, approved, artifact)
}

func updatePreparationReady(path string) error {
	if nativeManagementEnabled(path) {
		return callManagement(context.Background(), "ready", io.Discard)
	}
	if err := checkRuntimeReady(path, time.Now()); err != nil {
		return err
	}
	state, err := readRuntimeStatus(path)
	if err != nil {
		return err
	}
	if state.Version != version {
		return errors.New("running Relay version does not match the update command version")
	}
	return nil
}

func callUpdatedDiagnostics() error {
	return callManagement(context.Background(), "doctor", io.Discard)
}
