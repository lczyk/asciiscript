package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lczyk/assert"
)

func TestParseScriptCommands(t *testing.T) {
	s, err := parseScript("echo hi\n#$ delay 100\n#$ wait 250\necho bye")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 4)

	sh, ok := s.Commands[0].(Shell)
	assert.That(t, ok, "command 0 should be a Shell")
	assert.Equal(t, sh.Cmd, "echo hi\n")

	d, ok := s.Commands[1].(Delay)
	assert.That(t, ok, "command 1 should be a Delay")
	assert.Equal(t, d.Interval, 100*time.Millisecond)

	w, ok := s.Commands[2].(Wait)
	assert.That(t, ok, "command 2 should be a Wait")
	assert.Equal(t, w.Duration, 250*time.Millisecond)

	sh2, ok := s.Commands[3].(Shell)
	assert.That(t, ok, "command 3 should be a Shell")
	assert.Equal(t, sh2.Cmd, "echo bye\n")
}

func TestParseScriptDefaults(t *testing.T) {
	s, err := parseScript("echo hi")
	assert.NoError(t, err)
	assert.Equal(t, s.Delay, 40*time.Millisecond)
	assert.Equal(t, s.Wait, 100*time.Millisecond)
}

func TestParseScriptSkipsBlankLines(t *testing.T) {
	s, err := parseScript("echo a\n\n\necho b\n")
	assert.NoError(t, err)
	assert.Len(t, s.Commands, 2)
}

func TestNewShellAppendsNewline(t *testing.T) {
	assert.Equal(t, NewShell("echo hi").Cmd, "echo hi\n")
	assert.Equal(t, NewShell("echo hi\n").Cmd, "echo hi\n")
}

func TestParseScriptUnknownCtrl(t *testing.T) {
	_, err := parseScript("#$ bogus 1")
	assert.ErrorIs(t, err, ErrUnknownCtrl)
}

func TestParseScriptCtrlNoArgs(t *testing.T) {
	_, err := parseScript("#$ delay")
	assert.ErrorIs(t, err, ErrNoArgs)
}

func TestParseScriptCtrlBadArg(t *testing.T) {
	_, err := parseScript("#$ wait abc")
	assert.ErrorIs(t, err, ErrBadArg)
}

func TestBashCommand(t *testing.T) {
	cmd, cleanup, err := bashCommand()
	assert.NoError(t, err)
	assert.ContainsString(t, cmd, "--rcfile")
	assert.ContainsString(t, cmd, "BASH_SILENCE_DEPRECATION_WARNING=1")

	fields := strings.Fields(cmd)
	path := fields[len(fields)-1]
	_, statErr := os.Stat(path)
	assert.NoError(t, statErr, "rcfile should exist before cleanup")

	cleanup()
	_, statErr = os.Stat(path)
	assert.That(t, os.IsNotExist(statErr), "rcfile should be gone after cleanup")
}

func TestStripQueries(t *testing.T) {
	in := []byte("before\x1b[6nmiddle\x1b[0c\x1b[?2026$p\x1b]11;?\x07after")
	assert.Equal(t, string(stripQueries(in)), "beforemiddleafter")
}

func TestStripQueriesNoQuery(t *testing.T) {
	assert.Equal(t, string(stripQueries([]byte("plain text\r\n"))), "plain text\r\n")
}
