// Copyright (C) 2015-2025  Nexedi SA and Contributors.
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

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"lab.nexedi.com/kirr/go123/exc"
	"lab.nexedi.com/kirr/go123/my"
	"lab.nexedi.com/kirr/go123/xerr"
	"lab.nexedi.com/kirr/go123/xruntime"
	"lab.nexedi.com/kirr/go123/xstrings"

	"lab.nexedi.com/kirr/git-backup/internal/git"
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

func xgittype(s string) git.ObjectType {
	type_, ok := gittype(s)
	if !ok {
		exc.Raisef("unknown git type %q", s)
	}
	return type_
}

// xnoref asserts that git reference ref does not exists.
func xnoref(ref string) {
	xgit(context.Background(), "update-ref", "--stdin", RunWith{stdin: fmt.Sprintf("verify refs/%s %s\n", ref, Sha1{})})
}

// xcopy recursively copies directory src to dst.
func xcopy(t *testing.T, src, dst string) {
	cmd := exec.Command("cp", "-a", src, dst)
	err := cmd.Run()
	if err != nil {
		t.Fatal(err)
	}
}

// xprepareTestdata ensures that all .git directories in testdata have objects and refs subdirectories created if missing.
func xprepareTestdata(t *testing.T, testdataDir string) {
	err := filepath.Walk(testdataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && strings.HasSuffix(path, ".git") {
			// create objects and refs if missing
			err1 := os.MkdirAll(filepath.Join(path, "objects"), 0777)
			err2 := os.MkdirAll(filepath.Join(path, "refs"), 0777)
			err = xerr.Merge(err1, err2)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}


// verify end-to-end pull-restore
func TestPullRestore(t *testing.T) {
	ctx := context.Background()

	// if something raises -> don't let testing panic - report it as proper error with context.
	here := my.FuncName()
	defer exc.Catch(func(e *exc.Error) {
		e = exc.Addcallingcontext(here, e)

		// add file:line for failing code inside testing function - so we have exact context to debug
		failedat := ""
		for _, f := range xruntime.Traceback(1) {
			if f.Function == here {
				failedat = fmt.Sprintf("%s:%d", filepath.Base(f.File), f.Line)
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
	xgit(ctx, "init", "--bare", "backup.git")
	xchdir(t, "backup.git")
	gb, err := git.OpenRepository(".")
	if err != nil {
		t.Fatal(err)
	}

	xprepareTestdata(t, mydir+"/testdata")

	// pull from testdata
	my0 := mydir + "/testdata/0"
	cmd_pull(ctx, gb, []string{my0 + ":b0"}) // only empty repo in testdata/0

	my1 := mydir + "/testdata/1"
	cmd_pull(ctx, gb, []string{my1 + ":b1"})

	// verify tag/tree/blob encoding is 1) consistent and 2) always the same.
	// we need it be always the same so different git-backup versions can
	// interoperate with each other.
	var noncommitv = []struct {
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
		obj_type := xgit(ctx, "cat-file", "-t", nc.sha1)
		sha1_ := obj_represent_as_commit(ctx, gb, nc.sha1, xgittype(obj_type))
		if sha1_ != nc.sha1_ {
			t.Fatalf("encode %s -> %s ;  want %s", sha1, sha1_, nc.sha1_)
		}
	}

	// checks / cleanups after cmd_pull
	afterPull := func() {
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
		xgit(ctx, "prune")	   // remove unreachable loose objects
		xgit(ctx, "repack", "-ad") // remove unreachable objects in packs

		// verify backup repo is all ok
		xgit(ctx, "fsck")

		// verify that just pulled tag objects are now gone after pruning -
		// - they become not directly git-present. The only possibility to
		// get them back is via recreating from encoded commit objects.
		for _, nc := range noncommitv {
			if !nc.istag {
				continue
			}
			gerr, _, _ := ggit(ctx, "cat-file", "-p", nc.sha1)
			if gerr == nil {
				t.Fatalf("tag %s still present in backup.git after git-prune + `git-repack -ad`", nc.sha1)
			}
		}

		// reopen backup repository - to avoid having stale cache with present
		// objects we deleted above with `git prune` + `git repack -ad`
		gb, err = git.OpenRepository(".")
		if err != nil {
			t.Fatal(err)
		}
	}

	afterPull()

	// pull again - it should be noop
	h1 := xgitSha1(ctx, "rev-parse", "HEAD")
	cmd_pull(ctx, gb, []string{my1 + ":b1"})
	afterPull()
	h2 := xgitSha1(ctx, "rev-parse", "HEAD")
	if h1 == h2 {
		t.Fatal("pull: second run did not ajusted HEAD")
	}
	δ12 := xgit(ctx, "diff", h1, h2)
	if δ12 != "" {
		t.Fatalf("pull: second run was not noop: δ:\n%s", δ12)
	}

	// pull again with modifying one of the repositories simultaneously
	//
	// This verifies that pulling results in consistent refs and objects
	// even if backed'up repo is modified concurrently. Previously in such
	// case pull used to create backup.refs and repo.git/refs inconsistent
	// to each other leading to an error on restore.
	//
	// We test this case via modifying a ref right after fetch but before
	// pulling of plain files from inside repo.git . In order to create
	// consisten backup pull should ignore content of refs in files and
	// read refs only once - when doing the fetch.
	mod1 := workdir + "/1mod"
	xcopy(t, my1, mod1)

	mod1RepoUpdated := false
	defer func() { tfetchPostHook = nil }()
	tfetchPostHook = func(repo string) {
		if repo != mod1 + "/dir/hello.git" {
			return
		}
		if mod1RepoUpdated {
			t.Fatal("hello.git post fetch called twice")
		}

		hold := xgitSha1(ctx, "--git-dir="+repo, "rev-parse", "--verify", "refs/test/ref-to-blob")
		hnew := xgitSha1(ctx, "--git-dir="+repo, "hash-object", "-w", "--stdin", RunWith{stdin: "new blob"})
		if hnewOk := XSha1("cb8d6bb5e54b1c7159698442057416473a3b5385"); hnew != hnewOk {
			t.Fatalf("object for new blob did not hash correctly: have: %s  ; want: %s", hnew, hnewOk)
		}
		xgit(ctx, "--git-dir="+repo, "update-ref", "refs/test/ref-to-blob", hnew, hold)
		hnew_ := xgitSha1(ctx, "--git-dir="+repo, "rev-parse", "--verify", "refs/test/ref-to-blob")
		if hnew_ != hnew {
			t.Fatalf("ref-to-blob did not update correctly: have %s  ; want %s", hnew_, hnew)
		}

		mod1RepoUpdated = true
	}

	cmd_pull(ctx, gb, []string{mod1 + ":b1"})
	afterPull()

	tfetchPostHook = nil
	if !mod1RepoUpdated {
		t.Fatal("mod1/hello.git: hook to update it right after fetch was not ran")
	}

	h3 := xgitSha1(ctx, "rev-parse", "HEAD")
	if h3 == h2 {
		t.Fatal("pull: third run did not adjusted HEAD")
	}
	δ23 := xgit(ctx, "diff", h2, h3)
	if δ23 != "" {
		t.Fatalf("pull: third run was not noop: δ:\n%s", δ23)
	}


	// restore backup
	work1 := workdir + "/1"
	cmd_restore(ctx, gb, []string{"HEAD", "b1:" + work1})

	// verify files restored to the same as original
	gerr, diff, _ := ggit(ctx, "diff", "--no-index", "--raw", "--exit-code", my1, work1)
	// 0 - no diff, 1 - has diff, 2 - problem
	if gerr != nil && gerr.Sys().(syscall.WaitStatus).ExitStatus() > 1 {
		t.Fatal(gerr)
	}
	gitObjectsAndRefsRe := regexp.MustCompile(`\.git/(objects/|refs/|packed-refs|reftable/)`)
	for _, diffline := range strings.Split(diff, "\n") {
		// :srcmode dstmode srcsha1 dstsha1 status\tpath
		_, path, err := xstrings.HeadTail(diffline, "\t")
		if err != nil {
			t.Fatalf("restorecheck: cannot parse diff line %q", diffline)
		}
		// git objects and refscan be represented differently (we check them later)
		if gitObjectsAndRefsRe.FindString(path) != "" {
			continue
		}
		t.Fatal("restorecheck: unexpected diff:", diffline)
	}

	// verify git objects and refs restored to the same as original
	err = filepath.Walk(my1, func(path string, info os.FileInfo, err error) error {
		// any error -> stop
		if err != nil {
			return err
		}

		// non *.git/ or nongit.git/ -- not interesting
		if !(info.IsDir() && strings.HasSuffix(path, ".git")) || info.Name() == "nongit.git" {
			return nil
		}

		// found git repo - check refs & objects in original and restored are exactly the same,
		var R = [2]struct{ path, reflist, revlist string }{
			{path: path},                       // original
			{path: reprefix(my1, work1, path)}, // restored
		}

		for _, repo := range R {
			// fsck just in case
			xgit(ctx, "--git-dir="+repo.path, "fsck")
			// NOTE for-each-ref sorts output by refname
			repo.reflist = xgit(ctx, "--git-dir="+repo.path, "for-each-ref")
			// NOTE rev-list emits objects in reverse chronological order,
			//      starting from refs roots which are also ordered by refname
			repo.revlist = xgit(ctx, "--git-dir="+repo.path, "rev-list", "--all", "--objects")
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
		defer exc.Catch(func(e *exc.Error) {
			// it ok - pull should raise

			// git-backup should not leave backup repo locked on error
			xnoref("backup.locked")
		})

		cmd_pull(ctx, gb, []string{my2 + ":b2"})
		t.Fatal("pull corrupt.git: did not complain")
	}()

	// now try to pull repo where `git pack-objects` misbehaves
	my3 := mydir + "/testdata/3"
	checkIncompletePack := func(kind, errExpect string) {
		errExpectRe := regexp.MustCompile(errExpect)
		defer exc.Catch(func(e *exc.Error) {
			estr := e.Error()
			bad := ""
			badf := func(format string, argv ...interface{}) {
				bad += fmt.Sprintf(format+"\n", argv...)
			}

			if errExpectRe.FindString(estr) == "" {
				badf("- no %q", errExpect)
			}

			if bad != "" {
				t.Fatalf("pull incomplete-send-pack.git/%s: complained, but error is wrong:\n%s\nerror: %s", kind, bad, estr)
			}

			// git-backup should not leave backup repo locked on error
			xnoref("backup.locked")
		})

		// for incomplete-send-pack.git to indeed send incomplete pack, its git
		// config has to be activated via tweaked $HOME.
		home, ok := os.LookupEnv("HOME")
		defer func() {
			if ok {
				err = os.Setenv("HOME", home)
			} else {
				err = os.Unsetenv("HOME")
			}
			exc.Raiseif(err)
		}()
		err = os.Setenv("HOME", my3+"/incomplete-send-pack.git/"+kind)
		exc.Raiseif(err)

		cmd_pull(ctx, gb, []string{my3 + ":b3"})
		t.Fatalf("pull incomplete-send-pack.git/%s: did not complain", kind)
	}

	// missing blob: should be caught by git itself, because unpack-objects (git < 2.31)
	// or index-pack (git ≥ 2.31) perform full reachability checks of fetched tips.
	checkIncompletePack("x-missing-blob", "fatal: (unpack-objects|index-pack)")

	// missing commit: remote sends a pack that is closed under reachability,
	// but it has objects starting from only parent of requested tip. This way
	// e.g. commit at tip itself is not sent and the fact that it is missing in
	// the pack is not caught by fetch-pack. git-backup has to detect the
	// problem itself.
	checkIncompletePack("x-commit-send-parent", "remote did not send all neccessary objects")

	// pulling incomplete-send-pack.git without pack-objects hook must succeed:
	// without $HOME tweaks full and complete pack is sent.
	cmd_pull(ctx, gb, []string{my3 + ":b3"})
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


// blob_to_file used to corrupt memory if GC triggers inside it
func init() {
	tblob_to_file_mid_hook = func() {
		runtime.GC()
	}
}
