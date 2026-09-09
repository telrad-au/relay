package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

var remoteRunName = regexp.MustCompile(`^relay-perf-[0-9]+$`)

// Preserve the explicit archive modes: the public CA must remain readable by
// the release UID, while the workspace and configuration stay private.
const remoteExtract = "umask 077; tar -xpf - -C "

func validateRemoteDocker(endpoint, relay, support string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "ssh" || u.User == nil || u.User.Username() != "ec2-user" || u.Hostname() != relay || u.Port() != "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("remote Docker must be ssh://ec2-user@relay-private-ip")
	}
	if _, password := u.User.Password(); password {
		return errors.New("SSH passwords are not supported")
	}
	for _, host := range []string{relay, support} {
		ip := net.ParseIP(host)
		if ip.To4() == nil || !ip.IsPrivate() {
			return errors.New("remote screening requires private IPv4 addresses")
		}
	}
	if relay == support {
		return errors.New("Relay and supporting services require separate hosts")
	}
	return nil
}

func (r *runner) remoteCommand(args []string) bool {
	if r.remoteDocker == "" {
		return false
	}
	for _, arg := range args {
		if r.remoteContainers[arg] {
			return true
		}
	}
	return len(args) >= 3 && args[0] == "volume" && r.volume != "" && args[len(args)-1] == r.volume
}

func (r *runner) ssh(ctx context.Context, command string, input io.Reader) ([]byte, error) {
	u, err := url.Parse(r.remoteDocker)
	if err != nil {
		return nil, err
	}
	c := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", u.User.Username()+"@"+u.Hostname(), command)
	c.Stdin = input
	b, err := c.CombinedOutput()
	if err != nil {
		return nil, errors.New("remote test setup or cleanup failed")
	}
	return b, nil
}

func (r *runner) prepareRemote(ctx context.Context, res *result) error {
	// Only the validated run-owned temporary path is sent to the remote shell.
	if !validRemoteWorkspace(r.work, r.id) {
		return errors.New("remote controller requires a run-owned /tmp or /var/tmp workspace")
	}
	fmt.Println("Copying release and observer images to the isolated Relay VM...")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	save := exec.CommandContext(ctx, "docker", "image", "save", r.targetImage, r.helperImage)
	pipe, err := save.StdoutPipe()
	if err != nil {
		return err
	}
	if err := save.Start(); err != nil {
		return err
	}
	_, copyErr := r.ssh(ctx, "docker image load >/dev/null", pipe)
	if copyErr != nil {
		cancel()
	}
	saveErr := save.Wait()
	if copyErr != nil {
		return copyErr
	}
	if saveErr != nil {
		return errors.New("test image export failed")
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: r.id + "/", Mode: 0700, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	for _, name := range []string{"worker.json", "relay-defaults.json", "ca.pem"} {
		b, err := os.ReadFile(filepath.Join(r.work, name))
		if err != nil {
			return err
		}
		mode := int64(0600)
		if name == "ca.pem" {
			mode = 0644 // Public test CA must be readable by the release UID.
		}
		if err := tw.WriteHeader(&tar.Header{Name: r.id + "/" + name, Mode: mode, Size: int64(len(b))}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := r.ssh(ctx, remoteExtract+filepath.Dir(r.work), &archive); err != nil {
		return err
	}
	name, err := r.start(ctx, "target-hardware", nil, "telrad-relay-perf:local", []string{"host-info"})
	if err != nil {
		return err
	}
	if err := r.waitContainer(ctx, name); err != nil {
		return err
	}
	b, err := r.docker(ctx, "logs", name)
	if err != nil {
		return err
	}
	var hardware map[string]any
	if err := json.Unmarshal(b, &hardware); err != nil {
		return err
	}
	res.Host["relayHardware"] = hardware
	res.Host["relayDocker"] = r.remoteDocker
	return nil
}

func (r *runner) cleanupRemote(ctx context.Context) {
	if r.remoteDocker == "" || r.work == "" {
		return
	}
	if !validRemoteWorkspace(r.work, r.id) {
		r.cleanupErrors = append(r.cleanupErrors, "invalid_remote_cleanup_scope")
		return
	}
	if _, err := r.ssh(ctx, "rm -rf -- "+r.work, nil); err != nil {
		r.cleanupErrors = append(r.cleanupErrors, "remote_workspace_cleanup_failed")
	}
	for _, id := range []string{r.targetImage, r.helperImage} {
		if id == "" {
			continue
		}
		inspect := exec.CommandContext(ctx, "docker", "--host", r.remoteDocker, "image", "inspect", id)
		if b, err := inspect.CombinedOutput(); err != nil {
			if !bytes.Contains(b, []byte("No such image")) {
				r.cleanupErrors = append(r.cleanupErrors, "remote_image_inventory_failed")
			}
			continue
		}
		c := exec.CommandContext(ctx, "docker", "--host", r.remoteDocker, "image", "rm", id)
		if err := c.Run(); err != nil {
			r.cleanupErrors = append(r.cleanupErrors, "remote_image_cleanup_failed")
		}
	}
}

func validRemoteWorkspace(path, id string) bool {
	parent := filepath.Dir(path)
	return (parent == "/tmp" || parent == "/var/tmp") && path == filepath.Join(parent, id) && remoteRunName.MatchString(id)
}
