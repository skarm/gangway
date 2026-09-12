package gangway_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
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
	if err := gangway.DecodeJSON(strings.NewReader(strings.Repeat("x", 32)), 8, 32, &dst); !errors.Is(err, gangway.ErrResponseTooLarge) {
		t.Errorf("oversized invalid JSON: %v", err)
	}
}

// TestDecodedValuesOutliveTheBufferTheyCameFrom protects the one assumption
// that makes the buffer pool safe: nothing a decode produces may still point
// into the buffer it was decoded from, because that buffer goes back to the
// pool the moment the decode returns and the next request writes over it.
//
// It holds today because encoding/json copies strings, map keys and byte slices
// out of its input. It is a property of the field types in the destination
// rather than of the decoder, so a field added later — anything that keeps a
// reference to what it was given — would break it silently, and every value the
// proxy relays would be whatever the following request happened to read.
func TestDecodedValuesOutliveTheBufferTheyCameFrom(t *testing.T) {
	const decodes = 64

	type config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	}
	type inspect struct {
		Config config `json:"Config"`
	}

	kept := make([]inspect, decodes)

	for i := range kept {
		// Every body is the same length and differs in every byte that matters,
		// so a reused buffer overwrites exactly where the last value's bytes
		// were and corruption cannot hide behind identical content.
		body := fmt.Sprintf(`{"Config":{"Image":"image-%03d","Labels":{"label-%03d":"%s"}}}`,
			i, i, strings.Repeat(string(rune('a'+i%26)), 32))

		if err := gangway.DecodeJSON(strings.NewReader(body), 1<<20, int64(len(body)), &kept[i]); err != nil {
			t.Fatal(err)
		}
	}

	for i, got := range kept {
		wantImage := fmt.Sprintf("image-%03d", i)
		wantValue := strings.Repeat(string(rune('a'+i%26)), 32)

		if got.Config.Image != wantImage {
			t.Fatalf("decode %d holds image %q, want %q: a decoded value points into the pool",
				i, got.Config.Image, wantImage)
		}
		if value := got.Config.Labels[fmt.Sprintf("label-%03d", i)]; value != wantValue {
			t.Fatalf("decode %d holds label %q, want %q: a decoded value points into the pool",
				i, value, wantValue)
		}
	}
}

// TestInvalidUTF8IsReplacedAndBounded pins what the size limit does and does not
// promise. It bounds the bytes read from Docker, not the bytes written to the
// client: the decoder turns every byte of invalid UTF-8 into a replacement
// character three bytes long, so a reply can be three times the response it came
// from. Three times, and no more, is what the memory the process is given rests
// on — and whatever the daemon sent, what leaves the proxy is valid UTF-8.
//
// A real daemon cannot produce this, because its own encoder replaces invalid
// UTF-8 before the bytes ever reach the socket. Something else on that socket
// can.
func TestInvalidUTF8IsReplacedAndBounded(t *testing.T) {
	const invalid = "\x80"

	body := `{"Config":{"Image":"app","Labels":{"a":"` + strings.Repeat(invalid, 4096) + `"}}}`
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))

	rr := httptest.NewRecorder()
	newHandler(t, socket).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !utf8.Valid(rr.Body.Bytes()) {
		t.Error("the proxy relayed bytes that are not valid UTF-8")
	}
	if !strings.Contains(rr.Body.String(), "\uFFFD") {
		t.Error("invalid UTF-8 was neither rejected nor replaced")
	}
	if got, limit := rr.Body.Len(), 3*len(body); got > limit {
		t.Errorf("a %d byte response produced a %d byte reply, over the %d bytes three times its size allows",
			len(body), got, limit)
	}
}

// TestDeclaredLengthOnlySizesTheBuffer covers the one thing the response's
// declared length is used for. It comes from the daemon, so a length that lies
// about being enormous must cost no more than a response at the limit: sizing
// the buffer from it unclamped would let anything on that socket ask the proxy
// for a gigabyte per request.
func TestDeclaredLengthOnlySizesTheBuffer(t *testing.T) {
	const body = `{"Id":"sha256:abc"}`
	const lie = 1 << 30

	var dst map[string]any

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	if err := gangway.DecodeJSON(strings.NewReader(body), 1<<20, lie, &dst); err != nil {
		t.Fatal(err)
	}

	runtime.ReadMemStats(&after)

	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Errorf("a declared length of %d bytes allocated %d bytes for a %d byte body", lie, allocated, len(body))
	}

	// A response that declares no length at all still decodes; Docker frames
	// some replies in chunks, where there is nothing to read the size from.
	dst = nil
	if err := gangway.DecodeJSON(strings.NewReader(body), 1<<20, -1, &dst); err != nil {
		t.Fatalf("undeclared length: %v", err)
	}
	if dst["Id"] != "sha256:abc" {
		t.Errorf("decoded %v", dst)
	}
}

// TestAnUnencodableReplyIsStillAJSONFailure covers the one answer the proxy
// produces that is not built from a fixed string. Every failure a client can
// see carries the same body shape, and a reply that could not be encoded is the
// one place that could quietly stop being true.
func TestAnUnencodableReplyIsStillAJSONFailure(t *testing.T) {
	rr := httptest.NewRecorder()
	// A channel has no JSON form, which is what a field type that cannot be
	// encoded would look like if one were ever added to a response struct.
	gangway.WriteJSON(rr, http.StatusOK, make(chan int))

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(rr.Body.Len()) {
		t.Errorf("content length = %q for a %d byte body", got, rr.Body.Len())
	}

	var failure struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &failure); err != nil || failure.Message == "" {
		t.Errorf("body = %q, error = %v", rr.Body.String(), err)
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
