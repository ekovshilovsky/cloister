# cloister demo
# Uses demo scripts as fake CLIs for colorful output without real VMs.
# Run from the demo/ directory:
#   cd demo && vhs demo.tape

Output demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 900
Set Height 600
Set Theme "Dracula"
Set TypingSpeed 30ms
Set Padding 20

Env PS1 "\033[1;35m❯\033[0m "
Env PATH "./:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"

# Rename demo scripts so typed commands show clean names
Hide
Type "cp demo-cloister.sh cloister && cp demo-brew.sh brew && cp demo-claude.sh claude && chmod +x cloister brew claude"
Enter
Sleep 300ms
Type "clear"
Enter
Sleep 300ms
Show

# --- Install ---
Type "brew install ekovshilovsky/tap/cloister"
Enter
Sleep 2s

# --- Create work profile with web and ollama stacks ---
Type "cloister create work --defaults --stack web,ollama"
Enter
Sleep 2s

# --- Create personal profile with web and cloud ---
Type "cloister create personal --defaults --stack web,cloud"
Enter
Sleep 2s

# --- Status — both running ---
Type "cloister status"
Enter
Sleep 2500ms

# --- Enter work profile ---
Type "cloister work"
Enter
Sleep 1500ms

# After entering, apply a green-tinted background via ANSI 24-bit color.
# This simulates the iTerm2 background change that happens in real usage.
# Color 051a05 = RGB(5, 26, 5)
Hide
Type 'export PS1="\033[48;2;5;26;5m\033[1;35m❯\033[0m\033[48;2;5;26;5m "'
Enter
Sleep 200ms
# Fill background by clearing with the color set
Type "printf '\033[48;2;5;26;5m'; clear"
Enter
Sleep 300ms
Show

# --- First time in VM: login ---
Type "claude login"
Enter
Sleep 2500ms

# --- Start Claude Code (first run shows theme picker) ---
Type "claude"
Enter
Sleep 3s

# --- User types a prompt ---
Type "Review the auth module for security issues"
Sleep 2500ms

# --- Exit Claude ---
Hide
Type ""
Enter
Sleep 200ms
Type "printf '\033[48;2;5;26;5m'; clear"
Enter
Sleep 300ms
Show

# --- Check ollama works via host tunnel ---
Type "ollama list"
Enter
Sleep 1200ms

# Simulate ollama list output
Hide
Type 'echo -e "NAME                ID              SIZE     MODIFIED"'
Enter
Type 'echo -e "qwen2.5-coder:7b    a418f5838eaf    4.7 GB   2 hours ago"'
Enter
Sleep 200ms
Show
Sleep 1500ms

# --- Exit VM, reset background to default Dracula (40, 42, 54) ---
Hide
Type 'export PS1="\033[1;35m❯\033[0m "'
Enter
Type "printf '\033[0m'; clear"
Enter
Sleep 300ms
Show

# --- Stop all ---
Type "cloister stop all"
Enter
Sleep 2s
