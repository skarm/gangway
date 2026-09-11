package gangway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"unicode/utf8"
)

// maxLoggedPathBytes bounds how much of a rejected path reaches the log.
const maxLoggedPathBytes = 256

// maxPooledBuffer caps the scratch space a request may hand back, so a single
// large Docker response cannot pin MaxResponseBytes for the life of the pool.
const maxPooledBuffer = 64 << 10

// buffers holds the scratch space a request needs to read Docker's response and
// to build its own. A pooled buffer settles at the size the workload actually
// uses; a per-request decoder starts small and grows into garbage every time.
var buffers sync.Pool

func getBuffer() *bytes.Buffer {
	buf, ok := buffers.Get().(*bytes.Buffer)
	if !ok {
		return new(bytes.Buffer)
	}

	buf.Reset()

	return buf
}

func putBuffer(buf *bytes.Buffer) {
	if buf.Cap() <= maxPooledBuffer {
		buffers.Put(buf)
	}
}

// ErrResponseTooLarge reports that Docker sent more than the configured limit.
var ErrResponseTooLarge = errors.New("docker response exceeds configured limit")

// decodeJSON decodes exactly one JSON value from at most maxBytes of r. Reading
// past the limit is an error rather than a truncation, so an oversized response
// can never be accepted as a shorter valid one.
func decodeJSON(r io.Reader, maxBytes int64, dst any) error {
	buf := getBuffer()
	defer putBuffer(buf)

	// One byte past the limit separates a response at the limit from one over
	// it without reading any more of an oversized body than that.
	read, err := buf.ReadFrom(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return err
	}

	if read > maxBytes {
		return ErrResponseTooLarge
	}

	// Unmarshal rejects trailing content itself, so a second value in the
	// response fails here rather than being silently ignored.
	return json.Unmarshal(buf.Bytes(), dst)
}

// errorMessage is the only body shape the proxy emits for a failure, and the
// only field it relays from a Docker failure.
type errorMessage struct {
	Message string `json:"message"`
}

// The proxy's own failure messages are fixed strings, so they are encoded once
// at start-up: rejection is the one path whose rate an untrusted client sets.
var (
	bodyNotAllowed   = errorBody("request is not allowed")
	bodyBusy         = errorBody("proxy is busy")
	bodyRateLimited  = errorBody("docker API request rate exceeded")
	bodyTimedOut     = errorBody("docker daemon request timed out")
	bodyUnavailable  = errorBody("docker daemon is unavailable")
	bodyInvalid      = errorBody("invalid response from docker daemon")
	bodyUpstreamFail = errorBody("docker API request failed")
)

func errorBody(message string) []byte {
	var buf bytes.Buffer
	// A fixed string that cannot be encoded is a programming error, and one
	// that must not be discovered by serving a half-written response.
	if err := encodeJSON(&buf, errorMessage{Message: message}); err != nil {
		panic(err)
	}

	return buf.Bytes()
}

func encodeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	return encoder.Encode(value)
}

// writeError sends one of the pre-encoded failure bodies above.
func writeError(w http.ResponseWriter, status int, body []byte) {
	writeBody(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	buf := getBuffer()
	defer putBuffer(buf)

	if err := encodeJSON(buf, value); err != nil {
		// Nothing has reached the client yet, so the status can still report
		// that the response could not be produced.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	writeBody(w, status, buf.Bytes())
}

// writeBody sends an already-encoded body. Declaring its length keeps the
// response out of chunked framing, which net/http falls back to for anything
// past its own write buffer: a container carrying the usual Kubernetes
// annotations clears that in labels alone.
func writeBody(w http.ResponseWriter, status int, body []byte) {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// pathForLog bounds an untrusted path without splitting a UTF-8 sequence.
func pathForLog(path string) string {
	if len(path) <= maxLoggedPathBytes {
		return path
	}

	cut := maxLoggedPathBytes
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}

	return path[:cut] + "..."
}
