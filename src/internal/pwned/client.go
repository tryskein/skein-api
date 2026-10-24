// Package pwned implements a client for the Have I Been Pwned "Pwned
// Passwords" k-anonymity range API, used by internal/users.ChangePassword
// to reject a new password that appears in a known breach corpus (NIST SP
// 800-63B rev. 4 SHALL — compare against a list of unacceptable passwords).
//
// Only the first 5 hex characters of the SHA-1 hash of the candidate
// password ever leave this process — the range endpoint returns every
// suffix sharing that prefix, and the match is decided locally against the
// full hash. Neither the password nor its full hash crosses the wire.
package pwned

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // required by the k-anonymity API contract; only a 5-char prefix of the digest ever leaves the process, and it is never used for storage/verification.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrServiceUnavailable is returned by Check when the range lookup could
// not be completed — network failure, timeout, or a non-200 response.
// Callers MUST treat a non-nil error as "unknown", never as "clean": the
// caller (internal/users.ChangePassword) logs it and lets the password
// change proceed unblocked rather than locking users out of their own
// accounts because an external service is unreachable.
var ErrServiceUnavailable = errors.New("pwned passwords service unavailable")

// Client is a small HTTP client for the Pwned Passwords range endpoint.
// Build once at startup (or leave the owning users.PwnedChecker interface
// value nil to disable the feature entirely — see internal/users.Store).
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient returns a Client bound to baseURL (e.g.
// "https://api.pwnedpasswords.com" or a self-hosted mirror for a
// closed-network deployment) with a hard per-request timeout.
func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: timeout},
	}
}

// Check reports whether password's SHA-1 hash appears in the breach
// corpus. It sends only the first 5 hex characters of the hash (the
// k-anonymity prefix) to baseURL + "/range/{prefix}", then compares every
// returned suffix against the remaining 35 characters locally.
//
// A non-nil error means the lookup could not be completed — the caller
// must treat that as "unknown", not as "clean" or "breached".
func (c *Client) Check(ctx context.Context, password string) (bool, error) {
	sum := sha1.Sum([]byte(password)) //nolint:gosec // see package doc — k-anonymity contract, not used for storage/verification.
	hexSum := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := hexSum[:5], hexSum[5:]

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/range/"+prefix, nil)
	if err != nil {
		return false, fmt.Errorf("%w: building request: %v", ErrServiceUnavailable, err)
	}
	// Add-Padding asks the service to pad the response with decoy rows so
	// response size can't be used to infer how many suffixes matched.
	req.Header.Set("Add-Padding", "true")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%w: status %d", ErrServiceUnavailable, resp.StatusCode)
	}

	// 2 MiB safety cap — a real response (even padded to ~1000 rows of
	// "SUFFIX:COUNT\r\n") is a few tens of KB; this only guards against a
	// misbehaving or malicious baseURL streaming an unbounded body.
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 2<<20))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		if line[:idx] == suffix {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("%w: reading response: %v", ErrServiceUnavailable, err)
	}
	return false, nil
}
