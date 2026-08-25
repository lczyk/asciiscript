#$ delay 45
#$ wait 500
rm -rf /tmp/asciiscript-demo && mkdir -p /tmp/asciiscript-demo
cd /tmp/asciiscript-demo
export GIT_PAGER=cat GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

git init -q -b main .
git config user.name "Ada Lovelace"
git config user.email "ada@example.com"

#$ wait 800
echo "print('hello')" > hello.py
cat hello.py

git add hello.py
git commit -q -m "feat: hello"

#$ delay 60
echo "print('hello, world')" > hello.py
git diff --stat

#$ delay 45
git commit -q -am "fix: greet the world"

#$ wait 1500
git log --graph --oneline --decorate
