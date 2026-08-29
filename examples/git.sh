rm -rf /tmp/asciiscript-demo && mkdir -p /tmp/asciiscript-demo
cd /tmp/asciiscript-demo
export GIT_PAGER=cat GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

git init -q -b main .
git config user.name "Ada Lovelace"
git config user.email "ada@example.com"

#$ pause 800
echo "print('hello')" > hello.py
cat hello.py

git add hello.py
git commit -q -m "feat: hello"

#$ delay 60
echo "print('hello, world')" > hello.py
#$ pause 500
git diff --stat

#$ pause 800
git commit -q -am "fix: greet the world"

#$ pause 1500
git log --graph --oneline --decorate
#$ pause 2000
