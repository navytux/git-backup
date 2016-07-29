// Copyright (C) 2015-2016  Nexedi SA and Contributors.
//                          Kirill Smelkov <kirr@nexedi.com>
//
// This program is free software: you can Use, Study, Modify and Redistribute
// it under the terms of the GNU General Public License version 3, or (at your
// option) any later version, as published by the Free Software Foundation.
//
// This program is distributed WITHOUT ANY WARRANTY; without even the implied
// warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
//
// See COPYING file for full licensing terms.

// Git-backup | Run git subprocess
package main

import (
    "bytes"
    "fmt"
    "os"
    "os/exec"
    "strings"
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
func _git(argv []string, ctx RunWith) (err error, stdout, stderr string) {
    debugf("git %s", strings.Join(argv, " "))

    cmd := exec.Command("git", argv...)
    stdoutBuf := bytes.Buffer{}
    stderrBuf := bytes.Buffer{}

    if ctx.stdin != "" {
        cmd.Stdin = strings.NewReader(ctx.stdin)
    }

    switch ctx.stdout {
    case PIPE:
        cmd.Stdout = &stdoutBuf
    case DontRedirect:
        cmd.Stdout = os.Stdout
    default:
        panic("git: stdout redirect mode invalid")
    }

    switch ctx.stderr {
    case PIPE:
        cmd.Stderr = &stderrBuf
    case DontRedirect:
        cmd.Stderr = os.Stderr
    default:
        panic("git: stderr redirect mode invalid")
    }

    if ctx.env != nil {
        env := []string{}
        for k, v := range ctx.env {
            env = append(env, k+"="+v)
        }
        cmd.Env = env
    }

    err = cmd.Run()
    stdout = String(stdoutBuf.Bytes())
    stderr = String(stderrBuf.Bytes())

    if !ctx.raw {
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

// argv -> []string, ctx    (for passing argv + RunWith handy - see ggit() for details)
func _gitargv(argv ...interface{}) (argvs []string, ctx RunWith) {
    ctx_seen := false

    for _, arg := range argv {
        switch arg := arg.(type) {
        case string:
            argvs = append(argvs, arg)
        default:
            argvs = append(argvs, fmt.Sprint(arg))
        case RunWith:
            if ctx_seen {
                panic("git: multiple RunWith contexts")
            }
            ctx, ctx_seen = arg, true
        }
    }

    return argvs, ctx
}

// run `git *argv` -> err, stdout, stderr
// - arguments are automatically converted to strings
// - RunWith argument is passed as ctx
// - error is returned only when git command could run and exits with error status
// - on other errors - exception is raised
//
// NOTE err is concrete *GitError, not error
func ggit(argv ...interface{}) (err *GitError, stdout, stderr string) {
    return ggit2(_gitargv(argv...))
}

func ggit2(argv []string, ctx RunWith) (err *GitError, stdout, stderr string) {
    e, stdout, stderr := _git(argv, ctx)
    eexec, _ := e.(*exec.ExitError)
    if e != nil && eexec == nil {
        raisef("git %s : ", strings.Join(argv, " "), e)
    }
    if eexec != nil {
        err = &GitError{GitErrContext{argv, ctx.stdin, stdout, stderr}, eexec}
    }
    return err, stdout, stderr
}

// run `git *argv` -> stdout
// on error - raise exception
func xgit(argv ...interface{}) string {
    return xgit2(_gitargv(argv...))
}

func xgit2(argv []string, ctx RunWith) string {
    gerr, stdout, _ := ggit2(argv, ctx)
    if gerr != nil {
        raise(gerr)
    }
    return stdout
}

// like xgit(), but automatically parse stdout to Sha1
func xgitSha1(argv ...interface{}) Sha1 {
    return xgit2Sha1(_gitargv(argv...))
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

func xgit2Sha1(argv []string, ctx RunWith) Sha1 {
    gerr, stdout, stderr := ggit2(argv, ctx)
    if gerr != nil {
        raise(gerr)
    }
    sha1, err := Sha1Parse(stdout)
    if err != nil {
        raise(&GitSha1Error{GitErrContext{argv, ctx.stdin, stdout, stderr}})
    }
    return sha1
}
