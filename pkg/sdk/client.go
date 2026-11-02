package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultClientTimeout bounds a satellite's outbound call to Aether. The
// notifier the client backs has no retries of its own (plan §3.12).
const defaultClientTimeout = 10 * time.Second

// Client posts outbound responses to Aether's /message endpoint.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient returns a client for baseURL authenticating with token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: defaultClientTimeout},
	}
}

// Send posts resp to Aether. A non-2xx reply is an error so the caller can
// record a failed delivery.
func (c *Client) Send(ctx context.Context, resp Response) error {
	if c == nil {
		return errors.New("sdk: nil client")
	}

	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("sdk: marshal response: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/message", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sdk: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sdk: send: %w", err)
	}
	defer httpResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, 4<<10))

	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("sdk: send: unexpected status %s", httpResp.Status)
	}
	return nil
}
