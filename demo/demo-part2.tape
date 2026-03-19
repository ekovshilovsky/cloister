# Part 2: Inside VM (green tinted background) — login, claude, ollama
Output "demo-part2.gif"

Set Shell "bash"
Set FontSize 18
Set Width 900
Set Height 600
Set Theme "darkmatrix"
Set TypingSpeed 30ms
Set Padding 20

Env PS1 "\033[1;35m❯\033[0m "
Env PATH "./:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"

Hide
Type "cp demo-cloister.sh cloister && cp demo-brew.sh brew && cp demo-claude.sh claude && chmod +x cloister brew claude"
Enter
Sleep 300ms
Type "clear"
Enter
Sleep 300ms
Show

Type "claude login"
Enter
Sleep 2500ms

Type "claude"
Enter
Sleep 3s

Type "Review the auth module for security issues"
Sleep 2500ms

Hide
Ctrl+C
Sleep 200ms
Type "clear"
Enter
Sleep 300ms
Show

Type "ollama list"
Enter
Sleep 1200ms

Hide
Type 'echo -e "NAME                ID              SIZE     MODIFIED"'
Enter
Type 'echo -e "qwen2.5-coder:7b    a418f5838eaf    4.7 GB   2 hours ago"'
Enter
Sleep 200ms
Show
Sleep 1500ms
