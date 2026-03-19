#!/bin/bash
# Fake cloister CLI for demo recording — produces colorful output without real VMs

GREEN='\033[1;32m'
CYAN='\033[1;36m'
YELLOW='\033[1;33m'
RED='\033[1;31m'
BOLD='\033[1m'
DIM='\033[90m'
PURPLE='\033[1;35m'
RESET='\033[0m'

case "$1" in
  create)
    profile="$2"
    stack=""
    for arg in "$@"; do
      [[ "$prev" == "--stack" ]] && stack="$arg"
      prev="$arg"
    done
    echo -e "Profile ${GREEN}\"${profile}\"${RESET} created."
    echo "Starting \"${profile}\"..."
    echo "Provisioning environment..."
    echo -e "  ${GREEN}✓${RESET} Base tools installed"
    if [[ -n "$stack" ]]; then
      IFS=',' read -ra stacks <<< "$stack"
      for s in "${stacks[@]}"; do
        echo -e "  ${GREEN}✓${RESET} ${s} stack installed"
      done
    fi
    echo ""
    echo -e "Profile \"${profile}\" ready. Enter with: ${CYAN}cloister ${profile}${RESET}"
    ;;

  status)
    echo -e "${BOLD}PROFILE    STATE     MEMORY  IDLE    STACKS${RESET}"
    echo -e "${GREEN}work${RESET}        running   4GB     2m      web,ollama"
    echo -e "${CYAN}personal${RESET}    running   4GB     5m      web,cloud"
    echo ""
    echo -e "Budget: ${YELLOW}8GB / 22GB${RESET} used"
    echo -e "Tunnels: ${GREEN}✓${RESET} clipboard  ${GREEN}✓${RESET} op-forward  ${GREEN}✓${RESET} ollama"
    ;;

  work|personal)
    profile="$1"
    echo "Starting \"${profile}\"..."
    echo "Tunnels:"
    echo -e "  ${GREEN}✓${RESET} clipboard (port 18339)"
    echo -e "  ${GREEN}✓${RESET} op-forward (port 18340)"
    echo -e "  ${GREEN}✓${RESET} ollama (port 11434)"
    echo -e "  ${RED}✗${RESET} audio (port 4713) — install: brew install pulseaudio"
    echo ""
    echo -e "Entering ${CYAN}${profile}${RESET}..."

    # Simulate iTerm2 background color change — only background, no text color changes
    if [[ "$profile" == "work" ]]; then
      printf '\033]Ph051a05\033\\'
    elif [[ "$profile" == "personal" ]]; then
      printf '\033]Ph0a1628\033\\'
    fi
    ;;

  stop)
    echo -e "Stopping ${YELLOW}\"work\"${RESET}... ${GREEN}Stopped${RESET}"
    echo -e "Stopping ${YELLOW}\"personal\"${RESET}... ${GREEN}Stopped${RESET}"
    ;;

  exec)
    profile="$2"
    shift 2
    cmd="$*"
    case "$cmd" in
      "claude --version")
        echo "2.1.79 (Claude Code)"
        ;;
      "ollama list")
        echo -e "NAME                ID              SIZE     MODIFIED"
        echo -e "qwen2.5-coder:7b    a418f5838eaf    4.7 GB   2 hours ago"
        ;;
      *)
        echo "$cmd: executed"
        ;;
    esac
    ;;

  version)
    echo "cloister 0.1.3"
    ;;
esac
