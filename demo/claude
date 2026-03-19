#!/bin/bash
# Fake claude CLI for demo recording — simulates first-run experience

BOLD='\033[1m'
DIM='\033[90m'
CYAN='\033[1;36m'
GREEN='\033[1;32m'
YELLOW='\033[1;33m'
PURPLE='\033[1;35m'
RESET='\033[0m'

if [[ "$1" == "--version" ]]; then
    echo "2.1.79 (Claude Code)"
    exit 0
fi

if [[ "$1" == "login" ]]; then
    echo ""
    echo -e "${BOLD}Authenticate with Claude${RESET}"
    echo ""
    sleep 0.5
    echo -e "  Opening browser for authentication..."
    sleep 1
    echo -e "  ${GREEN}✓${RESET} Authentication successful"
    echo -e "  ${DIM}Logged in as dev@company.com${RESET}"
    echo ""
    exit 0
fi

# First run — theme selection then session
echo ""
echo -e "${BOLD}Welcome to Claude Code!${RESET}"
echo ""
echo -e "  Choose a theme:"
echo -e "    ${BOLD}❯ Dark (default)${RESET}"
echo -e "      Light"
echo -e "      Light (high contrast)"
echo -e "      Dark (high contrast)"
echo ""
sleep 1
echo -e "  ${GREEN}✓${RESET} Theme: Dark"
echo ""
sleep 0.5
echo -e "${BOLD}╭──────────────────────────────────────────────────╮${RESET}"
echo -e "${BOLD}│${RESET}                                                  ${BOLD}│${RESET}"
echo -e "${BOLD}│${RESET}   ${CYAN}Claude Code${RESET} v2.1.79                          ${BOLD}│${RESET}"
echo -e "${BOLD}│${RESET}                                                  ${BOLD}│${RESET}"
echo -e "${BOLD}│${RESET}   ${DIM}Project:${RESET} ~/code/my-project                    ${BOLD}│${RESET}"
echo -e "${BOLD}│${RESET}   ${DIM}Model:${RESET}   claude-sonnet-4-6                    ${BOLD}│${RESET}"
echo -e "${BOLD}│${RESET}                                                  ${BOLD}│${RESET}"
echo -e "${BOLD}╰──────────────────────────────────────────────────╯${RESET}"
echo ""
echo -e "  ${DIM}Tips: /help for commands, /model to switch models${RESET}"
echo ""
printf "${PURPLE}❯${RESET} "
