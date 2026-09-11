package gangway_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skarm/gangway/internal/gangway"
)

func TestContainerInspectKeepsOnlyImageAndLabels(t *testing.T) {
	dockerSocket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1.55/containers/"+testContainerID+"/json"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"Id":"`+testContainerID+`",
			"Config":{
				"Image":"registry.example.com/team/app:v1",
				"Labels":{"service":"api","env":"prod"},
				"Env":["DB_PASSWORD=secret","TOKEN=secret"]
			},
			"HostConfig":{"Binds":["/:/host"]},
			"Mounts":[{"Source":"/secret","Destination":"/data"}],
			"NetworkSettings":{"IPAddress":"10.0.0.2"}
		}`)
	}))

	rr := httptest.NewRecorder()
	newHandler(t, dockerSocket).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("top-level fields = %#v", got)
	}
	cfg, ok := got["Config"].(map[string]any)
	if !ok {
		t.Fatalf("Config = %#v", got["Config"])
	}
	if len(cfg) != 2 {
		t.Fatalf("Config fields = %#v", cfg)
	}
	if _, ok := cfg["Env"]; ok {
		t.Fatal("Env leaked through proxy")
	}
	if cfg["Image"] != "registry.example.com/team/app:v1" {
		t.Fatalf("Image = %#v", cfg["Image"])
	}
	if want := map[string]any{"service": "api", "env": "prod"}; !reflect.DeepEqual(cfg["Labels"], want) {
		t.Fatalf("Labels = %#v, want %#v", cfg["Labels"], want)
	}
}

// TestLabelPrefixesRestrictWhatLeavesTheProxy covers the data-minimisation
// option: labels are relayed for selectors, but a container's labels routinely
// carry whatever the orchestrator put there, and an allowlist is the only thing
// that keeps a label added elsewhere from reaching this proxy's clients.
func TestLabelPrefixesRestrictWhatLeavesTheProxy(t *testing.T) {
	const body = `{"Config":{"Image":"app:v1","Labels":{
		"spiffe.io/team":"payments",
		"com.example/tier":"web",
		"secret.internal/token":"must-not-be-relayed",
		"org.opencontainers.image.source":"https://example.invalid"
	}},"Env":["SECRET=hidden"]}`

	for name, tc := range map[string]struct {
		prefixes []string
		want     map[string]string
	}{
		"no allowlist relays every label": {
			prefixes: nil,
			want: map[string]string{
				"spiffe.io/team":                  "payments",
				"com.example/tier":                "web",
				"secret.internal/token":           "must-not-be-relayed",
				"org.opencontainers.image.source": "https://example.invalid",
			},
		},
		"one prefix": {
			prefixes: []string{"spiffe.io/"},
			want:     map[string]string{"spiffe.io/team": "payments"},
		},
		"several prefixes": {
			prefixes: []string{"spiffe.io/", "com.example/"},
			want: map[string]string{
				"spiffe.io/team":   "payments",
				"com.example/tier": "web",
			},
		},
		"nothing matches": {
			prefixes: []string{"absent."},
			want:     map[string]string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			h := newHandlerWithConfig(t, gangway.Config{
				DockerSocket:    socket,
				UpstreamTimeout: time.Second,
				LabelPrefixes:   tc.prefixes,
			})

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/containers/"+testContainerID+"/json", nil))

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}

			var got struct {
				Config struct {
					Image  string            `json:"Image"`
					Labels map[string]string `json:"Labels"`
				} `json:"Config"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}

			if got.Config.Image != "app:v1" {
				t.Errorf("image = %q, want app:v1", got.Config.Image)
			}
			if !reflect.DeepEqual(got.Config.Labels, tc.want) {
				t.Errorf("labels = %#v, want %#v", got.Config.Labels, tc.want)
			}
			if strings.Contains(rr.Body.String(), "hidden") {
				t.Errorf("environment reached the client: %s", rr.Body.String())
			}
		})
	}
}

