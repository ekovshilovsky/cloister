// Proprietary and confidential. All rights reserved.

package vcsbroker

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const exitTrailer = "X-Cloister-Exit-Code"

// Server is a loopback-only host VCS command service.
type Server struct {
	listener net.Listener
	http     *http.Server
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
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "VCS broker authentication failed", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid VCS broker request", http.StatusBadRequest)
			return
		}
		w.Header().Add("Trailer", exitTrailer)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		exit, err := proxy.Execute(r.Context(), Request{
			Tool: r.Form.Get("tool"),
			CWD:  r.Form.Get("cwd"),
			Args: append([]string(nil), r.Form["arg"]...),
			Env:  append([]string(nil), r.Form["env"]...),
		}, w)
		if err != nil {
			fmt.Fprintf(w, "cloister VCS broker: %v\n", err)
		}
		w.Header().Set(exitTrailer, strconv.Itoa(exit))
	})
	server := &Server{listener: listener, http: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}}
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

// Close shuts down the service.
func (s *Server) Close() error {
	if s == nil || s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.http.Shutdown(ctx)
	if err != nil && !strings.Contains(err.Error(), "Server closed") {
		return err
	}
	return nil
}
