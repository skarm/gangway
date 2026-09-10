package gangway

import (
	"context"
	"fmt"
	"net"
	"net/http"
)

// Probe sends HEAD /_ping through the listen socket and reports whether the
// proxy answered successfully. It exercises the full path a client takes,
// including Docker itself, so an unreachable daemon fails the probe.
func Probe(ctx context.Context, socketPath string) error {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, upstreamHost+pingPath, nil)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy returned HTTP %d", resp.StatusCode)
	}

	return nil
}
