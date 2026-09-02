# Reproducible demo recording: vhs .vhs/demo.tape
# Only commands that print no real addresses are used here.

Output docs/demo.gif

Set Shell zsh
Set FontSize 15
Set Width 1280
Set Height 440
Set Padding 20
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

Hide
Type "setopt interactive_comments; clear" Enter
Sleep 1s
Show

Type "splitr doctor" Sleep 400ms Enter
Sleep 4s

Type "clear" Enter
Sleep 300ms

Type "splitr status" Sleep 400ms Enter
Sleep 3s

Type "# no tunnel, so pf drops anything headed for a protected route" Sleep 300ms Enter
Sleep 1200ms
Type "curl --max-time 3 http://10.5.5.5" Sleep 400ms Enter
Sleep 4500ms

Type "clear" Enter
Sleep 300ms

Type "# the rules pf loads, generated from the example config" Sleep 300ms Enter
Sleep 1200ms
Type "splitr validate config.example.yaml" Sleep 400ms Enter
Sleep 6s
