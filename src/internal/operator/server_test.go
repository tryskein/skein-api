package operator_test

// server_test.go — sk-674.18, the test delivery of row 180 (T042, US-11,
// D-9), on the router operator.NewRouter builds — the one the operator's
// server serves. The references are the spec (US-11: the port serves no
// public API and does not answer without the key) and the contract
// accepted for T042 (operator/contract.go): the key in X-Skein-Operator-Key,
// 401 operator_unauthorized before routing on every path, the three
// operations at their paths. The other half of the row — the public router
// serves none of the operator's routes — needs the public router, which
// only cmd/api builds: cmd/api/operator_wiring_test.go holds it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"skein-api/internal/operator"
)

const testKey = "operator-key-aaaaaaaaaaaaaaaa"

// recorder is the operations, recording which one was reached with what.
type recorder struct {
	calls []string
}

func (r *recorder) Block(_ context.Context, email string) error {
	r.calls = append(r.calls, "block "+email)
	return nil
}

func (r *recorder) Unblock(_ context.Context, email string) error {
	r.calls = append(r.calls, "unblock "+email)
	return nil
}

func (r *recorder) Delete(_ context.Context, email string) error {
	r.calls = append(r.calls, "delete "+email)
	return nil
}

func send(h http.Handler, method, path, key string, body any) (int, string) {
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if key != "" {
		req.Header.Set(operator.KeyHeader, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Error
}

// TestOperatorRouter_KeyGuardsEveryPath — row 180. With the right key each
// operation's path reaches that operation and no other; without the key or
// with a wrong one — one character off, a prefix, another of the same
// length — every path answers 401 operator_unauthorized and no operation
// runs, an unknown path included; with the key an unknown path is 404.
func TestOperatorRouter_KeyGuardsEveryPath(t *testing.T) {
	body := map[string]string{"email": "someone@example.com"}

	t.Run("the right key reaches each operation", func(t *testing.T) {
		for _, c := range []struct{ path, want string }{
			{operator.PathBlock, "block someone@example.com"},
			{operator.PathUnblock, "unblock someone@example.com"},
			{operator.PathDelete, "delete someone@example.com"},
		} {
			ops := &recorder{}
			code, _ := send(operator.NewRouter(testKey, ops), http.MethodPost, c.path, testKey, body)
			if code != http.StatusNoContent {
				t.Errorf("POST %s with the key = %d, want 204", c.path, code)
			}
			if len(ops.calls) != 1 || ops.calls[0] != c.want {
				t.Errorf("POST %s reached %v, want exactly [%s]", c.path, ops.calls, c.want)
			}
		}
	})

	t.Run("no key or a wrong key is 401 and reaches nothing", func(t *testing.T) {
		wrong := map[string]string{
			"missing":            "",
			"one character off":  testKey[:len(testKey)-1] + "0",
			"a prefix":           testKey[:16],
			"longer":             testKey + "x",
			"same length, other": "xxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		}
		paths := []string{operator.PathBlock, operator.PathUnblock, operator.PathDelete, "/operator/v1/nowhere", "/"}
		for name, key := range wrong {
			for _, path := range paths {
				ops := &recorder{}
				code, errCode := send(operator.NewRouter(testKey, ops), http.MethodPost, path, key, body)
				if code != http.StatusUnauthorized || errCode != operator.CodeUnauthorized {
					t.Errorf("%s key, POST %s = %d %q, want 401 %s", name, path, code, errCode, operator.CodeUnauthorized)
				}
				if len(ops.calls) != 0 {
					t.Errorf("%s key, POST %s reached %v", name, path, ops.calls)
				}
			}
		}
	})

	t.Run("public routes are not served", func(t *testing.T) {
		ops := &recorder{}
		h := operator.NewRouter(testKey, ops)
		for _, c := range []struct{ method, path string }{
			{http.MethodGet, "/healthz"},
			{http.MethodGet, "/ready"},
			{http.MethodGet, "/metrics"},
			{http.MethodGet, "/api/v1/projects"},
			{http.MethodDelete, "/api/v1/me"},
			{http.MethodPost, "/api/v1/auth/signup"},
			{http.MethodPost, "/mcp"},
			{http.MethodGet, "/operator/v1/nowhere"},
		} {
			if code, _ := send(h, c.method, c.path, testKey, nil); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s on the operator's router with the key = %d, want not served (404)", c.method, c.path, code)
			}
			if code, errCode := send(h, c.method, c.path, "", nil); code != http.StatusUnauthorized || errCode != operator.CodeUnauthorized {
				t.Errorf("%s %s on the operator's router without the key = %d %q, want 401", c.method, c.path, code, errCode)
			}
		}
		if len(ops.calls) != 0 {
			t.Errorf("a public path reached %v", ops.calls)
		}
	})
}
