// Proprietary and confidential. All rights reserved.

package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"cloister.io/internal/config"
	"cloister.io/internal/tunnel"
	"cloister.io/internal/vcsbroker"
	"cloister.io/internal/vm"
)

const vcsBrokerGuestPort = 49231

type vcsBrokerSession struct {
	backend vm.Backend
	profile string
	server  *vcsbroker.Server
}

func startVCSBroker(backend vm.Backend, profile string, p *config.Profile) (*vcsBrokerSession, error) {
	if !workspaceProvider(p).IsBroker() {
		return nil, nil
	}
	specs, err := brokerSessionSpecs(backend, profile, p)
	if err != nil {
		return nil, err
	}
	syncBroker, err := newWorkspaceBroker()
	if err != nil {
		return nil, err
	}
	guestHomeOutput, err := backend.SSHCommand(profile, `printf '%s' "$HOME"`)
	if err != nil {
		return nil, fmt.Errorf("resolving guest home for VCS broker: %w", err)
	}
	guestHome := strings.TrimSpace(guestHomeOutput)
	mapper, err := vcsbroker.NewMapper(guestHome, specs)
	if err != nil {
		return nil, err
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("creating VCS broker token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	server, err := vcsbroker.StartServer(vcsbroker.NewProxy(syncBroker, mapper, nil), token)
	if err != nil {
		return nil, err
	}
	session := &vcsBrokerSession{backend: backend, profile: profile, server: server}
	if err := tunnel.StartReverseForward(profile, "vcs-broker", server.Port(), vcsBrokerGuestPort, backend.SSHConfig(profile)); err != nil {
		_ = server.Close()
		return nil, fmt.Errorf("starting VCS broker tunnel: %w", err)
	}
	if err := vcsbroker.DeployGuest(backend, profile, vcsBrokerGuestPort, token); err != nil {
		tunnel.StopNamed(profile, "vcs-broker")
		_ = server.Close()
		return nil, err
	}
	return session, nil
}

func (s *vcsBrokerSession) Close() {
	if s == nil {
		return
	}
	vcsbroker.RemoveGuestConfig(s.backend, s.profile)
	tunnel.StopNamed(s.profile, "vcs-broker")
	_ = s.server.Close()
}
