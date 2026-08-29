echo "Hello, world..."
echo "Here's a demo of asciiscript."

# Comments with a '$' are control lines. Each one applies to the next command only.

#$ delay 100  - Time between keypresses for this command (milliseconds).
echo "We can type slow..."
#$ delay 10
echo "Or quite fast."

echo "And the line after is back to the usual pace."

#$ pause 1500  - Hold the previous output on screen this long before typing (milliseconds).
echo "...that gave the line above room to be read."

sleep 1 && echo "A slow command needs no pause: the next line waits for it on its own."
echo 'I hope you like it!'
#$ pause 1000
