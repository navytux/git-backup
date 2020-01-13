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

/*
Git-backup - Backup set of Git repositories & just files; efficiently.

This program backups files and set of bare Git repositories into one Git repository.
Files are copied to blobs and then added to tree under certain place, and for
Git repositories, all reachable objects are pulled in with maintaining index
which remembers reference -> sha1 for every pulled repositories.

After objects from backuped Git repositories are pulled in, we create new
commit which references tree with changed backup index and files, and also has
all head objects from pulled-in repositories in its parents(*). This way backup
has history and all pulled objects become reachable from single head commit in
backup repository. In particular that means that the whole state of backup can
be described with only single sha1, and that backup repository itself could be
synchronized via standard git pull/push, be repacked, etc.

Restoration process is the opposite - from a particular backup state, files are
extracted at a proper place, and for Git repositories a pack with all objects
reachable from that repository heads is prepared and extracted from backup
repository object database.

This approach allows to leverage Git's good ability for object contents
deduplication and packing, especially for cases when there are many hosted
repositories which are forks of each other with relatively minor changes in
between each other and over time, and mostly common base. In author experience
the size of backup is dramatically smaller compared to straightforward "let's
tar it all" approach.

Data for all backuped files and repositories can be accessed if one has access
to backup repository, so either they all should be in the same security domain,
or extra care has to be taken to protect access to backup repository.

File permissions are not managed with strict details due to inherent
nature of Git. This aspect can be improved with e.g. etckeeper-like
(http://etckeeper.branchable.com/) approach if needed.

Please see README.rst with user-level overview on how to use git-backup.

NOTE the idea of pulling all refs together is similar to git-namespaces
     http://git-scm.com/docs/gitnamespaces

(*) Tag objects are handled specially - because in a lot of places Git insists and
    assumes commit parents can only be commit objects. We encode tag objects in
    specially-crafted commit object on pull, and decode back on backup restore.

    We do likewise if a ref points to tree or blob, which is valid in Git.
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"lab.nexedi.com/kirr/go123/exc"
	"lab.nexedi.com/kirr/go123/mem"
	"lab.nexedi.com/kirr/go123/my"
	"lab.nexedi.com/kirr/go123/xerr"
	"lab.nexedi.com/kirr/go123/xflag"
	"lab.nexedi.com/kirr/go123/xstrings"
	"lab.nexedi.com/kirr/go123/xsync"

	git "github.com/libgit2/git2go"
)

// verbose output
// 0 - silent
// 1 - info
// 2 - progress of long-running operations
// 3 - debug
var verbose = 1

func infof(format string, a ...interface{}) {
	if verbose > 0 {
		fmt.Printf(format, a...)
		fmt.Println()
	}
}

// what to pass to git subprocess to stdout/stderr
// DontRedirect - no-redirection, PIPE - output to us
func gitprogress() StdioRedirect {
	if verbose > 1 {
		return DontRedirect
	}
	return PIPE
}

func debugf(format string, a ...interface{}) {
	if verbose > 2 {
		fmt.Printf(format, a...)
		fmt.Println()
	}
}

// how many max jobs to spawn
var njobs = runtime.NumCPU()

// -------- create/extract blob --------

// file -> blob_sha1, mode
func file_to_blob(g *git.Repository, path string) (Sha1, uint32) {
	var blob_content []byte

	// because we want to pass mode to outside world (to e.g. `git update-index`)
	// we need to get native OS mode, not translated one as os.Lstat() would give us.
	var st syscall.Stat_t
	err := syscall.Lstat(path, &st)
	if err != nil {
		exc.Raise(&os.PathError{"lstat", path, err})
	}

	if st.Mode&syscall.S_IFMT == syscall.S_IFLNK {
		__, err := os.Readlink(path)
		blob_content = mem.Bytes(__)
		exc.Raiseif(err)
	} else {
		blob_content, err = ioutil.ReadFile(path)
		exc.Raiseif(err)
	}

	blob_sha1, err := WriteObject(g, blob_content, git.ObjectBlob)
	exc.Raiseif(err)

	return blob_sha1, st.Mode
}

// blob_sha1, mode -> file
func blob_to_file(g *git.Repository, blob_sha1 Sha1, mode uint32, path string) {
	blob, err := ReadObject(g, blob_sha1, git.ObjectBlob)
	exc.Raiseif(err)
	blob_content := blob.Data()

	err = os.MkdirAll(pathpkg.Dir(path), 0777)
	exc.Raiseif(err)

	if mode&syscall.S_IFMT == syscall.S_IFLNK {
		err = os.Symlink(mem.String(blob_content), path)
		exc.Raiseif(err)
	} else {
		// NOTE mode is native - we cannot use ioutil.WriteFile() directly
		err = writefile(path, blob_content, mode)
		exc.Raiseif(err)
	}
}

// -------- tags representation --------

// represent tag/tree/blob as specially crafted commit
//
// The reason we do this is that we want refs/tag/* to be parents of synthetic
// backup commit, but git does not allow tag objects to be in commit parents.
// Also besides commit and tag, it is possible for a ref to point to a tree or blob.
//
// We always attach original tagged object to crafted commit in one way or
// another, so that on backup restore we only have to recreate original tag
// object and tagged object is kept there in repo thanks to it being reachable
// through created commit.
func obj_represent_as_commit(ctx context.Context, g *git.Repository, sha1 Sha1, obj_type git.ObjectType) Sha1 {
	switch obj_type {
	case git.ObjectTag, git.ObjectTree, git.ObjectBlob:
		// ok
	default:
		exc.Raisef("%s (%s): cannot encode as commit", sha1, obj_type)
	}

	// first line in commit msg = object type
	obj_encoded := gittypestr(obj_type) + "\n"
	var tagged_type git.ObjectType
	var tagged_sha1 Sha1

	// below the code layout is mainly for tag type, and we hook tree and blob
	// types handling into that layout
	if obj_type == git.ObjectTag {
		tag, tag_obj := xload_tag(g, sha1)
		tagged_type = tag.tagged_type
		tagged_sha1 = tag.tagged_sha1
		obj_encoded += mem.String(tag_obj.Data())
	} else {
		// for tree/blob we only care that object stays reachable
		tagged_type = obj_type
		tagged_sha1 = sha1
	}

	// all commits we do here - we do with fixed name/date, so transformation
	// tag->commit is stable wrt git environment and time change
	fixed := AuthorInfo{Name: "Git backup", Email: "git@backup.org", When: time.Unix(0, 0).UTC()}
	zcommit_tree := func(tree Sha1, parents []Sha1, msg string) Sha1 {
		return xcommit_tree2(g, tree, parents, msg, fixed, fixed)
	}

	// Tag        ~>     Commit*
	//  |                 .msg:      Tag
	//  v                 .tree   -> ø
	// Commit             .parent -> Commit
	if tagged_type == git.ObjectCommit {
		return zcommit_tree(mktree_empty(ctx), []Sha1{tagged_sha1}, obj_encoded)
	}

	// Tag        ~>     Commit*
	//  |                 .msg:      Tag
	//  v                 .tree   -> Tree
	// Tree               .parent -> ø
	if tagged_type == git.ObjectTree {
		return zcommit_tree(tagged_sha1, []Sha1{}, obj_encoded)
	}

	// Tag        ~>     Commit*
	//  |                 .msg:      Tag
	//  v                 .tree   -> Tree* "tagged" -> Blob
	// Blob               .parent -> ø
	if tagged_type == git.ObjectBlob {
		tree_for_blob := xgitSha1(ctx, "mktree", RunWith{stdin: fmt.Sprintf("100644 blob %s\ttagged\n", tagged_sha1)})
		return zcommit_tree(tree_for_blob, []Sha1{}, obj_encoded)
	}

	// Tag₂       ~>     Commit₂*
	//  |                 .msg:      Tag₂
	//  v                 .tree   -> ø
	// Tag₁               .parent -> Commit₁*
	if tagged_type == git.ObjectTag {
		commit1 := obj_represent_as_commit(ctx, g, tagged_sha1, tagged_type)
		return zcommit_tree(mktree_empty(ctx), []Sha1{commit1}, obj_encoded)
	}

	exc.Raisef("%s (%q): unknown tagged type", sha1, tagged_type)
	panic(0)
}

// recreate tag/tree/blob from specially crafted commit
// (see obj_represent_as_commit() about how a objects are originally translated into commit)
// returns:
//   - tag:       recreated object sha1
//   - tree/blob: null sha1
func obj_recreate_from_commit(g *git.Repository, commit_sha1 Sha1) Sha1 {
	xraise := func(info interface{}) { exc.Raise(&RecreateObjError{commit_sha1, info}) }
	xraisef := func(f string, a ...interface{}) { xraise(fmt.Sprintf(f, a...)) }

	commit, err := g.LookupCommit(commit_sha1.AsOid())
	if err != nil {
		xraise(err)
	}
	if commit.ParentCount() > 1 {
		xraise(">1 parents")
	}

	obj_type, obj_raw, err := xstrings.HeadTail(commit.Message(), "\n")
	if err != nil {
		xraise("invalid encoded format")
	}
	switch obj_type {
	case "tag", "tree", "blob":
		// ok
	default:
		xraisef("unexpected encoded object type %q", obj_type)
	}

	// for tree/blob we do not need to do anything - that objects were reachable
	// from commit and are present in git db.
	if obj_type == "tree" || obj_type == "blob" {
		return Sha1{}
	}

	// re-create tag object
	tag_sha1, err := WriteObject(g, mem.Bytes(obj_raw), git.ObjectTag)
	exc.Raiseif(err)

	// the original tagged object should be already in repository, because we
	// always attach it to encoding commit one way or another,
	// except we need to recurse, if it was Tag₂->Tag₁
	tag, err := tag_parse(obj_raw)
	if err != nil {
		xraisef("encoded tag: %s", err)
	}
	if tag.tagged_type == git.ObjectTag {
		if commit.ParentCount() == 0 {
			xraise("encoded tag corrupt (tagged is tag but []parent is empty)")
		}
		obj_recreate_from_commit(g, Sha1FromOid(commit.ParentId(0)))
	}

	return tag_sha1
}

type RecreateObjError struct {
	commit_sha1 Sha1
	info        interface{}
}

func (e *RecreateObjError) Error() string {
	return fmt.Sprintf("commit %s: %s", e.commit_sha1, e.info)
}

// -------- git-backup pull --------

func cmd_pull_usage() {
	fmt.Fprint(os.Stderr,
`git-backup pull <dir1>:<prefix1> <dir2>:<prefix2> ...

Pull bare Git repositories & just files from dir1 into backup prefix1,
from dir2 into backup prefix2, etc...
`)
}

type PullSpec struct {
	dir, prefix string
}

func cmd_pull(ctx context.Context, gb *git.Repository, argv []string) {
	flags := flag.FlagSet{Usage: cmd_pull_usage}
	flags.Init("", flag.ExitOnError)
	flags.Parse(argv)

	argv = flags.Args()
	if len(argv) < 1 {
		cmd_pull_usage()
		os.Exit(1)
	}

	pullspecv := []PullSpec{}
	for _, arg := range argv {
		dir, prefix, err := xstrings.Split2(arg, ":")
		if err != nil {
			fmt.Fprintf(os.Stderr, "E: invalid pullspec %q\n", arg)
			cmd_pull_usage()
			os.Exit(1)
		}

		pullspecv = append(pullspecv, PullSpec{dir, prefix})
	}

	cmd_pull_(ctx, gb, pullspecv)
}

// Ref is info about a reference pointing to sha1.
type Ref struct {
	name string // reference name without "refs/" prefix
	sha1 Sha1
}

func cmd_pull_(ctx context.Context, gb *git.Repository, pullspecv []PullSpec) {
	// while pulling, we'll keep refs from all pulled repositories under temp
	// unique work refs namespace.
	backup_time := time.Now().Format("20060102-1504")               // %Y%m%d-%H%M
	backup_refs_work := fmt.Sprintf("refs/backup/%s/", backup_time) // refs/backup/20150820-2109/

	// prevent another `git-backup pull` from running simultaneously
	backup_lock := "refs/backup.locked"
	xgit(ctx, "update-ref", backup_lock, mktree_empty(ctx), Sha1{})
	defer xgit(context.Background(), "update-ref", "-d", backup_lock)

	// make sure there is root commit
	var HEAD Sha1
	var err error
	gerr, __, _ := ggit(ctx, "rev-parse", "--verify", "HEAD")
	if gerr != nil {
		infof("# creating root commit")
		// NOTE `git commit` does not work in bare repo - do commit by hand
		HEAD = xcommit_tree(gb, mktree_empty(ctx), []Sha1{}, "Initialize git-backup repository")
		xgit(ctx, "update-ref", "-m", "git-backup pull init", "HEAD", HEAD)
	} else {
		HEAD, err = Sha1Parse(__)
		exc.Raiseif(err)
	}

	// build index of "already-have" objects: all commits + tag/tree/blob that
	// were at heads of already pulled repositories.
	//
	// Build it once and use below to check ourselves whether a head from a pulled
	// repository needs to be actually fetched. If we don't, `git fetch-pack`
	// will do similar to "all commits" linear scan for every pulled repository,
	// which are many out there.
	alreadyHave := Sha1Set{}
	infof("# building \"already-have\" index")

	// already have: all commits
	//
	// As of lab.nexedi.com/20180612 there are ~ 1.7·10⁷ objects total in backup.
	// Of those there are ~ 1.9·10⁶ commit objects, i.e. ~10% of total.
	// Since 1 sha1 is 2·10¹ bytes, the space needed for keeping sha1 of all
	// commits is ~ 4·10⁷B = ~40MB. It is thus ok to keep this index in RAM for now.
	for _, __ := range xstrings.SplitLines(xgit(ctx, "rev-list", HEAD), "\n") {
		sha1, err := Sha1Parse(__)
		exc.Raiseif(err)
		alreadyHave.Add(sha1)
	}

	// already have: tag/tree/blob that were at heads of already pulled repositories
	//
	// As of lab.nexedi.com/20180612 there are ~ 8.4·10⁴ refs in total.
	// Of those encoded tag/tree/blob are ~ 3.2·10⁴, i.e. ~40% of total.
	// The number of tag/tree/blob objects in alreadyHave is thus negligible
	// compared to the number of "all commits".
	hcommit, err := gb.LookupCommit(HEAD.AsOid())
	exc.Raiseif(err)
	htree, err := hcommit.Tree()
	exc.Raiseif(err)
	if htree.EntryByName("backup.refs") != nil {
		repotab, err := loadBackupRefs(ctx, fmt.Sprintf("%s:backup.refs", HEAD))
		exc.Raiseif(err)

		for _, repo := range repotab {
			for _, xref := range repo.refs {
				if xref.sha1 != xref.sha1_ && !alreadyHave.Contains(xref.sha1) {
					// make sure encoded tag/tree/blob objects represented as
					// commits are present. We do so, because we promise to
					// fetch that all objects in alreadyHave are present.
					obj_recreate_from_commit(gb, xref.sha1_)

					alreadyHave.Add(xref.sha1)
				}
			}
		}
	}

	// walk over specified dirs, pulling objects from git and blobbing non-git-object files
	blobbedv := []string{} // info about file pulled to blob, and not yet added to index
	for _, __ := range pullspecv {
		dir, prefix := __.dir, __.prefix

		// make sure index is empty for prefix (so that we start from clean
		// prefix namespace and this way won't leave stale removed things)
		xgit(ctx, "rm", "--cached", "-r", "--ignore-unmatch", "--", prefix)

		here := my.FuncName()
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) (errout error) {
			if err != nil {
				if os.IsNotExist(err) {
					// a file or directory was removed in parallel to us scanning the tree.
					infof("Warning: Skipping %s: %s", path, err)
					return nil
				}
				// any other error -> stop
				return err
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}

			// propagate exceptions properly via filepath.Walk as errors with calling context
			// (filepath is not our code)
			defer exc.Catch(func(e *exc.Error) {
				errout = exc.Addcallingcontext(here, e)
			})

			// files -> blobs + queue info for adding blobs to index
			if !info.IsDir() {
				infof("# file %s\t<- %s", prefix, path)
				blob, mode := file_to_blob(gb, path)
				blobbedv = append(blobbedv,
					fmt.Sprintf("%o %s\t%s", mode, blob, reprefix(dir, prefix, path)))
				return nil
			}

			// directories -> look for *.git and handle git object specially.

			// do not recurse into *.git/objects/  - we'll save them specially
			if strings.HasSuffix(path, ".git/objects") {
				return filepath.SkipDir
			}

			// else we recurse, but handle *.git specially - via fetching objects from it
			if !strings.HasSuffix(path, ".git") {
				return nil
			}

			// git repo - let's pull all refs from it to our backup refs namespace
			infof("# git  %s\t<- %s", prefix, path)
			refv, _, err := fetch(ctx, path, alreadyHave)
			exc.Raiseif(err)

			// TODO don't store to git references all references from fetched repository:
			//
			//      We need to store to git references only references that were actually
			//      fetched - so that next fetch, e.g. from a fork that also has new data
			//      as its upstream, won't have to transfer what we just have fetched
			//      from upstream.
			//
			//      For this purpose we can also save references by naming them as their
			//      sha1, not actual name, which will automatically deduplicate them in
			//      between several repositories, especially when/if pull will be made to
			//      work in parallel.
			//
			//      Such changed-only deduplicated references should be O(δ) - usually only
			//      a few, and this way we will also automatically avoid O(n^2) behaviour
			//      of every git fetch scanning all local references at its startup.
			//
			//      For backup.refs, we can generate it directly from refv of all fetched
			//      repositories saved in RAM.
			reporefprefix := backup_refs_work +
				// NOTE repo name is escaped as it can contain e.g. spaces, and refs must not
				path_refescape(reprefix(dir, prefix, path))
			for _, ref := range refv {
				err = mkref(gb, reporefprefix+"/"+ref.name, ref.sha1)
				exc.Raiseif(err)
			}

			// XXX do we want to do full fsck of source git repo on pull as well ?

			return nil
		})

		// re-raise / raise error after Walk
		if err != nil {
			e := exc.Aserror(err)
			e = exc.Addcontext(e, "pulling from "+dir)
			exc.Raise(e)
		}
	}

	// add to index files we converted to blobs
	xgit(ctx, "update-index", "--add", "--index-info", RunWith{stdin: strings.Join(blobbedv, "\n")})

	// all refs from all found git repositories populated.
	// now prepare manifest with ref -> sha1 and do a synthetic commit merging all that sha1
	// (so they become all reachable from HEAD -> survive repack and be transferable on git pull)
	//
	// NOTE we handle tag/tree/blob objects specially - because these objects cannot
	// be in commit parents, we convert them to specially-crafted commits and use them.
	// The commits prepared contain full info how to restore original objects.

	// backup.refs format:
	//
	//   1eeb0324 <prefix>/wendelin.core.git/heads/master
	//   213a9243 <prefix>/wendelin.core.git/tags/v0.4 <213a9243-converted-to-commit>
	//   ...
	//
	// NOTE `git for-each-ref` sorts output by ref
	//      -> backup_refs is sorted and stable between runs
	backup_refs_dump := xgit(ctx, "for-each-ref", backup_refs_work)
	backup_refs_list := []Ref{}       // parsed dump
	backup_refsv := []string{}        // backup.refs content
	backup_refs_parents := Sha1Set{}  // sha1 for commit parents, obtained from refs
	noncommit_seen := map[Sha1]Sha1{} // {} sha1 -> sha1_ (there are many duplicate tags)
	for _, __ := range xstrings.SplitLines(backup_refs_dump, "\n") {
		sha1, type_, ref := Sha1{}, "", ""
		_, err := fmt.Sscanf(__, "%s %s %s\n", &sha1, &type_, &ref)
		if err != nil {
			exc.Raisef("%s: strange for-each-ref entry %q", backup_refs_work, __)
		}
		backup_refs_list = append(backup_refs_list, Ref{ref, sha1})
		backup_refs_entry := fmt.Sprintf("%s %s", sha1, strip_prefix(backup_refs_work, ref))

		// represent tag/tree/blob as specially crafted commit, because we
		// cannot use it as commit parent.
		sha1_ := sha1
		if type_ != "commit" {
			//infof("obj_as_commit %s  %s\t%s", sha1, type_, ref)  XXX
			var seen bool
			sha1_, seen = noncommit_seen[sha1]
			if !seen {
				obj_type, ok := gittype(type_)
				if !ok {
					exc.Raisef("%s: invalid git type in entry %q", backup_refs_work, __)
				}
				sha1_ = obj_represent_as_commit(ctx, gb, sha1, obj_type)
				noncommit_seen[sha1] = sha1_
			}

			backup_refs_entry += fmt.Sprintf(" %s", sha1_)
		}

		backup_refsv = append(backup_refsv, backup_refs_entry)

		if !backup_refs_parents.Contains(sha1_) { // several refs can refer to the same sha1
			backup_refs_parents.Add(sha1_)
		}
	}

	backup_refs := strings.Join(backup_refsv, "\n")
	backup_refs_parentv := backup_refs_parents.Elements()
	sort.Sort(BySha1(backup_refs_parentv)) // so parents order is stable in between runs

	// backup_refs -> blob
	backup_refs_sha1 := xgitSha1(ctx, "hash-object", "-w", "--stdin", RunWith{stdin: backup_refs})

	// add backup_refs blob to index
	xgit(ctx, "update-index", "--add", "--cacheinfo", fmt.Sprintf("100644,%s,backup.refs", backup_refs_sha1))

	// index is ready - prepare tree and commit
	backup_tree_sha1 := xgitSha1(ctx, "write-tree")
	commit_sha1 := xcommit_tree(gb, backup_tree_sha1, append([]Sha1{HEAD}, backup_refs_parentv...),
		"Git-backup "+backup_time)

	xgit(ctx, "update-ref", "-m", "git-backup pull", "HEAD", commit_sha1, HEAD)

	// remove no-longer needed backup refs & verify they don't stay
	backup_refs_delete := ""
	for _, ref := range backup_refs_list {
		backup_refs_delete += fmt.Sprintf("delete %s %s\n", ref.name, ref.sha1)
	}

	xgit(ctx, "update-ref", "--stdin", RunWith{stdin: backup_refs_delete})
	__ = xgit(ctx, "for-each-ref", backup_refs_work)
	if __ != "" {
		exc.Raisef("Backup refs under %s not deleted properly", backup_refs_work)
	}

	// NOTE  `delete` deletes only files, but leaves empty dirs around.
	//       more important: this affect performance of future `git-backup pull` run a *LOT*
	//
	//       reason is: `git pull` first check local refs, and for doing so it
	//       recourse into all directories, even empty ones.
	//
	//       https://lab.nexedi.com/lab.nexedi.com/lab.nexedi.com/issues/4
	//
	//       So remove all dirs under backup_refs_work prefix in the end.
	//
	// TODO  Revisit this when reworking fetch to be parallel. Reason is: in
	//       the process of pulling repositories, the more references we
	//       accumulate, the longer pull starts to be, so it becomes O(n^2).
	//
	//       -> what to do is described nearby fetch/mkref call.
	gitdir := xgit(ctx, "rev-parse", "--git-dir")
	err = os.RemoveAll(gitdir + "/" + backup_refs_work)
	exc.Raiseif(err) // NOTE err is nil if path does not exist

	// if we have working copy - update it
	bare := xgit(ctx, "rev-parse", "--is-bare-repository")
	if bare != "true" {
		// `git checkout-index -af`  -- does not delete deleted files
		// `git read-tree -v -u --reset HEAD~ HEAD`  -- needs index matching
		// original worktree to properly work, but we already have updated index
		//
		// so we get changes we committed as diff and apply to worktree
		diff := xgit(ctx, "diff", "--binary", HEAD, "HEAD", RunWith{raw: true})
		if diff != "" {
			diffstat := xgit(ctx, "apply", "--stat", "--apply", "--binary", "--whitespace=nowarn",
				RunWith{stdin: diff, raw: true})
			infof("%s", diffstat)
		}
	}
}

// fetch makes sure all objects from a repository are present in backup place.
//
// It fetches objects that are potentially missing in backup from the
// repository in question. The objects considered to fetch are those, that are
// reachable from all repository references.
//
// AlreadyHave can be given to indicate knowledge on what objects our repository
// already has. If remote advertises tip with sha1 in alreadyHave, that tip won't be
// fetched. Notice: alreadyHave is consulted directly - no reachability scan is
// performed on it.
//
// All objects reachable from alreadyHave must be in our repository.
// AlreadyHave does not need to be complete - if we have something that is not
// in alreadyHave - it can affect only speed, not correctness.
//
// Returned are 2 lists of references from the source repository:
//
//  - list of all references, and
//  - list of references we actually had to fetch.
//
// Note: fetch does not create any local references - the references returned
// only describe state of references in fetched source repository.
func fetch(ctx context.Context, repo string, alreadyHave Sha1Set) (refv, fetchedv []Ref, err error) {
	defer xerr.Contextf(&err, "fetch %s", repo)

	// first check which references are advertised
	refv, err = lsremote(ctx, repo)
	if err != nil {
		return nil, nil, err
	}

	// check if we already have something
	var fetchv []Ref // references we need to actually fetch.
	for _, ref := range refv {
		if !alreadyHave.Contains(ref.sha1) {
			fetchv = append(fetchv, ref)
		}
	}

	// if there is nothing to fetch - we are done
	if len(fetchv) == 0 {
		return refv, fetchv, nil
	}

	// fetch by sha1 what we don't already have from advertised.
	//
	// even if refs would change after ls-remote but before here, we should be
	// getting exactly what was advertised.
	//
	// related link on the subject:
	// https://git.kernel.org/pub/scm/git/git.git/commit/?h=051e4005a3
	var argv []interface{}
	arg := func(v ...interface{}) { argv = append(argv, v...) }
	arg(
		// check objects for corruption as they are fetched
		"-c", "fetch.fsckObjects=true",
		"fetch-pack", "--thin",

		// force upload-pack to allow us asking any sha1 we want.
		// needed because advertised refs we got at lsremote time could have changed.
		"--upload-pack=git -c uploadpack.allowAnySHA1InWant=true"+
			// workarounds for git < 2.11.1, which does not have uploadpack.allowAnySHA1InWant:
			" -c uploadpack.allowTipSHA1InWant=true -c uploadpack.allowReachableSHA1InWant=true"+
			//
			" upload-pack",

		repo)
	for _, ref := range fetchv {
		arg(ref.sha1)
	}
	arg(RunWith{stderr: gitprogress()})

	gerr, _, _ := ggit(ctx, argv...)
	if gerr != nil {
		return nil, nil, gerr
	}

	// fetch-pack ran ok - now check that all fetched tips are indeed fully
	// connected and that we also have all referenced blob/tree objects. The
	// reason for this check is that source repository could send us a pack with
	// e.g. some objects missing and this way even if fetch-pack would report
	// success, chances could be we won't have all the objects we think we
	// fetched.
	//
	// when checking we assume that the roots we already have at all our
	// references are ok.
	//
	// related link on the subject:
	// https://git.kernel.org/pub/scm/git/git.git/commit/?h=6d4bb3833c
	argv = nil
	arg("rev-list", "--quiet", "--objects", "--not", "--all", "--not")
	for _, ref := range fetchv {
		arg(ref.sha1)
	}
	arg(RunWith{stderr: gitprogress()})

	gerr, _, _ = ggit(ctx, argv...)
	if gerr != nil {
		return nil, nil, fmt.Errorf("remote did not send all neccessary objects")
	}

	// fetched ok
	return refv, fetchv, nil
}

// lsremote lists all references advertised by repo.
func lsremote(ctx context.Context, repo string) (refv []Ref, err error) {
	defer xerr.Contextf(&err, "lsremote %s", repo)

	// NOTE --refs instructs to omit peeled refs like
	//
	//   c668db59ccc59e97ce81f769d9f4633e27ad3bdb refs/tags/v0.1
	//   4b6821f4a4e4c9648941120ccbab03982e33104f refs/tags/v0.1^{}  <--
	//
	// because fetch-pack errors on them:
	//
	//   https://public-inbox.org/git/20180610143231.7131-1-kirr@nexedi.com/
	//
	// we don't need to pull them anyway.
	gerr, stdout, _ := ggit(ctx, "ls-remote", "--refs", repo)
	if gerr != nil {
		return nil, gerr
	}

	//  oid refname
	//  oid refname
	//  ...
	for _, entry := range xstrings.SplitLines(stdout, "\n") {
		sha1, ref := Sha1{}, ""
		_, err := fmt.Sscanf(entry, "%s %s\n", &sha1, &ref)
		if err != nil {
			return nil, fmt.Errorf("strange output entry: %q", entry)
		}

		// Ref says its name goes without "refs/" prefix.
		if !strings.HasPrefix(ref, "refs/") {
			return nil, fmt.Errorf("non-refs/ reference: %q", ref)
		}
		ref = strings.TrimPrefix(ref, "refs/")

		refv = append(refv, Ref{ref, sha1})
	}

	return refv, nil
}

// -------- git-backup restore --------

func cmd_restore_usage() {
	fmt.Fprint(os.Stderr,
`git-backup restore <commit-ish> <prefix1>:<dir1> <prefix2>:<dir2> ...

Restore Git repositories & just files from backup prefix1 into dir1,
from backup prefix2 into dir2, etc...

Backup state to restore is taken from <commit-ish>.
`)
}

type RestoreSpec struct {
	prefix, dir string
}

func cmd_restore(ctx context.Context, gb *git.Repository, argv []string) {
	flags := flag.FlagSet{Usage: cmd_restore_usage}
	flags.Init("", flag.ExitOnError)
	flags.Parse(argv)

	argv = flags.Args()
	if len(argv) < 2 {
		cmd_restore_usage()
		os.Exit(1)
	}

	HEAD := argv[0]

	restorespecv := []RestoreSpec{}
	for _, arg := range argv[1:] {
		prefix, dir, err := xstrings.Split2(arg, ":")
		if err != nil {
			fmt.Fprintf(os.Stderr, "E: invalid restorespec %q\n", arg)
			cmd_restore_usage()
			os.Exit(1)
		}

		restorespecv = append(restorespecv, RestoreSpec{prefix, dir})
	}

	cmd_restore_(ctx, gb, HEAD, restorespecv)
}

// kirr/wendelin.core.git/heads/master -> kirr/wendelin.core.git, heads/master
// tiwariayush/Discussion%20Forum%20.git/... -> tiwariayush/Discussion Forum .git, ...
func reporef_split(reporef string) (repo, ref string) {
	dotgit := strings.Index(reporef, ".git/")
	if dotgit == -1 {
		exc.Raisef("E: %s is not a ref for a git repo", reporef)
	}
	repo, ref = reporef[:dotgit+4], reporef[dotgit+4+1:]
	repo, err := path_refunescape(repo) // unescape repo name we originally escaped when making backup
	exc.Raiseif(err)
	return repo, ref
}

// sha1 value(s) for a ref in 'backup.refs'
type BackupRefSha1 struct {
	sha1  Sha1 // original sha1 this ref was pointing to in original repo
	sha1_ Sha1 // sha1 actually used to represent sha1's object in backup repo
	           // (for tag/tree/blob - they are converted to commits)
}

// BackupRef represents 1 reference entry in 'backup.refs'   (repo prefix stripped)
type BackupRef struct {
	name string // reference name without "refs/" prefix
	BackupRefSha1
}

// {} refname -> sha1, sha1_
type RefMap map[string]BackupRefSha1

// info about a repository from backup.refs
type BackupRepo struct {
	repopath string // full repo path with backup prefix
	refs     RefMap
}

// all RefMap values as flat []BackupRef
func (m RefMap) Values() []BackupRef {
	ev := make([]BackupRef, 0, len(m))
	for ref, refsha1 := range m {
		ev = append(ev, BackupRef{ref, refsha1})
	}
	return ev
}

// for sorting []BackupRef by refname
type ByRefname []BackupRef

func (br ByRefname) Len() int           { return len(br) }
func (br ByRefname) Swap(i, j int)      { br[i], br[j] = br[j], br[i] }
func (br ByRefname) Less(i, j int) bool { return strings.Compare(br[i].name, br[j].name) < 0 }

// all sha1 heads RefMap points to, in sorted order
func (m RefMap) Sha1Heads() []Sha1 {
	hs := Sha1Set{}
	for _, refsha1 := range m {
		hs.Add(refsha1.sha1)
	}
	headv := hs.Elements()
	sort.Sort(BySha1(headv))
	return headv
}

// like Sha1Heads() but returns heads in text format delimited by "\n"
func (m RefMap) Sha1HeadsStr() string {
	s := ""
	for _, sha1 := range m.Sha1Heads() {
		s += sha1.String() + "\n"
	}
	return s
}

// for sorting []BackupRepo by repopath
type ByRepoPath []*BackupRepo

func (br ByRepoPath) Len() int           { return len(br) }
func (br ByRepoPath) Swap(i, j int)      { br[i], br[j] = br[j], br[i] }
func (br ByRepoPath) Less(i, j int) bool { return strings.Compare(br[i].repopath, br[j].repopath) < 0 }

// also for searching sorted []BackupRepo by repopath prefix
func (br ByRepoPath) Search(prefix string) int {
	return sort.Search(len(br), func(i int) bool {
		return strings.Compare(br[i].repopath, prefix) >= 0
	})
}

// request to extract a pack
type PackExtractReq struct {
	refs     RefMap // extract pack with objects from this heads
	repopath string // into repository located here

	// for info only: request was generated restoring from under this backup prefix
	prefix string
}

func cmd_restore_(ctx context.Context, gb *git.Repository, HEAD_ string, restorespecv []RestoreSpec) {
	HEAD := xgitSha1(ctx, "rev-parse", "--verify", HEAD_)

	// read backup refs index
	repotab, err := loadBackupRefs(ctx, fmt.Sprintf("%s:backup.refs", HEAD))
	exc.Raiseif(err)

	// flattened & sorted repotab
	// NOTE sorted - to process repos always in the same order & for searching
	repov := make([]*BackupRepo, 0, len(repotab))
	for _, repo := range repotab {
		repov = append(repov, repo)
	}
	sort.Sort(ByRepoPath(repov))

	// repotab no longer needed
	repotab = nil

	packxq := make(chan PackExtractReq, 2*njobs) // requests to extract packs
	wg := xsync.NewWorkGroup(ctx)

	// main worker: walk over specified prefixes restoring files and
	// scheduling pack extraction requests from *.git -> packxq
	wg.Go(func(ctx context.Context) (err error) {
		defer close(packxq)
		// raised err -> return
		here := my.FuncName()
		defer exc.Catch(func(e *exc.Error) {
			err = exc.Addcallingcontext(here, e)
		})

		for _, __ := range restorespecv {
			prefix, dir := __.prefix, __.dir

			// ensure dir did not exist before restore run
			err := os.Mkdir(dir, 0777)
			exc.Raiseif(err)

			// files
			lstree := xgit(ctx, "ls-tree", "--full-tree", "-r", "-z", "--", HEAD, prefix, RunWith{raw: true})
			repos_seen := StrSet{} // dirs of *.git seen while restoring files
			for _, __ := range xstrings.SplitLines(lstree, "\x00") {
				mode, type_, sha1, filename, err := parse_lstree_entry(__)
				// NOTE
				//  - `ls-tree -r` shows only leaf objects
				//  - git-backup repository does not have submodules and the like
				// -> type should be "blob" only
				if err != nil || type_ != "blob" {
					exc.Raisef("%s: invalid/unexpected ls-tree entry %q", HEAD, __)
				}

				exc.Raiseif(ctx.Err())

				filename = reprefix(prefix, dir, filename)
				infof("# file %s\t-> %s", prefix, filename)
				blob_to_file(gb, sha1, mode, filename)

				// make sure git will recognize *.git as repo:
				//   - it should have refs/{heads,tags}/ and objects/pack/ inside.
				//
				// NOTE doing it while restoring files, because a repo could be
				//   empty - without refs at all, and thus next "git packs restore"
				//   step will not be run for it.
				filedir := pathpkg.Dir(filename)
				if strings.HasSuffix(filedir, ".git") && !repos_seen.Contains(filedir) {
					infof("# repo %s\t-> %s", prefix, filedir)
					for _, __ := range []string{"refs/heads", "refs/tags", "objects/pack"} {
						err := os.MkdirAll(filedir+"/"+__, 0777)
						exc.Raiseif(err)
					}
					repos_seen.Add(filedir)
				}
			}

			// git packs
			for i := ByRepoPath(repov).Search(prefix); i < len(repov); i++ {
				repo := repov[i]
				if !strings.HasPrefix(repo.repopath, prefix) {
					break // repov is sorted - end of repositories with prefix
				}

				// make sure tag/tree/blob objects represented as commits are
				// present, before we generate pack for restored repo.
				// ( such objects could be lost e.g. after backup repo repack as they
				//   are not reachable from backup repo HEAD )
				for _, __ := range repo.refs {
					if __.sha1 != __.sha1_ {
						obj_recreate_from_commit(gb, __.sha1_)
					}
				}

				select {
				case packxq <- PackExtractReq{refs: repo.refs,
					repopath: reprefix(prefix, dir, repo.repopath),
					prefix:   prefix}:

				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}

		return nil
	})

	// pack workers: packxq -> extract packs
	for i := 0; i < njobs; i++ {
		wg.Go(func(ctx context.Context) (err error) {
			// raised err -> return
			here := my.FuncName()
			defer exc.Catch(func(e *exc.Error) {
				err = exc.Addcallingcontext(here, e)
			})

			for {
				select {
				case <-ctx.Done():
					return ctx.Err()

				case p, ok := <-packxq:
					if !ok {
						return nil
					}
					infof("# git  %s\t-> %s", p.prefix, p.repopath)

					// extract pack for that repo from big backup pack + decoded tags
					pack_argv := []string{
						"-c", "pack.threads=1", // occupy only 1 CPU + it packs better
						"pack-objects",
						"--revs", // include all objects referencable from input sha1 list
						"--reuse-object", "--reuse-delta", "--delta-base-offset",

						// use bitmap index from backup repo, if present (faster pack generation)
						// https://git.kernel.org/pub/scm/git/git.git/commit/?h=645c432d61
						"--use-bitmap-index",
					}
					if verbose <= 0 {
						pack_argv = append(pack_argv, "-q")
					}
					pack_argv = append(pack_argv, p.repopath+"/objects/pack/pack")

					xgit2(ctx, pack_argv, RunWith{stdin: p.refs.Sha1HeadsStr(), stderr: gitprogress()})

					// verify that extracted repo refs match backup.refs index after extraction
					x_ref_list := xgit(ctx, "--git-dir="+p.repopath,
						"for-each-ref", "--format=%(objectname) %(refname)")
					repo_refs := p.refs.Values()
					sort.Sort(ByRefname(repo_refs))
					repo_ref_listv := make([]string, 0, len(repo_refs))
					for _, ref := range repo_refs {
						repo_ref_listv = append(repo_ref_listv, fmt.Sprintf("%s refs/%s", ref.sha1, ref.name))
					}
					repo_ref_list := strings.Join(repo_ref_listv, "\n")
					if x_ref_list != repo_ref_list {
						// TODO show refs diff, not 2 dumps
						exc.Raisef("E: extracted %s refs corrupt:\n\nwant:\n%s\n\nhave:\n%s",
							p.repopath, repo_ref_list, x_ref_list)
					}

					// check connectivity in recreated repository.
					//
					// This way we verify that extracted pack indeed contains all
					// objects for all refs in the repo.
					//
					// Compared to fsck we do not re-compute sha1 sum of objects which
					// is significantly faster.
					gerr, _, _ := ggit(ctx, "--git-dir="+p.repopath,
						"rev-list", "--objects", "--stdin", "--quiet", RunWith{stdin: p.refs.Sha1HeadsStr()})
					if gerr != nil {
						fmt.Fprintln(os.Stderr, "E: Problem while checking connectivity of extracted repo:")
						exc.Raise(gerr)
					}

					// XXX disabled because it is slow
					// // NOTE progress goes to stderr, problems go to stdout
					// xgit("--git-dir=" + p.repopath, "fsck",
					//         # only check that traversal from refs is ok: this unpacks
					//         # commits and trees and verifies blob objects are there,
					//         # but do _not_ unpack blobs =fast.
					//         "--connectivity-only",
					//         RunWith{stdout: gitprogress(), stderr: gitprogress()})
				}
			}
		})
	}

	// wait for workers to finish & collect/reraise first error, if any
	err = wg.Wait()
	exc.Raiseif(err)
}

// loadBackupRefs loads 'backup.ref' content from a git object.
//
// an example of object is e.g. "HEAD:backup.ref".
func loadBackupRefs(ctx context.Context, object string) (repotab map[string]*BackupRepo, err error) {
	defer xerr.Contextf(&err, "load backup.refs %q", object)

	gerr, backup_refs, _ := ggit(ctx, "cat-file", "blob", object)
	if gerr != nil {
		return nil, gerr
	}

	repotab = make(map[string]*BackupRepo)
	for _, refentry := range xstrings.SplitLines(backup_refs, "\n") {
		// sha1 prefix+refname (sha1_)
		badentry := func() error { return fmt.Errorf("invalid entry: %q", refentry) }
		refentryv := strings.Fields(refentry)
		if !(2 <= len(refentryv) && len(refentryv) <= 3) {
			return nil, badentry()
		}
		sha1, err := Sha1Parse(refentryv[0])
		sha1_, err_ := sha1, err
		if len(refentryv) == 3 {
			sha1_, err_ = Sha1Parse(refentryv[2])
		}
		if err != nil || err_ != nil {
			return nil, badentry()
		}
		reporef := refentryv[1]
		repopath, ref := reporef_split(reporef)

		repo := repotab[repopath]
		if repo == nil {
			repo = &BackupRepo{repopath, RefMap{}}
			repotab[repopath] = repo
		}

		if _, alreadyin := repo.refs[ref]; alreadyin {
			return nil, fmt.Errorf("duplicate ref %q", ref)
		}
		repo.refs[ref] = BackupRefSha1{sha1, sha1_}
	}

	return repotab, nil
}

var commands = map[string]func(context.Context, *git.Repository, []string){
	"pull":    cmd_pull,
	"restore": cmd_restore,
}

func usage() {
	fmt.Fprintf(os.Stderr,
`git-backup [options] <command>

    pull        pull git-repositories and files to backup
    restore     restore git-repositories and files from backup

  common options:

    -h --help       this help text.
    -v              increase verbosity.
    -q              decrease verbosity.
    -j N            allow max N jobs to spawn; default=NPROC (%d on this system)
`, njobs)
}

func main() {
	flag.Usage = usage
	quiet := 0
	flag.Var((*xflag.Count)(&verbose), "v", "verbosity level")
	flag.Var((*xflag.Count)(&quiet), "q", "decrease verbosity")
	flag.IntVar(&njobs, "j", njobs, "allow max N jobs to spawn")
	flag.Parse()
	verbose -= quiet
	argv := flag.Args()

	if len(argv) == 0 {
		usage()
		os.Exit(1)
	}

	cmd := commands[argv[0]]
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "E: unknown command %q", argv[0])
		os.Exit(1)
	}

	// catch Error and report info from it
	here := my.FuncName()
	defer exc.Catch(func(e *exc.Error) {
		e = exc.Addcallingcontext(here, e)
		fmt.Fprintln(os.Stderr, e)

		// also show traceback if debug
		if verbose > 2 {
			fmt.Fprint(os.Stderr, "\n")
			debug.PrintStack()
		}

		os.Exit(1)
	})

	// cancel what we'll do on SIGINT | SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigq := make(chan os.Signal, 1)
	signal.Notify(sigq, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ctx.Done():
		case <-sigq:
			cancel()
		}
	}()

	// backup repository
	gb, err := git.OpenRepository(".")
	exc.Raiseif(err)

	cmd(ctx, gb, argv[1:])
}
