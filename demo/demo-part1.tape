# Part 1: Host — install, create profiles, status, enter work
Output "demo-part1.gif"

Set Shell "bash"
Set FontSize 18
Set Width 900
Set Height 600
Set Theme "Dracula"
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

Type "brew install ekovshilovsky/tap/cloister"
Enter
Sleep 2s

Type "cloister create work --defaults --stack web,ollama"
Enter
Sleep 2s

Type "cloister create personal --defaults --stack web,cloud"
Enter
Sleep 2s

Type "cloister status"
Enter
Sleep 2500ms

Type "cloister work"
Enter
Sleep 2s
