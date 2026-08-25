echo "Hello, world..."
echo "Here's a demo of asciiscript."

# Comments with a '$' are control commands.

#$ delay 100  - Time between keypresses for subsequent commands (milliseconds).
echo "We can type slow..."
#$ delay 10
echo "Or quite fast."

#$ wait 100  - Time between commands for subsequent commands (milliseconds).
sleep 1 && echo "We can wait for output..."
#$ wait 500
echo "Because otherwise, things could get a bit weird."

echo 'I hope you like it!'
