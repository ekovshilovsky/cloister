# cloister demo
# Prerequisites: run these BEFORE recording:
#   ./cloister create work --defaults --stack web
#   ./cloister create personal --defaults
#   ./cloister stop all
#   # Set anonymous prompt inside VMs:
#   colima ssh --profile cloister-work -- bash -c 'echo "export PS1=\"\\$ \"" >> ~/.bashrc'
#   colima ssh --profile cloister-personal -- bash -c 'echo "export PS1=\"\\$ \"" >> ~/.bashrc'
#
# Run: PATH="$PWD:$PATH" vhs demo.tape

Output demo.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 700
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

# Override host prompt
Env PS1 "$ "
Env HOSTNAME "mac"
# cloister binary must be in PATH — run with: PATH="$PWD:$PATH" vhs demo.tape

# Show status — two pre-created profiles
Type "cloister status"
Enter
Sleep 2s

# Enter work profile — starts VM, discovers tunnels, sets color
Type "cloister work"
Enter
Sleep 8s

# Show we're inside an isolated VM
Type "claude --version && node --version"
Enter
Sleep 2s

Type "echo 'Each profile has its own ~/.claude — separate credentials, history, CLAUDE.md'"
Enter
Sleep 2s

# Exit VM
Type "exit"
Enter
Sleep 1s

# Show status — work is running
Type "cloister status"
Enter
Sleep 2s

# Stop everything
Type "cloister stop all"
Enter
Sleep 8s

# Final status
Type "cloister status"
Enter
Sleep 2s

Type "echo 'Install: brew install ekovshilovsky/tap/cloister'"
Enter
Sleep 2s
