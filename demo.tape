# cloister demo
# Prerequisites: run these BEFORE recording:
#   make build
#   ./cloister create work --defaults --stack web
#   ./cloister create personal --defaults
#   ./cloister stop all
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

# Override host prompt and ensure cloister is in PATH
Env PS1 "$ "
Env HOSTNAME "mac"

# Alias cloister to absolute path so it survives SSH session exit
Hide
Type "alias cloister=$(pwd)/cloister"
Enter
Sleep 500ms
Show

# Show status — two pre-created profiles
Type "cloister status"
Enter
Sleep 2s

# Enter work profile — starts VM, discovers tunnels, sets color
Type "cloister work"
Enter
Sleep 12s

# Show we're inside an isolated VM
Type "claude --version && node --version"
Enter
Sleep 3s

Type "echo 'Each profile has its own ~/.claude — separate credentials, history, CLAUDE.md'"
Enter
Sleep 3s

# Exit VM
Type "exit"
Enter
Sleep 2s

# Show status — work is running
Type "cloister status"
Enter
Sleep 3s

# Stop everything
Type "cloister stop all"
Enter
Sleep 12s

# Final status
Type "cloister status"
Enter
Sleep 3s

Type "echo 'Install: brew install ekovshilovsky/tap/cloister'"
Enter
Sleep 2s
