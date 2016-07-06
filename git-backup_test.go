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

package main

import (
    "fmt"
    "io/ioutil"
    "os"
    "path/filepath"
    "regexp"
    "strings"
    "syscall"
    "testing"
)

func xgetcwd(t *testing.T) string {
    cwd, err := os.Getwd()
    if err != nil {
        t.Fatal(err)
    }
    return cwd
}

func xchdir(t *testing.T, dir string) {
    err := os.Chdir(dir)
    if err != nil {
        t.Fatal(err)
    }
}

// verify end-to-end pull-restore
func TestPullRestore(t *testing.T) {
    // if something raises -> don't let testing panic - report it as proper error with context.
    here := myfuncname()
    defer errcatch(func(e *Error) {
        e = erraddcallingcontext(here, e)

        // add file:line for failing code inside testing function - so we have exact context to debug
        failedat := ""
        for _, f := range xtraceback(1) {
            if f.Name() == here {
                // TODO(go1.7) -> f.File, f.Line  (f becomes runtime.Frame)
                file, line := f.FileLine(f.pc - 1)
                failedat = fmt.Sprintf("%s:%d", filepath.Base(file), line)
                break
            }
        }
        if failedat == "" {
            panic(fmt.Errorf("cannot lookup failedat for %s", here))
        }

        t.Errorf("%s: %v", failedat, e)
    })

    workdir, err := ioutil.TempDir("", "t-git-backup")
    if err != nil {
        t.Fatal(err)
    }
    defer os.RemoveAll(workdir)

    mydir := xgetcwd(t)
    xchdir(t, workdir)
    defer xchdir(t, mydir)

    // -test.v -> verbosity of git-backup
    if testing.Verbose() {
        verbose = 1
    } else {
        verbose = 0
    }

    // init backup repository
    xgit("init", "--bare", "backup.git")
    xchdir(t, "backup.git")

    // pull from testdata
    my1 := mydir + "/testdata/1"
    cmd_pull([]string{my1+":b1"})

    // prune all non-reachable objects (e.g. tags just pulled - they were encoded as commits)
    xgit("prune")

    // verify backup repo is all ok
    xgit("fsck")

    // verify that just pulled tag objects are now gone after pruning -
    // - they become not directly git-present. The only possibility to
    // get them back is via recreating from encoded commit objects.
    tags := []string{"11e67095628aa17b03436850e690faea3006c25d",
                     "ba899e5639273a6fa4d50d684af8db1ae070351e",
                     "7124713e403925bc772cd252b0dec099f3ced9c5",
                     "f735011c9fcece41219729a33f7876cd8791f659"}
    for _, tag := range tags {
        gerr, _, _ := git("cat-file", "-p", tag)
        if gerr == nil {
            t.Fatalf("tag %s still present in backup.git after git-prune", tag)
        }
    }

    // restore backup
    work1 := workdir + "/1"
    cmd_restore([]string{"HEAD", "b1:"+work1})

    // verify files restored to the same as original
    gerr, diff, _ := git("diff", "--no-index", "--raw", "--exit-code", my1, work1)
    // 0 - no diff, 1 - has diff, 2 - problem
    if gerr != nil && gerr.Sys().(syscall.WaitStatus).ExitStatus() > 1 {
        t.Fatal(gerr)
    }
    gitObjectsRe := regexp.MustCompile(`\.git/objects/`)
    for _, diffline := range strings.Split(diff, "\n") {
        // :srcmode dstmode srcsha1 dstsha1 status\tpath
        _, path, err := headtail(diffline, "\t")
        if err != nil {
            t.Fatalf("restorecheck: cannot parse diff line %q", diffline)
        }
        // git objects can be represented differently (we check them later)
        if gitObjectsRe.FindString(path) != "" {
            continue
        }
        t.Fatal("restorecheck: unexpected diff:", diffline)
    }

    // verify git objects restored to the same as original
    err = filepath.Walk(my1, func(path string, info os.FileInfo, err error) error {
        // any error -> stop
        if err != nil {
            return err
        }

        // non *.git/ -- not interesting
        if !(info.IsDir() && strings.HasSuffix(path, ".git")) {
            return nil
        }

        // found git repo - check refs & objects in original and restored are exactly the same,
        var R = [2]struct{ path, reflist, revlist string }{
            {path: path},                       // original
            {path: reprefix(my1, work1, path)}, // restored
        }

        for _, repo := range R {
            // fsck just in case
            xgit("--git-dir=" + repo.path, "fsck")
            // NOTE for-each-ref sorts output by refname
            repo.reflist = xgit("--git-dir=" + repo.path, "for-each-ref")
            // NOTE rev-list emits objects in reverse chronological order,
            //      starting from refs roots which are also ordered by refname
            repo.revlist = xgit("--git-dir=" + repo.path, "rev-list", "--all", "--objects")
        }

        if R[0].reflist != R[1].reflist {
            t.Fatalf("restorecheck: %q restored with different reflist (in %q)", R[0].path, R[1].path)
        }

        if R[0].revlist != R[1].revlist {
            t.Fatalf("restorecheck: %q restored with differrent objects (in %q)", R[0].path, R[1].path)
        }

        // .git verified - no need to recurse
        return filepath.SkipDir
    })

    if err != nil {
        t.Fatal(err)
    }

    // now try to pull corrupt repo - pull should refuse if transferred pack contains bad objects
    my2 := mydir + "/testdata/2"
    func() {
        defer errcatch(func(e *Error) {
            // it ok - pull should raise
        })
        cmd_pull([]string{my2+":b2"})
        t.Fatal("fetching from corrupt.git did not complain")
    }()
}

func TestRepoRefSplit(t *testing.T) {
    var tests = []struct{ reporef, repo, ref string }{
        {"kirr/wendelin.core.git/heads/master", "kirr/wendelin.core.git", "heads/master"},
        {"kirr/erp5.git/backup/x/master+erp5-data-notebook", "kirr/erp5.git", "backup/x/master+erp5-data-notebook"},
        {"tiwariayush/Discussion%20Forum%20.git/...", "tiwariayush/Discussion Forum .git", "..."},
        {"tiwariayush/Discussion%20Forum+.git/...", "tiwariayush/Discussion Forum+.git", "..."},
        {"tiwariayush/Discussion%2BForum+.git/...", "tiwariayush/Discussion+Forum+.git", "..."},
    }

    for _, tt := range tests {
        repo, ref := reporef_split(tt.reporef)
        if repo != tt.repo || ref != tt.ref {
            t.Errorf("reporef_split(%q) -> %q %q  ; want %q %q", tt.reporef, repo, ref, tt.repo, tt.ref)
        }
    }
}
