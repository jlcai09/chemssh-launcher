package chemssh

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"chemssh-launcher/internal/config"

	"golang.org/x/crypto/ssh"
)

type Identity struct {
	App            string `json:"app"`
	ProjectVersion string `json:"project_version"`
	PID            int    `json:"pid"`
	Scheduler      string `json:"scheduler"`
	WorkspaceRoot  string `json:"workspace_root"`
}

func FetchIdentity(ctx context.Context, client *ssh.Client, profile config.Profile) (Identity, error) {
	if client == nil {
		return Identity{}, fmt.Errorf("ssh client is not available")
	}
	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DialContext: sshDialContext(client)},
	}
	url := "http://" + profile.RemoteAddress() + "/api/system/identity"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Identity{}, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("fetch ChemSSH identity: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Identity{}, fmt.Errorf("fetch ChemSSH identity: unexpected HTTP status %d", resp.StatusCode)
	}
	var identity Identity
	if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
		return Identity{}, fmt.Errorf("decode ChemSSH identity: %w", err)
	}
	if identity.App != "chemssh" {
		return Identity{}, fmt.Errorf("remote service identity is %q, not chemssh", identity.App)
	}
	if identity.PID <= 0 {
		return Identity{}, fmt.Errorf("remote ChemSSH identity returned invalid pid %d", identity.PID)
	}
	return identity, nil
}

func sshDialContext(client *ssh.Client) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type dialResult struct {
			conn net.Conn
			err  error
		}
		ch := make(chan dialResult, 1)
		go func() {
			conn, err := client.Dial(network, address)
			ch <- dialResult{conn: conn, err: err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-ch:
			return result.conn, result.err
		}
	}
}