func TestImageInspectKeepsOnlyIDAndDigests(t *testing.T) {
	dockerSocket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"Id":"sha256:012345",
			"RepoDigests":["registry.example.com/team/app@sha256:abcdef"],
			"Config":{"Env":["TOKEN=secret"],"Labels":{"secret":"value"}},
			"RootFS":{"Layers":["sha256:sensitive"]},
			"Metadata":{"LastTagTime":"2026-01-01T00:00:00Z"}
		}`)
	}))

	rr := httptest.NewRecorder()
	newHandler(t, dockerSocket).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1.55/images/registry.example.com/team/app:v1/json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("fields = %#v", got)
	}
	if got["Id"] != "sha256:012345" {
		t.Fatalf("Id = %#v", got["Id"])
	}
	if want := []any{"registry.example.com/team/app@sha256:abcdef"}; !reflect.DeepEqual(got["RepoDigests"], want) {
		t.Fatalf("RepoDigests = %#v, want %#v", got["RepoDigests"], want)
	}
	if _, ok := got["Config"]; ok {
		t.Fatal("image Config leaked through proxy")
	}
}

// TestPingIsolatesClientAndDocker checks both directions at once: no client
// header reaches Docker, and no Docker header beyond the negotiation set
// reaches the client.
func TestPingIsolatesClientAndDocker(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			dockerSocket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method || r.Host != "docker" || r.URL.RequestURI() != "/_ping" {
					t.Errorf("upstream request: %s %s host = %q", r.Method, r.URL.RequestURI(), r.Host)
				}
				for _, header := range []string{
					"Authorization", "Cookie", "X-Registry-Auth", "X-Forwarded-For",
					"Connection", "X-Client-Secret", "Accept-Encoding",
				} {
					if got := r.Header.Get(header); got != "" {
						t.Errorf("client header %s reached Docker: %q", header, got)
					}
				}
				if got := r.Header.Get("User-Agent"); got != "gangway" {
					t.Errorf("User-Agent = %q", got)
				}
				w.Header().Set("Api-Version", "1.55")
				w.Header().Set("Ostype", "linux")
				w.Header().Set("Docker-Experimental", "false")
				w.Header().Set("Builder-Version", "2")
				w.Header().Set("Swarm", "inactive")
				w.Header().Set("Server", "sensitive-server-banner")
				w.Header().Set("Set-Cookie", "secret=value")
				w.Header().Set("X-Docker-Secret", "value")
				_, _ = io.WriteString(w, "UNTRUSTED BODY")
			}))

			req := httptest.NewRequest(method, "/_ping", nil)
			req.Host = "client-controlled.example"
			for _, header := range []string{
				"Authorization", "Cookie", "X-Registry-Auth", "X-Forwarded-For",
				"Connection", "X-Client-Secret", "Accept-Encoding", "User-Agent",
			} {
				req.Header.Set(header, "client-secret")
			}
			rr := httptest.NewRecorder()
			newHandler(t, dockerSocket).ServeHTTP(rr, req)

			wantBody := "OK"
			if method == http.MethodHead {
				wantBody = ""
			}
			if rr.Code != http.StatusOK || rr.Body.String() != wantBody {
				t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
			}
			for header, want := range map[string]string{
				"Api-Version": "1.55", "Ostype": "linux", "Docker-Experimental": "false",
				"Builder-Version": "2", "Swarm": "inactive", "X-Content-Type-Options": "nosniff",
				"Server": "", "Set-Cookie": "", "X-Docker-Secret": "",
			} {
				if got := rr.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

func TestConcurrencyLimitAndRelease(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	socket := startDocker(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "OK")
	}))
	h := newHandlerWithConfig(t, gangway.Config{DockerSocket: socket, MaxConcurrent: 1})

	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	}()
	awaitSignal(t, started, "first upstream request")

	busy := httptest.NewRecorder()
	h.ServeHTTP(busy, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	if busy.Code != http.StatusServiceUnavailable {
		t.Errorf("saturated status = %d, want 503", busy.Code)
	}
	// Denials are decided before a slot is needed, so they still work while busy.
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, httptest.NewRequest(http.MethodPost, "/containers/create", nil))
	if denied.Code != http.StatusForbidden {
		t.Errorf("denied request while busy: status = %d", denied.Code)
	}

	close(release)
	awaitSignal(t, done, "first response")
	if first.Code != http.StatusOK {
		t.Errorf("first status = %d", first.Code)
	}

	next := httptest.NewRecorder()
	h.ServeHTTP(next, httptest.NewRequest(http.MethodGet, "/_ping", nil))
	if next.Code != http.StatusOK || calls.Load() != 2 || h.InFlight() != 0 {
		t.Errorf("slot not released: status = %d calls = %d occupied = %d", next.Code, calls.Load(), h.InFlight())
	}
}

func TestClientCancellationReleasesUpstreamAndSlot(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	socket := startDocker(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	h := newHandlerWithConfig(t, gangway.Config{DockerSocket: socket, MaxConcurrent: 1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	rr := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/_ping", nil).WithContext(ctx))
	}()

	awaitSignal(t, started, "upstream request")
	cancel()
	awaitSignal(t, canceled, "upstream cancellation")
	awaitSignal(t, done, "handler return")
	if h.InFlight() != 0 || rr.Body.Len() != 0 {
		t.Errorf("canceled request: occupied slots = %d body = %q", h.InFlight(), rr.Body.String())
	}
}

func TestDeniedPathIsTruncatedInLogs(t *testing.T) {
	var logged strings.Builder
	h := newHandlerWithConfig(t, gangway.Config{
		DockerSocket: "/unused.sock",
		Logger:       jsonLogger(&logged),
	})
	long := "/" + strings.Repeat("a", 4096)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, long, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(logged.String()) > 1024 {
		t.Fatalf("denied path was logged in full: %d bytes", len(logged.String()))
	}
}

// TestDeniedRequestsAreDebugRecords pins that a rejection is invisible at the
// default level. A rejection is the one record whose rate an untrusted client
// controls, so it must not reach the log unless an operator asked for it.
func TestDeniedRequestsAreDebugRecords(t *testing.T) {
	for _, tc := range []struct {
		name  string
		level slog.Level
		want  bool
	}{
		{name: "info", level: slog.LevelInfo, want: false},
		{name: "debug", level: slog.LevelDebug, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged strings.Builder
			h := newHandlerWithConfig(t, gangway.Config{
				DockerSocket: "/unused.sock",
				Logger:       slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: tc.level})),
			})

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/containers/create", nil))

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d", rr.Code)
			}
			if got := strings.Contains(logged.String(), "denied docker API request"); got != tc.want {
				t.Fatalf("rejection logged = %v at %s, want %v: %q", got, tc.level, tc.want, logged.String())
			}
		})
	}
}
