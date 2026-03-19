# Part 3: Back on host — stop all
Output "demo-part3.gif"

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
Type "cp demo-cloister.sh cloister && chmod +x cloister"
Enter
Sleep 300ms
Type "clear"
Enter
Sleep 300ms
Show

Type "cloister stop all"
Enter
Sleep 2s
