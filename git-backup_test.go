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

    git "github.com/libgit2/git2go"
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

func XSha1(s string) Sha1 {
    sha1, err := Sha1Parse(s)
    if err != nil {
        panic(err)
    }
    return sha1
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
    gb, err := git.OpenRepository(".")
    if err != nil {
        t.Fatal(err)
    }

    // pull from testdata
    my0 := mydir + "/testdata/0"
    cmd_pull(gb, []string{my0+":b0"}) // only empty repo in testdata/0

    my1 := mydir + "/testdata/1"
    cmd_pull(gb, []string{my1+":b1"})

    // verify tag/tree/blob encoding is 1) consistent and 2) always the same.
    // we need it be always the same so different git-backup versions can
    // interoperate with each other.
    var noncommitv = []struct{
        sha1  Sha1 // original
        sha1_ Sha1 // encoded
        istag bool // is original object a tag object
    }{
        {XSha1("f735011c9fcece41219729a33f7876cd8791f659"), XSha1("4f2486e99ff9744751e0756b155e57bb24c453dd"), true},  // tag-to-commit
        {XSha1("7124713e403925bc772cd252b0dec099f3ced9c5"), XSha1("6b3beabee3e0704fa3269558deab01e9d5d7764e"), true},  // tag-to-tag
        {XSha1("11e67095628aa17b03436850e690faea3006c25d"), XSha1("89ad5fbeb9d3f0c7bc6366855a09484819289911"), true},  // tag-to-blob
        {XSha1("ba899e5639273a6fa4d50d684af8db1ae070351e"), XSha1("68ad6a7c31042e53201e47aee6096ed081b6fdb9"), true},  // tag-to-tree
        {XSha1("61882eb85774ed4401681d800bb9c638031375e2"), XSha1("761f55bcdf119ced3fcf23b69fdc169cbb5fc143"), false}, // ref-to-tree
        {XSha1("7a3343f584218e973165d943d7c0af47a52ca477"), XSha1("366f3598d662909e2537481852e42b775c7eb837"), false}, // ref-to-blob
    }

    for _, nc := range noncommitv {
        // encoded object should be already present
        _, err := ReadObject2(gb, nc.sha1_)
        if err != nil {
            t.Fatalf("encode %s should give %s but expected encoded object not found: %s", nc.sha1, nc.sha1_, err)
        }

        // decoding encoded object should give original sha1, if it was tag
        sha1 := obj_recreate_from_commit(gb, nc.sha1_)
        if nc.istag && sha1 != nc.sha1 {
            t.Fatalf("decode %s -> %s ;  want %s", nc.sha1_, sha1, nc.sha1)
        }

        // encoding original object should give sha1_
        obj_type := xgit("cat-file", "-t", nc.sha1)
        sha1_ := obj_represent_as_commit(gb, nc.sha1, obj_type)
        if sha1_ != nc.sha1_ {
            t.Fatalf("encode %s -> %s ;  want %s", sha1, sha1_, nc.sha1_)
        }
    }

    // verify no garbage is left under refs/backup/
    dentryv, err := ioutil.ReadDir("refs/backup/")
    if err != nil && !os.IsNotExist(err) {
        t.Fatal(err)
    }
    if len(dentryv) != 0 {
        namev := []string{}
        for _, fi := range dentryv {
            namev = append(namev, fi.Name())
        }
        t.Fatalf("refs/backup/ not empty after pull: %v", namev)
    }

    // prune all non-reachable objects (e.g. tags just pulled - they were encoded as commits)
    xgit("prune")

    // verify backup repo is all ok
    xgit("fsck")

    // verify that just pulled tag objects are now gone after pruning -
    // - they become not directly git-present. The only possibility to
    // get them back is via recreating from encoded commit objects.
    for _, nc := range noncommitv {
        if !nc.istag {
            continue
        }
        gerr, _, _ := ggit("cat-file", "-p", nc.sha1)
        if gerr == nil {
            t.Fatalf("tag %s still present in backup.git after git-prune", nc.sha1)
        }
    }

    // reopen backup repository - to avoid having stale cache with present
    // objects we deleted above with `git prune`
    gb, err = git.OpenRepository(".")
    if err != nil {
        t.Fatal(err)
    }

    // restore backup
    work1 := workdir + "/1"
    cmd_restore(gb, []string{"HEAD", "b1:"+work1})

    // verify files restored to the same as original
    gerr, diff, _ := ggit("diff", "--no-index", "--raw", "--exit-code", my1, work1)
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
        cmd_pull(gb, []string{my2+":b2"})
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
