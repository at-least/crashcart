package symbolicate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// MaxDSYMFrames bounds what is forwarded to the container per request.
const MaxDSYMFrames = 200

// DSYMClient talks to the symbolication container (container/symbolicate):
// the binary streams as the request body, frames ride in the X-Frames header.
type DSYMClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewDSYMClient returns a client for baseURL ("" disables it).
func NewDSYMClient(baseURL string) *DSYMClient {
	return &DSYMClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Enabled reports whether a container URL is configured.
func (c *DSYMClient) Enabled() bool { return c != nil && c.BaseURL != "" }

// Resolve sends the dSYM plus frames (lineno interpreted as an address).
func (c *DSYMClient) Resolve(ctx context.Context, dsym []byte, frames []Frame) ([]Frame, error) {
	if len(frames) > MaxDSYMFrames {
		frames = frames[:MaxDSYMFrames]
	}
	type addr struct {
		Address string `json:"address"`
		Module  string `json:"module"`
	}
	addrs := make([]addr, len(frames))
	for i, f := range frames {
		addrs[i] = addr{Address: "0x" + strconv.FormatInt(int64(f.Lineno), 16), Module: f.Filename}
	}
	hdr, _ := json.Marshal(addrs)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/symbolicate", bytes.NewReader(dsym))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Frames", string(hdr))
	req.ContentLength = int64(len(dsym))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("symbolicate container: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	var out struct {
		Frames []Frame `json:"frames"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Frames, nil
}
