# cloister demo
# Uses demo-cloister.sh as a fake CLI for colorful output.
# Run from the directory containing demo-cloister.sh:
#   vhs demo.tape

Output demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 900
Set Height 550
Set Theme "Dracula"
Set TypingSpeed 30ms
Set Padding 20

Env PS1 "\033[1;35m❯\033[0m "
Env PATH "./:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"

# Rename demo script so typed commands show "cloister" not "./demo-cloister.sh"
Hide
Type "cp demo-cloister.sh cloister && chmod +x cloister"
Enter
Sleep 300ms
Type "clear"
Enter
Sleep 300ms
Show

# Install
Type "brew install ekovshilovsky/tap/cloister"
Enter
Sleep 2s

# Create work profile with web stack
Type "cloister create work --defaults --stack web"
Enter
Sleep 2s

# Create personal profile
Type "cloister create personal --defaults"
Enter
Sleep 2s

# Status — both running
Type "cloister status"
Enter
Sleep 2500ms

# Enter work profile
Type "cloister work"
Enter
Sleep 2s

# Stop all
Type "cloister stop all"
Enter
Sleep 2s
