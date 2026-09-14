package vcsbroker

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const exitTrailer = "X-Cloister-Exit-Code"

const (
	// Host health retries distinguish a dead daemon from one transiently slow
	// loopback request. Three 500 ms attempts with 100 ms and 200 ms backoffs
	// add at most 1.8 seconds to an ensure operation.
	HostProbeAttempts = 3
	hostProbeTimeout  = 500 * time.Millisecond
	hostProbeBackoff  = 100 * time.Millisecond
)

// HostProbeStatus separates a live daemon transition from endpoint failure.
type HostProbeStatus uint8

const (
	HostProbeDead HostProbeStatus = iota
	HostProbeHealthy
	HostProbeDraining
)

// ActiveCommand identifies work that must finish before service teardown.
type ActiveCommand struct {
	Tool    string    `json:"tool"`
	Args    []string  `json:"args"`
	Project string    `json:"project"`
	Started time.Time `json:"started"`
}

// Server is a loopback-only host VCS command service.
type Server struct {
	listener net.Listener
	http     *http.Server

	mu       sync.Mutex
	draining bool
	nextID   uint64
	active   map[uint64]ActiveCommand
	idle     chan struct{}
	proxyMu  sync.RWMutex
	proxy    *Proxy
}

// StartServer starts an authenticated service on a random host loopback port.
func StartServer(proxy *Proxy, token string) (*Server, error) {
	if token == "" {
		return nil, fmt.Errorf("VCS broker token is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for VCS broker: %w", err)
	}
	server := &Server{listener: listener, active: make(map[uint64]ActiveCommand), proxy: proxy}
	mux := http.NewServeMux()
	expectedAuth := []byte("Bearer " + token)
	authenticated := func(r *http.Request) bool {
		return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), expectedAuth) == 1
	}
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !authenticated(r) {
			http.Error(w, "VCS broker authentication failed", http.StatusForbidden)
			return
		}
		if server.IsDraining() {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "VCS broker is restarting; retry", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !authenticated(r) {
			http.Error(w, "VCS broker authentication failed", http.StatusForbidden)
			return
		}
		commandID, accepted := server.beginCommand()
		if !accepted {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "VCS broker is restarting; retry command", http.StatusServiceUnavailable)
			return
		}
		defer server.finishCommand(commandID)

		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid VCS broker request", http.StatusBadRequest)
			return
		}
		request := Request{
			Tool: r.Form.Get("tool"),
			CWD:  r.Form.Get("cwd"),
			Args: append([]string(nil), r.Form["arg"]...),
			Env:  append([]string(nil), r.Form["env"]...),
		}
		server.describeCommand(commandID, request)
		w.Header().Add("Trailer", exitTrailer)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		exit, err := server.currentProxy().Execute(r.Context(), request, w)
		if err != nil {
			fmt.Fprintf(w, "cloister VCS broker: %v\n", err)
		}
		w.Header().Set(exitTrailer, strconv.Itoa(exit))
	})
	server.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.http.Serve(listener) }()
	return server, nil
}

// Port returns the host loopback port assigned to the server.
func (s *Server) Port() int {
	if s == nil || s.listener == nil {
		return 0
	}
	_, port, _ := net.SplitHostPort(s.listener.Addr().String())
	value, _ := strconv.Atoi(port)
	return value
}

// IsDraining reports whether the server has stopped admitting commands.
func (s *Server) IsDraining() bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

func (s *Server) beginCommand() (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || len(s.active) != 0 {
		return 0, false
	}
	s.nextID++
	id := s.nextID
	s.active[id] = ActiveCommand{Started: time.Now()}
	return id, true
}

func (s *Server) describeCommand(id uint64, request Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	command, ok := s.active[id]
	if !ok {
		return
	}
	command.Tool = request.Tool
	command.Args = append([]string(nil), request.Args...)
	command.Project = request.CWD
	s.active[id] = command
}

func (s *Server) finishCommand(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, id)
	if s.draining && len(s.active) == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

func (s *Server) startDrain() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	if len(s.active) == 0 {
		idle := make(chan struct{})
		close(idle)
		return idle
	}
	if s.idle == nil {
		s.idle = make(chan struct{})
	}
	return s.idle
}

// TryPauseIfIdle atomically stops admissions only when no command is active.
// A deferred config or build replacement therefore never creates a long
// admission pause behind an in-flight push.
func (s *Server) TryPauseIfIdle() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || len(s.active) != 0 {
		return false
	}
	s.draining = true
	return true
}

// SetProxy atomically publishes a mapper rebuilt from current profile config.
func (s *Server) SetProxy(proxy *Proxy) {
	s.proxyMu.Lock()
	s.proxy = proxy
	s.proxyMu.Unlock()
}

func (s *Server) currentProxy() *Proxy {
	s.proxyMu.RLock()
	defer s.proxyMu.RUnlock()
	return s.proxy
}

// Pause rejects new commands and waits for admitted work to finish without
// closing the listener. It is the first stage of graceful service shutdown.
func (s *Server) Pause(ctx context.Context) ([]ActiveCommand, error) {
	if s == nil || s.http == nil {
		return nil, nil
	}
	idle := s.startDrain()
	select {
	case <-idle:
		return nil, nil
	case <-ctx.Done():
		return s.activeCommands(), ctx.Err()
	}
}

// Resume admits commands again after an idle configuration reload.
func (s *Server) Resume() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.draining = false
	s.idle = nil
	s.mu.Unlock()
}

func (s *Server) activeCommands() []ActiveCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	commands := make([]ActiveCommand, 0, len(s.active))
	for _, command := range s.active {
		command.Args = append([]string(nil), command.Args...)
		commands = append(commands, command)
	}
	return commands
}

// Drain rejects new commands, waits for admitted handlers (including their
// post-command barriers), then gracefully closes the HTTP listener. The
// caller must keep the reverse tunnel alive until Drain returns.
func (s *Server) Drain(ctx context.Context) ([]ActiveCommand, error) {
	if s == nil || s.http == nil {
		return nil, nil
	}
	if commands, err := s.Pause(ctx); err != nil {
		return commands, err
	}
	if err := s.http.Shutdown(ctx); err != nil {
		return s.activeCommands(), err
	}
	return nil, nil
}

// Close immediately closes the listener and active client connections.
func (s *Server) Close() error {
	if s == nil || s.http == nil {
		return nil
	}
	return s.http.Close()
}

// ProbeHost verifies the authenticated daemon endpoint directly, independent
// of SSH and the reverse tunnel. Only all failed attempts count as unhealthy.
func ProbeHost(hostPort int, token string) HostProbeStatus {
	if hostPort <= 0 || hostPort > 65535 || token == "" || strings.ContainsAny(token, "\r\n") {
		return HostProbeDead
	}
	url := "http://127.0.0.1:" + strconv.Itoa(hostPort) + "/v1/health"
	for attempt := 0; attempt < HostProbeAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), hostProbeTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+token)
			response, requestErr := http.DefaultClient.Do(req)
			if requestErr == nil {
				_ = response.Body.Close()
				cancel()
				if response.StatusCode == http.StatusNoContent {
					return HostProbeHealthy
				}
				if response.StatusCode == http.StatusServiceUnavailable && response.Header.Get("Retry-After") != "" {
					return HostProbeDraining
				}
			} else {
				cancel()
			}
		} else {
			cancel()
		}
		if attempt+1 < HostProbeAttempts {
			time.Sleep(time.Duration(attempt+1) * hostProbeBackoff)
		}
	}
	return HostProbeDead
}
