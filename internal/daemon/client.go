package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

const clientHost = "cronmate.local"

// Client talks to a Daemon over its Unix domain socket.
type Client struct {
	socketPath string
	transport  *http.Transport
	httpClient *http.Client
}

// NewClient creates a local daemon client. It does not dial until a request is
// made.
func NewClient(socketPath string) (*Client, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, errors.New("daemon socket path cannot be empty")
	}

	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := dialer.DialContext(ctx, "unix", socketPath)
			if err != nil {
				return nil, fmt.Errorf("dial daemon unix socket %q: %w", socketPath, err)
			}
			return connection, nil
		},
	}

	return &Client{
		socketPath: socketPath,
		transport:  transport,
		httpClient: &http.Client{Transport: transport},
	}, nil
}

// Status fetches the daemon's current status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	if ctx == nil {
		return Status{}, errors.New("daemon client context cannot be nil")
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"http://"+clientHost+StatusEndpoint,
		nil,
	)
	if err != nil {
		return Status{}, fmt.Errorf("create daemon status request: %w", err)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return Status{}, fmt.Errorf("request daemon status: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return Status{}, fmt.Errorf(
			"daemon status returned %s: %s",
			response.Status,
			strings.TrimSpace(string(message)),
		)
	}

	var status Status
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&status); err != nil {
		return Status{}, fmt.Errorf("decode daemon status response: %w", err)
	}
	return status, nil
}

// Shutdown asks the currently identified daemon generation to stop. The
// instance ID should come from a fresh Status call; a restarted daemon rejects
// an old ID instead of being stopped accidentally.
func (c *Client) Shutdown(ctx context.Context, instanceID string) error {
	if ctx == nil {
		return errors.New("daemon client context cannot be nil")
	}
	if strings.TrimSpace(instanceID) == "" {
		return errors.New("daemon instance id cannot be empty")
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"http://"+clientHost+ShutdownEndpoint,
		nil,
	)
	if err != nil {
		return fmt.Errorf("create daemon shutdown request: %w", err)
	}
	request.Header.Set(InstanceIDHeader, instanceID)

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("request daemon shutdown: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf(
			"daemon shutdown returned %s: %s",
			response.Status,
			strings.TrimSpace(string(message)),
		)
	}

	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read daemon shutdown response: %w", err)
	}
	return nil
}

// CloseIdleConnections closes pooled connections held by the client.
func (c *Client) CloseIdleConnections() {
	c.transport.CloseIdleConnections()
}

// SocketPath returns the path this client dials.
func (c *Client) SocketPath() string {
	return c.socketPath
}
