# cloister demo
# This demo uses fake output via echo to avoid real VM timing issues.
# Run: vhs demo.tape

Output demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 900
Set Height 550
Set Theme "Dracula"
Set TypingSpeed 30ms
Set Padding 20

# Colored prompt
Env PS1 "\033[1;35m❯\033[0m "

# Simulate cloister commands with realistic output
# This avoids real VM startup timing and keeps the demo snappy.

Sleep 500ms

Type "# Cloister: Run Multiple Claude Code Accounts & AI Agents on One Mac"
Enter
Sleep 1s

# Show create
Type "cloister create work --defaults --stack web"
Enter
Sleep 300ms
Type 'echo -e "Profile \033[1;32m\"work\"\033[0m created.\nStarting \"work\"...\nProvisioning environment...\n\033[1;32m✓\033[0m Base tools installed\n\033[1;32m✓\033[0m Web stack installed\n\nProfile \"work\" ready. Enter with: \033[1;36mcloister work\033[0m"'
Enter
Sleep 2s

# Show create personal
Type "cloister create personal --defaults"
Enter
Sleep 300ms
Type 'echo -e "Profile \033[1;32m\"personal\"\033[0m created.\nStarting \"personal\"...\nProvisioning environment...\n\033[1;32m✓\033[0m Base tools installed\n\nProfile \"personal\" ready. Enter with: \033[1;36mcloister personal\033[0m"'
Enter
Sleep 2s

# Status — both running
Type "cloister status"
Enter
Sleep 300ms
Type 'echo -e "\033[1mPROFILE    STATE     MEMORY  IDLE    STACKS\033[0m\n\033[32mpersonal   running   4GB     2m      -\033[0m\n\033[36mwork       running   4GB     5m      web\033[0m\n\nBudget: \033[1;33m8GB / 22GB\033[0m used\nTunnels: \033[32m✓\033[0m clipboard  \033[32m✓\033[0m op-forward  \033[32m✓\033[0m audio"'
Enter
Sleep 2s

# Enter work
Type "cloister work"
Enter
Sleep 300ms
Type 'echo -e "Starting \"work\"...\nTunnels:\n  \033[32m✓\033[0m clipboard (port 18339)\n  \033[32m✓\033[0m op-forward (port 18340)\n  \033[32m✓\033[0m audio (port 4713)\nEntering \033[1;36mwork\033[0m..."'
Enter
Sleep 2s

# Stop all
Type "cloister stop all"
Enter
Sleep 300ms
Type 'echo -e "Stopping \033[33m\"personal\"\033[0m... \033[32mStopped\033[0m\nStopping \033[33m\"work\"\033[0m... \033[32mStopped\033[0m"'
Enter
Sleep 2s

# Final status
Type "cloister status"
Enter
Sleep 300ms
Type 'echo -e "\033[1mPROFILE    STATE     MEMORY  IDLE    STACKS\033[0m\n\033[90mpersonal   stopped   4GB     —       -\033[0m\n\033[90mwork       stopped   4GB     —       web\033[0m\n\nBudget: \033[1;33m0GB / 22GB\033[0m used"'
Enter
Sleep 2s

# Install CTA
Type 'echo -e "\n\033[1;35m→ brew install ekovshilovsky/tap/cloister\033[0m"'
Enter
Sleep 2s
