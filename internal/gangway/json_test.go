package gangway_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/skarm/gangway/internal/gangway"
)

// TestUpstreamJSONValidation pins down which Docker responses are accepted.
// Anything the proxy cannot vouch for becomes 502 rather than a partial relay.
func TestUpstreamJSONValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		container bool
		want      int
	}{
		{name: "null image", body: `null`, want: 502},
		{name: "empty image", body: `{}`, want: 502},
		{name: "empty image id", body: `{"Id":""}`, want: 502},
		{name: "null image id", body: `{"Id":null}`, want: 502},
		{name: "wrong image id type", body: `{"Id":123}`, want: 502},
		{name: "wrong digests type", body: `{"Id":"sha256:abc","RepoDigests":"digest"}`, want: 502},
		{name: "null digests", body: `{"Id":"sha256:abc","RepoDigests":null}`, want: 200},
		{name: "missing digests", body: `{"Id":"sha256:abc"}`, want: 200},
		{name: "null container", body: `null`, container: true, want: 502},
		{name: "empty container", body: `{}`, container: true, want: 502},
		{name: "null config", body: `{"Config":null}`, container: true, want: 502},
		{name: "labels without image", body: `{"Config":{"Labels":{"app":"api"}}}`, container: true, want: 200},
		{name: "null labels", body: `{"Config":{"Image":"app","Labels":null}}`, container: true, want: 200},
		{name: "wrong labels type", body: `{"Config":{"Image":"app","Labels":{"a":123}}}`, container: true, want: 502},
		{name: "malformed", body: `{"Id":`, want: 502},
		{name: "empty", body: ``, want: 502},
		{name: "array", body: `[]`, want: 502},
		{name: "trailing value", body: `{"Id":"sha256:abc"} {}`, want: 502},
		{name: "trailing null", body: `{"Id":"sha256:abc"} null`, want: 502},
		{name: "trailing garbage", body: `{"Id":"sha256:abc"} broken`, want: 502},
		{name: "trailing object close", body: `{"Id":"sha256:abc"}}`, want: 502},
		{name: "trailing array close", body: `{"Id":"sha256:abc"}]`, want: 502},
		{name: "trailing whitespace", body: "{\"Id\":\"sha256:abc\"}\n\t ", want: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			path := "/v1.55/images/app/json"
			if tc.container {
				path = "/v1.55/containers/" + testContainerID + "/json"
			}
			rr := httptest.NewRecorder()
			newHandler(t, socket).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))

			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d, body = %s", rr.Code, tc.want, rr.Body.String())
			}
			if rr.Header().Get("Content-Type") != "application/json" || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("response headers = %#v", rr.Header())
			}
		})
	}
}

// TestResponseSizeLimitIsExact checks the boundary: a body of exactly the
// configured size is accepted, one byte more is not, whatever that byte is.
func TestResponseSizeLimitIsExact(t *testing.T) {
	const body = `{"Id":"sha256:abc"}`
	for _, extra := range []string{"", " ", "\n", strings.Repeat(" ", 4096)} {
		t.Run(fmt.Sprintf("extra-%d", len(extra)), func(t *testing.T) {
			socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body+extra)
			}))
			h := newHandlerWithConfig(t, gangway.Config{DockerSocket: socket, MaxResponseBytes: int64(len(body))})
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/images/app/json", nil))

			want := http.StatusOK
			if extra != "" {
				want = http.StatusBadGateway
			}
			if rr.Code != want {
				t.Fatalf("status = %d, want %d", rr.Code, want)
			}
		})
	}
}

func TestOversizedResponseIsRejected(t *testing.T) {
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Config":{"Image":"`+strings.Repeat("a", 1024)+`","Labels":{}}}`)
	}))
	h := newHandlerWithConfig(t, gangway.Config{DockerSocket: socket, MaxResponseBytes: 128})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Invalid JSON past the limit reports the limit, not a syntax error, so the
	// cause reaching the log is the one that actually stopped the read.
	var dst any
	if err := gangway.DecodeJSON(strings.NewReader(strings.Repeat("x", 32)), 8, &dst); !errors.Is(err, gangway.ErrResponseTooLarge) {
		t.Errorf("oversized invalid JSON: %v", err)
	}
}

func TestPathForLog(t *testing.T) {
	short := "/v1.55/images/app/json"
	if got := gangway.PathForLog(short); got != short {
		t.Fatalf("short path = %q", got)
	}
	for _, path := range []string{
		strings.Repeat("a", 1024),
		strings.Repeat("é", 512),
		strings.Repeat("a", 255) + strings.Repeat("😀", 16),
	} {
		got := gangway.PathForLog(path)
		if len(got) > gangway.MaxLoggedPathBytes+len("...") {
			t.Errorf("truncated length = %d", len(got))
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("long path was not marked as truncated: %q", got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncation split a UTF-8 sequence: %q", got)
		}
		if !strings.HasPrefix(path, strings.TrimSuffix(got, "...")) {
			t.Errorf("truncated path is not a prefix of the original: %q", got)
		}
	}
}
