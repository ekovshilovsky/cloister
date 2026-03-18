# cloister demo
# Prerequisites: run these BEFORE recording:
#   ./cloister create work --defaults --stack web
#   ./cloister create personal --defaults
#   ./cloister stop all
#
# Run: vhs demo.tape

Output demo.gif

Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 700
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

# Override prompt to hide real hostname and username
Env PS1 "$ "
Env HOSTNAME "mac"

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
