#!/bin/bash
# Fake cloister CLI for demo recording — produces colorful output without real VMs

GREEN='\033[1;32m'
CYAN='\033[1;36m'
YELLOW='\033[1;33m'
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
    [[ -n "$stack" ]] && echo -e "  ${GREEN}✓${RESET} ${stack} stack installed"
    echo ""
    echo -e "Profile \"${profile}\" ready. Enter with: ${CYAN}cloister ${profile}${RESET}"
    ;;

  status)
    echo -e "${BOLD}PROFILE    STATE     MEMORY  IDLE    STACKS${RESET}"
    echo -e "${GREEN}personal${RESET}   running   4GB     2m      -"
    echo -e "${CYAN}work${RESET}       running   4GB     5m      web"
    echo ""
    echo -e "Budget: ${YELLOW}8GB / 22GB${RESET} used"
    echo -e "Tunnels: ${GREEN}✓${RESET} clipboard  ${GREEN}✓${RESET} op-forward  ${GREEN}✓${RESET} audio"
    ;;

  work|personal)
    echo "Starting \"$1\"..."
    echo "Tunnels:"
    echo -e "  ${GREEN}✓${RESET} clipboard (port 18339)"
    echo -e "  ${GREEN}✓${RESET} op-forward (port 18340)"
    echo -e "  ${GREEN}✓${RESET} audio (port 4713)"
    echo -e "Entering ${CYAN}$1${RESET}..."
    ;;

  stop)
    echo -e "Stopping ${YELLOW}\"personal\"${RESET}... ${GREEN}Stopped${RESET}"
    echo -e "Stopping ${YELLOW}\"work\"${RESET}... ${GREEN}Stopped${RESET}"
    ;;

  version)
    echo "cloister 0.0.2"
    ;;
esac
