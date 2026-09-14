package vcsbroker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
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

// ServerStatus describes admission state and commands already accepted by a
// broker. It is exposed only through the authenticated host-loopback API.
type ServerStatus struct {
	Draining bool            `json:"draining"`
	Commands []ActiveCommand `json:"commands"`
}

// Server is a loopback-only host VCS command service.
type Server struct {
	listener net.Listener
	http     *http.Server

	mu         sync.Mutex
	draining   bool
	nextID     uint64
	active     map[uint64]ActiveCommand
	delivering int
	idle       chan struct{}
	proxyMu    sync.RWMutex
	proxy      *Proxy
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
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !authenticated(r) {
			http.Error(w, "VCS broker authentication failed", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(server.Status())
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
		defer server.finishDelivery()
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		if err := r.ParseForm(); err != nil {
			server.finishCommand(commandID)
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
		spool, err := newResponseSpool()
		if err != nil {
			server.finishCommand(commandID)
			http.Error(w, "creating VCS broker response spool", http.StatusInternalServerError)
			return
		}
		defer spool.close()
		w.Header().Add("Trailer", exitTrailer)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		type executionResult struct {
			exit int
		}
		completed := make(chan executionResult, 1)
		go func() {
			// Once accepted, command execution is independent of the client
			// connection. This preserves repository/barrier consistency when a
			// guest curl is killed or its tunnel disappears.
			exit, executeErr := server.currentProxy().Execute(context.WithoutCancel(r.Context()), request, spool)
			if executeErr != nil {
				_, _ = fmt.Fprintf(spool, "cloister VCS broker: %v\n", executeErr)
			}
			spool.finish(nil)
			server.finishCommand(commandID)
			completed <- executionResult{exit: exit}
		}()
		streamErr := spool.stream(w)
		result := <-completed
		if streamErr == nil {
			w.Header().Set(exitTrailer, strconv.Itoa(result.exit))
		}
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
	if s.draining {
		return 0, false
	}
	s.nextID++
	id := s.nextID
	s.active[id] = ActiveCommand{Started: time.Now()}
	s.delivering++
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
	if s.draining && len(s.active) == 0 && s.delivering == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

func (s *Server) finishDelivery() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.delivering > 0 {
		s.delivering--
	}
	if s.draining && len(s.active) == 0 && s.delivering == 0 && s.idle != nil {
		close(s.idle)
		s.idle = nil
	}
}

func (s *Server) startDrain() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.draining = true
	if len(s.active) == 0 && s.delivering == 0 {
		idle := make(chan struct{})
		close(idle)
		return idle
	}
	if s.idle == nil {
		s.idle = make(chan struct{})
	}
	return s.idle
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

// Status returns a snapshot suitable for lifecycle progress reporting.
func (s *Server) Status() ServerStatus {
	if s == nil {
		return ServerStatus{Draining: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	commands := make([]ActiveCommand, 0, len(s.active))
	for _, command := range s.active {
		command.Args = append([]string(nil), command.Args...)
		commands = append(commands, command)
	}
	return ServerStatus{Draining: s.draining, Commands: commands}
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

// ReadHostStatus fetches one authenticated status snapshot from the host
// listener. Lifecycle progress reporting deliberately does not traverse SSH.
func ReadHostStatus(hostPort int, token string) (ServerStatus, error) {
	if hostPort <= 0 || hostPort > 65535 || token == "" || strings.ContainsAny(token, "\r\n") {
		return ServerStatus{}, fmt.Errorf("invalid VCS broker host endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(hostPort)+"/v1/status", nil)
	if err != nil {
		return ServerStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return ServerStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ServerStatus{}, fmt.Errorf("VCS broker status returned HTTP %d", response.StatusCode)
	}
	var status ServerStatus
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		return ServerStatus{}, err
	}
	return status, nil
}
