// Copyright (C) 2015-2020  Nexedi SA and Contributors.
//                          Kirill Smelkov <kirr@nexedi.com>
//
// This program is free software: you can Use, Study, Modify and Redistribute
// it under the terms of the GNU General Public License version 3, or (at your
// option) any later version, as published by the Free Software Foundation.
//
// You can also Link and Combine this program with other software covered by
// the terms of any of the Free Software licenses or any of the Open Source
// Initiative approved licenses and Convey the resulting work. Corresponding
// source of such a combination shall include the source code for all other
// software used.
//
// This program is distributed WITHOUT ANY WARRANTY; without even the implied
// warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
//
// See COPYING file for full licensing terms.
// See https://www.nexedi.com/licensing for rationale and options.

package main
// Git-backup | Run git subprocess

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"lab.nexedi.com/kirr/go123/exc"
	"lab.nexedi.com/kirr/go123/mem"
)

// how/whether to redirect stdio of spawned process
type StdioRedirect int

const (
	PIPE StdioRedirect = iota // connect stdio channel via PIPE to parent (default value)
	DontRedirect
)

type RunWith struct {
	stdin  string
	stdout StdioRedirect     // PIPE | DontRedirect
	stderr StdioRedirect     // PIPE | DontRedirect
	raw    bool              // !raw -> stdout, stderr are stripped
	env    map[string]string // !nil -> subprocess environment setup from env
}

// run `git *argv` -> error, stdout, stderr
func _git(ctx context.Context, argv []string, rctx RunWith) (err error, stdout, stderr string) {
	debugf("git %s", strings.Join(argv, " "))

	// XXX exec.CommandContext does `kill -9` on ctx cancel
	// XXX -> rework to `kill -TERM` so that spawned process can finish cleanly?
	cmd := exec.CommandContext(ctx, "git", argv...)
	stdoutBuf := bytes.Buffer{}
	stderrBuf := bytes.Buffer{}

	if rctx.stdin != "" {
		cmd.Stdin = strings.NewReader(rctx.stdin)
	}

	switch rctx.stdout {
	case PIPE:
		cmd.Stdout = &stdoutBuf
	case DontRedirect:
		cmd.Stdout = os.Stdout
	default:
		panic("git: stdout redirect mode invalid")
	}

	switch rctx.stderr {
	case PIPE:
		cmd.Stderr = &stderrBuf
	case DontRedirect:
		cmd.Stderr = os.Stderr
	default:
		panic("git: stderr redirect mode invalid")
	}

	if rctx.env != nil {
		env := []string{}
		for k, v := range rctx.env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	err = cmd.Run()
	stdout = mem.String(stdoutBuf.Bytes())
	stderr = mem.String(stderrBuf.Bytes())

	if !rctx.raw {
		// prettify stdout (e.g. so that 'sha1\n' becomes 'sha1' and can be used directly
		stdout = strings.TrimSpace(stdout)
		stderr = strings.TrimSpace(stderr)
	}

	return err, stdout, stderr
}

// error a git command returned
type GitError struct {
	GitErrContext
	*exec.ExitError
}

type GitErrContext struct {
	argv   []string
	stdin  string
	stdout string
	stderr string
}

func (e *GitError) Error() string {
	msg := e.GitErrContext.Error()
	if e.stderr == "" {
		msg += "(failed)\n"
	}
	return msg
}

func (e *GitErrContext) Error() string {
	msg := "git " + strings.Join(e.argv, " ")
	if e.stdin == "" {
		msg += " </dev/null\n"
	} else {
		msg += " <<EOF\n" + e.stdin
		if !strings.HasSuffix(msg, "\n") {
			msg += "\n"
		}
		msg += "EOF\n"
	}

	msg += e.stderr
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	return msg
}

// ctx, argv -> ctx, []string, rctx  (for passing argv + RunWith handy - see ggit() for details)
func _gitargv(ctx context.Context, argv ...interface{}) (_ context.Context, argvs []string, rctx RunWith) {
	rctx_seen := false

	for _, arg := range argv {
		switch arg := arg.(type) {
		case string:
			argvs = append(argvs, arg)
		default:
			argvs = append(argvs, fmt.Sprint(arg))
		case RunWith:
			if rctx_seen {
				panic("git: multiple RunWith contexts")
			}
			rctx, rctx_seen = arg, true
		}
	}

	return ctx, argvs, rctx
}

// run `git *argv` -> err, stdout, stderr
// - arguments are automatically converted to strings
// - RunWith argument is passed as rctx
// - error is returned only when git command could run and exits with error status
// - on other errors - exception is raised
//
// NOTE err is concrete *GitError, not error
func ggit(ctx context.Context, argv ...interface{}) (err *GitError, stdout, stderr string) {
	return ggit2(_gitargv(ctx, argv...))
}

func ggit2(ctx context.Context, argv []string, rctx RunWith) (err *GitError, stdout, stderr string) {
	e, stdout, stderr := _git(ctx, argv, rctx)
	eexec, _ := e.(*exec.ExitError)
	if e != nil && eexec == nil {
		exc.Raisef("git %s : %s", strings.Join(argv, " "), e)
	}
	if eexec != nil {
		err = &GitError{GitErrContext{argv, rctx.stdin, stdout, stderr}, eexec}
	}
	return err, stdout, stderr
}

// run `git *argv` -> stdout
// on error - raise exception
func xgit(ctx context.Context, argv ...interface{}) string {
	return xgit2(_gitargv(ctx, argv...))
}

func xgit2(ctx context.Context, argv []string, rctx RunWith) string {
	gerr, stdout, _ := ggit2(ctx, argv, rctx)
	if gerr != nil {
		exc.Raise(gerr)
	}
	return stdout
}

// like xgit(), but automatically parse stdout to Sha1
func xgitSha1(ctx context.Context, argv ...interface{}) Sha1 {
	return xgit2Sha1(_gitargv(ctx, argv...))
}

// error when git output is not valid sha1
type GitSha1Error struct {
	GitErrContext
}

func (e *GitSha1Error) Error() string {
	msg := e.GitErrContext.Error()
	msg += fmt.Sprintf("expected valid sha1 (got %q)\n", e.stdout)
	return msg
}

func xgit2Sha1(ctx context.Context, argv []string, rctx RunWith) Sha1 {
	gerr, stdout, stderr := ggit2(ctx, argv, rctx)
	if gerr != nil {
		exc.Raise(gerr)
	}
	sha1, err := Sha1Parse(stdout)
	if err != nil {
		exc.Raise(&GitSha1Error{GitErrContext{argv, rctx.stdin, stdout, stderr}})
	}
	return sha1
}
