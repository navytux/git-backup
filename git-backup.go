// Copyright (C) 2015-2016  Nexedi SA and Contributors.
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
Git-backup - Backup set of Git repositories & just files; efficiently

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
    "flag"
    "fmt"
    "io/ioutil"
    "os"
    pathpkg "path"
    "path/filepath"
    "runtime"
    "runtime/debug"
    "sort"
    "strings"
    "sync"
    "syscall"
    "time"

    "lab.nexedi.com/kirr/go123/exc"
    "lab.nexedi.com/kirr/go123/mem"
    "lab.nexedi.com/kirr/go123/my"
    "lab.nexedi.com/kirr/go123/xerr"
    "lab.nexedi.com/kirr/go123/xflag"
    "lab.nexedi.com/kirr/go123/xstrings"

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
func obj_represent_as_commit(g *git.Repository, sha1 Sha1, obj_type git.ObjectType) Sha1 {
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
        return zcommit_tree(mktree_empty(), []Sha1{tagged_sha1}, obj_encoded)
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
        tree_for_blob := xgitSha1("mktree", RunWith{stdin: fmt.Sprintf("100644 blob %s\ttagged\n", tagged_sha1)})
        return zcommit_tree(tree_for_blob, []Sha1{}, obj_encoded)
    }

    // Tag₂       ~>     Commit₂*
    //  |                 .msg:      Tag₂
    //  v                 .tree   -> ø
    // Tag₁               .parent -> Commit₁*
    if tagged_type == git.ObjectTag {
        commit1 := obj_represent_as_commit(g, tagged_sha1, tagged_type)
        return zcommit_tree(mktree_empty(), []Sha1{commit1}, obj_encoded)
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
    xraise  := func(info interface{})           { exc.Raise(&RecreateObjError{commit_sha1, info}) }
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

func cmd_pull(gb *git.Repository, argv []string) {
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

    cmd_pull_(gb, pullspecv)
}

// info about ref pointing to sha1
type Ref struct {
    ref  string
    sha1 Sha1
}

func cmd_pull_(gb *git.Repository, pullspecv []PullSpec) {
    // while pulling, we'll keep refs from all pulled repositories under temp
    // unique work refs namespace.
    backup_time := time.Now().Format("20060102-1504")               // %Y%m%d-%H%M
    backup_refs_work := fmt.Sprintf("refs/backup/%s/", backup_time) // refs/backup/20150820-2109/
    backup_lock := "refs/backup.locked"

    // make sure another `git-backup pull` is not running
    xgit("update-ref", backup_lock, mktree_empty(), Sha1{})

    // make sure there is root commit
    gerr, _, _ := ggit("rev-parse", "--verify", "HEAD")
    if gerr != nil {
        infof("# creating root commit")
        // NOTE `git commit` does not work in bare repo - do commit by hand
        commit := xcommit_tree(gb, mktree_empty(), []Sha1{}, "Initialize git-backup repository")
        xgit("update-ref", "-m", "git-backup pull init", "HEAD", commit)
    }

    // walk over specified dirs, pulling objects from git and blobbing non-git-object files
    blobbedv := []string{} // info about file pulled to blob, and not yet added to index
    for _, __ := range pullspecv {
        dir, prefix := __.dir, __.prefix

        // make sure index is empty for prefix (so that we start from clean
        // prefix namespace and this way won't leave stale removed things)
        xgit("rm", "--cached", "-r", "--ignore-unmatch", "--", prefix)

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
            // NOTE --no-tags : do not try to autoextend commit -> covering tag
            // NOTE fetch.fsckObjects=true : check objects for corruption as they are fetched
            xgit("-c", "fetch.fsckObjects=true",
                 "fetch", "--no-tags", path,
                  fmt.Sprintf("refs/*:%s%s/*", backup_refs_work,
                        // NOTE repo name is escaped as it can contain e.g. spaces, and refs must not
                        path_refescape(reprefix(dir, prefix, path))),
                        // TODO do not show which ref we pulled - show only pack transfer progress
                        RunWith{stderr: gitprogress()})

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
    xgit("update-index", "--add", "--index-info", RunWith{stdin: strings.Join(blobbedv, "\n")})

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
    backup_refs_dump := xgit("for-each-ref", backup_refs_work)
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
                sha1_ = obj_represent_as_commit(gb, sha1, obj_type)
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
    backup_refs_sha1 := xgitSha1("hash-object", "-w", "--stdin", RunWith{stdin: backup_refs})

    // add backup_refs blob to index
    xgit("update-index", "--add", "--cacheinfo", fmt.Sprintf("100644,%s,backup.refs", backup_refs_sha1))

    // index is ready - prepare tree and commit
    backup_tree_sha1 := xgitSha1("write-tree")

    HEAD := xgitSha1("rev-parse", "HEAD")
    commit_sha1 := xcommit_tree(gb, backup_tree_sha1, append([]Sha1{HEAD}, backup_refs_parentv...),
            "Git-backup " + backup_time)

    xgit("update-ref", "-m", "git-backup pull", "HEAD", commit_sha1, HEAD)

    // remove no-longer needed backup refs & verify they don't stay
    backup_refs_delete := ""
    for _, __ := range backup_refs_list {
        backup_refs_delete += fmt.Sprintf("delete %s %s\n", __.ref, __.sha1)
    }

    xgit("update-ref", "--stdin", RunWith{stdin: backup_refs_delete})
    __ := xgit("for-each-ref", backup_refs_work)
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
    //       We can avoid quadratic behaviour via removing refs from just
    //       pulled repo right after the pull.
    gitdir := xgit("rev-parse", "--git-dir")
    err := os.RemoveAll(gitdir+"/"+backup_refs_work)
    exc.Raiseif(err) // NOTE err is nil if path does not exist

    // if we have working copy - update it
    bare := xgit("rev-parse", "--is-bare-repository")
    if bare != "true" {
        // `git checkout-index -af`  -- does not delete deleted files
        // `git read-tree -v -u --reset HEAD~ HEAD`  -- needs index matching
        // original worktree to properly work, but we already have updated index
        //
        // so we get changes we committed as diff and apply to worktree
        diff := xgit("diff", "--binary", HEAD, "HEAD", RunWith{raw: true})
        if diff != "" {
            diffstat := xgit("apply", "--stat", "--apply", "--binary", "--whitespace=nowarn",
                            RunWith{stdin: diff, raw: true})
            infof("%s", diffstat)
        }
    }

    // we are done - unlock
    xgit("update-ref", "-d", backup_lock)
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

func cmd_restore(gb *git.Repository, argv []string) {
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

    cmd_restore_(gb, HEAD, restorespecv)
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

// ref entry in 'backup.refs'   (repo prefix stripped)
type BackupRef struct {
    refname string // ref without "refs/" prefix
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
func (br ByRefname) Less(i, j int) bool { return strings.Compare(br[i].refname, br[j].refname) < 0 }

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
    return sort.Search(len(br), func (i int) bool {
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

func cmd_restore_(gb *git.Repository, HEAD_ string, restorespecv []RestoreSpec) {
    HEAD := xgitSha1("rev-parse", "--verify", HEAD_)

    // read backup refs index
    repotab := map[string]*BackupRepo{} // repo.path -> repo
    backup_refs := xgit("cat-file", "blob", fmt.Sprintf("%s:backup.refs", HEAD))
    for _, refentry := range xstrings.SplitLines(backup_refs, "\n") {
        // sha1 prefix+refname (sha1_)
        badentry := func() { exc.Raisef("E: invalid backup.refs entry: %q", refentry) }
        refentryv := strings.Fields(refentry)
        if !(2 <= len(refentryv) && len(refentryv) <= 3) {
            badentry()
        }
        sha1, err := Sha1Parse(refentryv[0])
        sha1_, err_ := sha1, err
        if len(refentryv) == 3 {
            sha1_, err_ = Sha1Parse(refentryv[2])
        }
        if err != nil || err_ != nil {
            badentry()
        }
        reporef := refentryv[1]
        repopath, ref := reporef_split(reporef)

        repo := repotab[repopath]
        if repo == nil {
            repo = &BackupRepo{repopath, RefMap{}}
            repotab[repopath] = repo
        }

        if _, alreadyin := repo.refs[ref]; alreadyin {
            exc.Raisef("E: duplicate ref %s in backup.refs", reporef)
        }
        repo.refs[ref] = BackupRefSha1{sha1, sha1_}
    }

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
    errch  := make(chan error)                   // errors from workers
    stopch := make(chan struct{})                // broadcasts restore has to be cancelled
    wg := sync.WaitGroup{}

    // main worker: walk over specified prefixes restoring files and
    // scheduling pack extraction requests from *.git -> packxq
    wg.Add(1)
    go func() {
        defer wg.Done()
        defer close(packxq)
        // raised err -> errch
        here := my.FuncName()
        defer exc.Catch(func(e *exc.Error) {
            errch <- exc.Addcallingcontext(here, e)
        })

    runloop:
        for _, __ := range restorespecv {
            prefix, dir := __.prefix, __.dir

            // ensure dir did not exist before restore run
            err := os.Mkdir(dir, 0777)
            exc.Raiseif(err)

            // files
            lstree := xgit("ls-tree", "--full-tree", "-r", "-z", "--", HEAD, prefix, RunWith{raw: true})
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
                                prefix: prefix}:

                case <-stopch:
                    break runloop
                }
            }
        }
    }()

    // pack workers: packxq -> extract packs
    for i := 0; i < njobs; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            // raised err -> errch
            here := my.FuncName()
            defer exc.Catch(func(e *exc.Error) {
                errch <- exc.Addcallingcontext(here, e)
            })

        runloop:
            for {
                select {
                case <-stopch:
                    break runloop

                case p, ok := <-packxq:
                    if !ok {
                        break runloop
                    }
                    infof("# git  %s\t-> %s", p.prefix, p.repopath)

                    // extract pack for that repo from big backup pack + decoded tags
                    pack_argv := []string{
                            "-c", "pack.threads=1", // occupy only 1 CPU + it packs better
                            "pack-objects",
                            "--revs", // include all objects referencable from input sha1 list
                            "--reuse-object", "--reuse-delta", "--delta-base-offset"}
                    if verbose <= 0 {
                        pack_argv = append(pack_argv, "-q")
                    }
                    pack_argv = append(pack_argv, p.repopath+"/objects/pack/pack")

                    xgit2(pack_argv, RunWith{stdin: p.refs.Sha1HeadsStr(), stderr: gitprogress()})

                    // verify that extracted repo refs match backup.refs index after extraction
                    x_ref_list := xgit("--git-dir=" + p.repopath,
                                       "for-each-ref", "--format=%(objectname) %(refname)")
                    repo_refs := p.refs.Values()
                    sort.Sort(ByRefname(repo_refs))
                    repo_ref_listv := make([]string, 0, len(repo_refs))
                    for _, __ := range repo_refs {
                        repo_ref_listv = append(repo_ref_listv, fmt.Sprintf("%s refs/%s", __.sha1, __.refname))
                    }
                    repo_ref_list := strings.Join(repo_ref_listv, "\n")
                    if x_ref_list != repo_ref_list {
                        exc.Raisef("E: extracted %s refs corrupt", p.repopath)
                    }

                    // check connectivity in recreated repository.
                    //
                    // This way we verify that extracted pack indeed contains all
                    // objects for all refs in the repo.
                    //
                    // Compared to fsck we do not re-compute sha1 sum of objects which
                    // is significantly faster.
                    gerr, _, _ := ggit("--git-dir=" + p.repopath,
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
        }()
    }

    // wait for workers to finish & collect/reraise their errors
    go func() {
        wg.Wait()
        close(errch)
    }()

    ev := xerr.Errorv{}
    for e := range errch {
        // tell everything to stop on first error
        if len(ev) == 0 {
            close(stopch)
        }
        ev = append(ev, e)
    }

    if len(ev) != 0 {
        exc.Raise(ev)
    }
}

var commands = map[string]func(*git.Repository, []string){
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

    // backup repository
    gb, err := git.OpenRepository(".")
    exc.Raiseif(err)

    cmd(gb, argv[1:])
}
