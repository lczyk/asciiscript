echo "The usual pace: 40ms a keystroke, a short beat between commands."

#$ delay 130
echo "Slow and deliberate -- good for the line you want read."

echo "And straight back to the usual pace: a control line is for one command only."

#$ delay 15
echo "Fast, for boilerplate nobody needs to watch being typed."

#$ pause 1200
echo "A pause in front of a command holds the previous output on screen first."

echo "See? Room to look at the previous line. This one got the usual short beat."

sleep 2 && echo "Slow work needs no padding -- the next line waits for it."

#$ delay 15
#$ pause 800
echo "Control lines stack: this one waited 800ms, then typed fast."

#$ pause 2000
