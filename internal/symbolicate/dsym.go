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

// DSYMAddr is one address to look up: the offset into the image (the
// sidecar runs llvm-symbolizer on the binary, so addresses are relative
// to the image base) and the image's basename.
type DSYMAddr struct {
	Address uint64
	Module  string
}

// DSYMResult is what the sidecar returns for one address; Function is
// "??" (or empty) when the address is not covered.
type DSYMResult struct {
	Function string `json:"function"`
	Filename string `json:"filename"`
	Lineno   int    `json:"lineno"`
}

// Resolved reports whether the sidecar found a symbol.
func (r DSYMResult) Resolved() bool {
	return r.Function != "" && r.Function != "??" && r.Function != "?"
}

// Resolve sends the dSYM plus addresses; the result is index-aligned with
// addrs (truncated to MaxDSYMFrames).
func (c *DSYMClient) Resolve(ctx context.Context, dsym []byte, addrs []DSYMAddr) ([]DSYMResult, error) {
	if len(addrs) > MaxDSYMFrames {
		addrs = addrs[:MaxDSYMFrames]
	}
	type addr struct {
		Address string `json:"address"`
		Module  string `json:"module"`
	}
	wire := make([]addr, len(addrs))
	for i, a := range addrs {
		wire[i] = addr{Address: "0x" + strconv.FormatUint(a.Address, 16), Module: a.Module}
	}
	hdr, _ := json.Marshal(wire)
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
		Frames []DSYMResult `json:"frames"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Frames, nil
}
