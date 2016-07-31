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

// Git-backup | Git object: Blob Tree Commit Tag
package main

import (
    "errors"
    "fmt"
    "os"
    "strings"

    git "github.com/libgit2/git2go"
)

// read/write raw objects
func ReadObject(g *git.Repository, sha1 Sha1, objtype git.ObjectType) (*git.OdbObject, error) {
    obj, err := ReadObject2(g, sha1)
    if err != nil {
        return nil, err
    }
    if objtype != obj.Type() {
        return nil, &UnexpectedObjType{obj, objtype}
    }
    return obj, nil
}

func ReadObject2(g *git.Repository, sha1 Sha1) (*git.OdbObject, error) {
    odb, err := g.Odb()
    if err != nil {
        return nil, &OdbNotReady{g, err}
    }
    obj, err := odb.Read(sha1.AsOid())
    if err != nil {
        return nil, err
    }
    return obj, nil
}

func WriteObject(g *git.Repository, content []byte, objtype git.ObjectType) (Sha1, error) {
    odb, err := g.Odb()
    if err != nil {
        return Sha1{}, &OdbNotReady{g, err}
    }
    oid, err := odb.Write(content, objtype)
    if err != nil {
        // err is e.g. "Failed to create temporary file '.../objects/tmp_object_git2_G045iN': Permission denied"
        return Sha1{}, err
    }
    return Sha1FromOid(oid), nil
}

type OdbNotReady struct {
    g   *git.Repository
    err error
}

func (e *OdbNotReady) Error() string {
    return fmt.Sprintf("git(%q): odb not ready: %s", e.g.Path(), e.err)
}

type UnexpectedObjType struct {
    obj      *git.OdbObject
    wantType git.ObjectType
}

func (e *UnexpectedObjType) Error() string {
    return fmt.Sprintf("%s: type is %s  (expected %s)", e.obj.Id(), e.obj.Type(), e.wantType)
}



type Tag struct {
    tagged_type string
    tagged_sha1 Sha1
    // TODO msg
}

// load/parse Tag
//
// Reasons why not use g.LookupTag():
//  - we need to get not only parsed tag object, but also its raw content
//    (libgit2 drops raw data after parsing object)
//  - we need to have tag_parse() -- a way to parse object from a buffer
//    (libgit2 does not provide such functionality at all)
func xload_tag(g *git.Repository, tag_sha1 Sha1) (tag *Tag, tag_obj *git.OdbObject) {
    tag_obj, err := ReadObject(g, tag_sha1, git.ObjectTag)
    raiseif(err)

    tag, err = tag_parse(String(tag_obj.Data()))
    if err != nil {
        raise(&TagLoadError{tag_sha1, err})
    }
    return tag, tag_obj
}

type TagLoadError struct {
    tag_sha1 Sha1
    err      error
}

func (e *TagLoadError) Error() string {
    return fmt.Sprintf("tag %s: %s", e.tag_sha1, e.err)
}

func tag_parse(tag_raw string) (*Tag, error) {
    t := Tag{}
    _, err := fmt.Sscanf(tag_raw, "object %s\ntype %s\n", &t.tagged_sha1, &t.tagged_type)
    if err != nil {
        return nil, errors.New("invalid header")
    }
    return &t, nil
}

// parse lstree entry
func parse_lstree_entry(lsentry string) (mode uint32, type_ string, sha1 Sha1, filename string, err error) {
    // <mode> SP <type> SP <object> TAB <file>      # NOTE file can contain spaces
    __, filename, err1 := headtail(lsentry, "\t")
    _, err2 := fmt.Sscanf(__, "%o %s %s\n", &mode, &type_, &sha1)

    if err1 != nil || err2 != nil {
        return 0, "", Sha1{}, "", &InvalidLstreeEntry{lsentry}
    }

    // parsed ok
    return
}

type InvalidLstreeEntry struct {
    lsentry string
}

func (e *InvalidLstreeEntry) Error() string {
    return fmt.Sprintf("invalid ls-tree entry %q", e.lsentry)
}

// create empty git tree -> tree sha1
var tree_empty Sha1
func mktree_empty() Sha1 {
    if tree_empty.IsNull() {
        tree_empty = xgitSha1("mktree", RunWith{stdin: ""})
    }
    return tree_empty
}

// commit tree
//
// Reason why not use g.CreateCommit():
//  - we don't want to load tree and parent objects - we only have their sha1

// `git commit-tree` -> commit_sha1,   raise on error
type AuthorInfo struct {
    name  string
    email string
    date  string
}

func xcommit_tree2(tree Sha1, parents []Sha1, msg string, author AuthorInfo, committer AuthorInfo) Sha1 {
    argv := []string{"commit-tree", tree.String()}
    for _, p := range parents {
        argv = append(argv, "-p", p.String())
    }

    // env []string -> {}
    env := map[string]string{}
    for _, e := range os.Environ() {
        i := strings.Index(e, "=")
        if i == -1 {
            panic(fmt.Errorf("E: env variable format invalid: %q", e))
        }
        k, v := e[:i], e[i+1:]
        if _, dup := env[k]; dup {
            panic(fmt.Errorf("E: env has duplicate entry for %q", k))
        }
        env[k] = v
    }

    if author.name  != ""       { env["GIT_AUTHOR_NAME"]     = author.name     }
    if author.email != ""       { env["GIT_AUTHOR_EMAIL"]    = author.email    }
    if author.date  != ""       { env["GIT_AUTHOR_DATE"]     = author.date     }
    if committer.name  != ""    { env["GIT_COMMITTER_NAME"]  = committer.name  }
    if committer.email != ""    { env["GIT_COMMITTER_EMAIL"] = committer.email }
    if committer.date  != ""    { env["GIT_COMMITTER_DATE"]  = committer.date  }

    return xgit2Sha1(argv, RunWith{stdin: msg, env: env})
}

func xcommit_tree(tree Sha1, parents []Sha1, msg string) Sha1 {
    return xcommit_tree2(tree, parents, msg, AuthorInfo{}, AuthorInfo{})
}
