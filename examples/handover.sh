rm -rf /tmp/asciiscript-handover && mkdir -p /tmp/asciiscript-handover
cd /tmp/asciiscript-handover

cat > rock.yaml <<'YAML'
name: python
base: ubuntu@24.04
YAML

#$ pause 800
cat rock.yaml

#$ pause 1200
# Over to you: change the base to 'bare', then save and quit -- ctrl-o, enter, ctrl-x.
#$ handover
nano rock.yaml

#$ pause 800
# ...and the script carries on from where it left off.
cat rock.yaml

#$ pause 1500
grep -q '^base: bare$' rock.yaml && echo "your edit was recorded"
#$ pause 1500
