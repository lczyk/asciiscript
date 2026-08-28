#$ delay 40
#$ wait 400
echo "A calm pace: 40ms a keystroke, 400ms between commands."

#$ delay 130
echo "Slow and deliberate -- good for the line you want read."

#$ delay 15
echo "Fast, for boilerplate nobody needs to watch being typed."

#$ delay 40
#$ wait 1200
echo "A long wait before the next line lets this output breathe."

echo "See? Room to look at the previous line."

#$ wait 2500
sleep 2 && echo "Slow work needs no padding -- the next line waits for it."

#$ wait 400
echo "Back to a normal pace."
