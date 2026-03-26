package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ekovshilovsky/cloister/internal/config"
)

// OpenClawDefaults returns the default AgentConfig for an OpenClaw profile.
func OpenClawDefaults() *config.AgentConfig {
	return &config.AgentConfig{
		Type:  "openclaw",
		Image: "alpine/openclaw:latest",
		Ports: []int{18789},
	}
}

// OpenClawStacks returns the provisioning stacks required by OpenClaw.
func OpenClawStacks() []string {
	return []string{"web"}
}

// ComposeYAML generates a docker-compose.yml matching the official OpenClaw
// dual-service architecture: a gateway service that runs the WebSocket server
// and Control UI, and a CLI service that shares the gateway's network for
// interactive access. This matches the upstream docker-compose.yml to ensure
// correct CSP headers and Control UI rendering.
// Deprecated: the canonical implementation now lives in agent/docker.ComposeYAML.
// This wrapper is retained for backward compatibility with manager.go and will
// be removed when callers migrate to the Runtime interface in Task 10.
func ComposeYAML(profile string, cfg *config.AgentConfig, agentDataDir, workspaceDir string) string {
	envBlock := buildEnvBlock(cfg)

	return fmt.Sprintf(`services:
  openclaw-gateway:
    image: %s
    container_name: %s-gateway
    cap_drop:
      - ALL
    cap_add:
      - SYS_ADMIN
    user: "1000:1000"
    shm_size: "2g"
    environment:
      HOME: /home/node
      TERM: xterm-256color
      TZ: UTC
%s
    volumes:
      - %s:/home/node/.openclaw
      - %s:/home/node/.openclaw/workspace
      - %s/tmp:/tmp
      - %s/tmp/browser-cache:/home/node/.cache
    expose:
      - "18789"
    init: true
    restart: unless-stopped
    command:
      [
        "node",
        "dist/index.js",
        "gateway",
        "--bind",
        "lan",
        "--port",
        "18789",
      ]
    healthcheck:
      test:
        [
          "CMD",
          "node",
          "-e",
          "fetch('http://127.0.0.1:18789/healthz').then((r)=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))",
        ]
      interval: 30s
      timeout: 5s
      retries: 5
      start_period: 20s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "5"

  openclaw-cli:
    image: %s
    container_name: %s-cli
    network_mode: "service:openclaw-gateway"
    cap_drop:
      - NET_RAW
      - NET_ADMIN
    security_opt:
      - no-new-privileges:true
    user: "1000:1000"
    environment:
      HOME: /home/node
      TERM: xterm-256color
      BROWSER: echo
      TZ: UTC
%s
    volumes:
      - %s:/home/node/.openclaw
      - %s:/home/node/.openclaw/workspace
    stdin_open: true
    tty: true
    init: true
    entrypoint: ["node", "dist/index.js"]
    depends_on:
      - openclaw-gateway

  openclaw-proxy:
    image: caddy:2-alpine
    container_name: %s-proxy
    ports:
      - "127.0.0.1:18789:18789"
    depends_on:
      openclaw-gateway:
        condition: service_healthy
    command:
      - sh
      - -c
      - |
        cat > /etc/caddy/Caddyfile <<'CADDY'
        :18789 {
          reverse_proxy openclaw-gateway:18789
          header /* {
            -Content-Security-Policy
            Content-Security-Policy "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; img-src 'self' data: https:; font-src 'self' https://fonts.gstatic.com; connect-src 'self' ws: wss:; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"
          }
        }
        CADDY
        caddy run --config /etc/caddy/Caddyfile
    restart: unless-stopped
    logging:
      driver: json-file
      options:
        max-size: "5m"
        max-file: "3"
`,
		cfg.Image, profile,
		envBlock,
		agentDataDir, workspaceDir, agentDataDir, agentDataDir,
		cfg.Image, profile,
		envBlock,
		agentDataDir, workspaceDir,
		profile,
	)
}

// buildEnvBlock formats the user-supplied env vars as docker-compose environment entries.
func buildEnvBlock(cfg *config.AgentConfig) string {
	if len(cfg.Env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var lines []string
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("      %s: %q", k, cfg.Env[k]))
	}
	return strings.Join(lines, "\n")
}

// DockerRunArgs is kept for backward compatibility and non-compose agent types.
// For OpenClaw, use ComposeYAML instead.
// Deprecated: the canonical implementation now lives in agent/docker.DockerRunArgs.
// This wrapper is retained for backward compatibility with manager.go and will
// be removed when callers migrate to the Runtime interface in Task 10.
func DockerRunArgs(profile string, cfg *config.AgentConfig, agentDataDir, workspaceDir string) []string {
	args := []string{
		"run", "-d",
		"--name", profile,
		"--cap-drop", "ALL",
		"--cap-add", "SYS_ADMIN",
		"--user", "1000:1000",
		"--shm-size=2g",
	}

	args = append(args,
		"-v", fmt.Sprintf("%s/tmp:/tmp", agentDataDir),
		"-v", fmt.Sprintf("%s/tmp/browser-cache:/home/node/.cache", agentDataDir),
	)

	for _, port := range cfg.Ports {
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port))
	}

	args = append(args,
		"-v", fmt.Sprintf("%s:/home/node/.openclaw", agentDataDir),
		"-v", fmt.Sprintf("%s:/home/node/.openclaw/workspace", workspaceDir),
	)

	args = append(args,
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=5",
	)

	args = append(args, "-e", "OPENCLAW_GATEWAY_BIND=lan")

	if len(cfg.Env) > 0 {
		keys := make([]string, 0, len(cfg.Env))
		for k := range cfg.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", fmt.Sprintf("%s=%s", k, cfg.Env[k]))
		}
	}

	args = append(args, cfg.Image)
	return args
}
