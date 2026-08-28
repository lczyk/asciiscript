echo "Hello, world..."
echo "Here's a demo of asciiscript."

# Comments with a '$' are control commands.

#$ delay 100  - Time between keypresses for subsequent commands (milliseconds).
echo "We can type slow..."
#$ delay 10
echo "Or quite fast."

#$ wait 100  - Pause after each command finishes, before the next (milliseconds).
sleep 1 && echo "The next line waits for this to finish on its own..."
#$ wait 500
echo "...and then this one gets a longer beat before it."

echo 'I hope you like it!'
