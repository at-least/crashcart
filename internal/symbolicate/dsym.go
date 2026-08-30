package symbolicate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// MaxDSYMFrames bounds what is forwarded to the sidecar per request.
const MaxDSYMFrames = 200

// DSYMClient talks to the symbolication sidecar (Sidecar, run as
// `crashcart symbolicate`). The sidecar caches symbol files by key; a
// request names the key and only when the sidecar does not have the file
// yet are its bytes read from the database and sent (PUT).
type DSYMClient struct {
	BaseURL string
	HTTP    *http.Client
}

// NewDSYMClient returns a client for baseURL ("" disables it).
func NewDSYMClient(baseURL string) *DSYMClient {
	return &DSYMClient{BaseURL: baseURL, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// Enabled reports whether a sidecar URL is configured.
func (c *DSYMClient) Enabled() bool { return c != nil && c.BaseURL != "" }

// ErrNotCached: the sidecar does not have the symbol file and the caller
// asked not to send it (Resolve with a nil loader — the ingest path,
// which leaves the upload to the job worker).
var ErrNotCached = errors.New("symbol file not cached by the sidecar")

// DSYMAddr is one address to look up: the offset into the image (the
// sidecar runs llvm-symbolizer on the binary, so addresses are relative
// to the image base) and the image's basename.
type DSYMAddr struct {
	Address uint64
	Module  string
}

// DSYMResult is what the sidecar returns for one address; Function is
// empty (or "??") when the address is not covered.
type DSYMResult struct {
	Function string `json:"function"`
	Filename string `json:"filename"`
	Lineno   int    `json:"lineno"`
}

// Resolved reports whether the sidecar found a symbol.
func (r DSYMResult) Resolved() bool {
	return r.Function != "" && r.Function != "??" && r.Function != "?"
}

// Resolve symbolicates addrs against the symbol file named key. When the
// sidecar does not have the file, load is called for its bytes, which are
// sent once (PUT) before retrying; with load nil, ErrNotCached is
// returned instead. The result is index-aligned with addrs (truncated to
// MaxDSYMFrames).
func (c *DSYMClient) Resolve(ctx context.Context, key string, load func(context.Context) ([]byte, error), addrs []DSYMAddr) ([]DSYMResult, error) {
	if len(addrs) > MaxDSYMFrames {
		addrs = addrs[:MaxDSYMFrames]
	}
	results, found, err := c.symbolicate(ctx, key, addrs)
	if err != nil || found {
		return results, err
	}
	if load == nil {
		return nil, ErrNotCached
	}
	data, err := load(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.put(ctx, key, data); err != nil {
		return nil, err
	}
	results, found, err = c.symbolicate(ctx, key, addrs)
	if err == nil && !found {
		err = fmt.Errorf("symbolicate sidecar: %s still unknown after upload", key)
	}
	return results, err
}

func (c *DSYMClient) symbolicate(ctx context.Context, key string, addrs []DSYMAddr) ([]DSYMResult, bool, error) {
	type addr struct {
		Address string `json:"address"`
		Module  string `json:"module"`
	}
	req := struct {
		Symbol string `json:"symbol"`
		Frames []addr `json:"frames"`
	}{Symbol: key, Frames: make([]addr, len(addrs))}
	for i, a := range addrs {
		req.Frames[i] = addr{Address: "0x" + strconv.FormatUint(a.Address, 16), Module: a.Module}
	}
	body, _ := json.Marshal(req)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/symbolicate", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, sidecarError(resp)
	}
	var out struct {
		Frames []DSYMResult `json:"frames"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return out.Frames, true, nil
}

func (c *DSYMClient) put(ctx context.Context, key string, data []byte) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/symbols/"+key, bytes.NewReader(data))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/octet-stream")
	r.ContentLength = int64(len(data))
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return sidecarError(resp)
	}
	return nil
}

func sidecarError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("symbolicate sidecar: %s: %s", resp.Status, bytes.TrimSpace(body))
}
