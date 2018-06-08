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

package main
// Git-backup | Git object: Blob Tree Commit Tag

import (
    "errors"
    "fmt"
    "os"
    "os/user"
    "sync"
    "time"

    "lab.nexedi.com/kirr/go123/exc"
    "lab.nexedi.com/kirr/go123/mem"
    "lab.nexedi.com/kirr/go123/xstrings"

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
    tagged_type git.ObjectType
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
    exc.Raiseif(err)

    tag, err = tag_parse(mem.String(tag_obj.Data()))
    if err != nil {
        exc.Raise(&TagLoadError{tag_sha1, err})
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
    tagged_type := ""
    _, err := fmt.Sscanf(tag_raw, "object %s\ntype %s\n", &t.tagged_sha1, &tagged_type)
    if err != nil {
        return nil, errors.New("invalid header")
    }
    var ok bool
    t.tagged_type, ok = gittype(tagged_type)
    if !ok {
        return nil, fmt.Errorf("invalid tagged type %q", tagged_type)
    }
    return &t, nil
}

// parse lstree entry
func parse_lstree_entry(lsentry string) (mode uint32, type_ string, sha1 Sha1, filename string, err error) {
    // <mode> SP <type> SP <object> TAB <file>      # NOTE file can contain spaces
    __, filename, err1 := xstrings.HeadTail(lsentry, "\t")
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

type AuthorInfo git.Signature

func (ai *AuthorInfo) String() string {
    _, toffset := ai.When.Zone()
    // offset: Git wants in minutes, .Zone() gives in seconds
    return fmt.Sprintf("%s <%s> %d %+05d", ai.Name, ai.Email, ai.When.Unix(), toffset / 60)
}

var (
    defaultIdent     AuthorInfo // default ident without date
    defaultIdentOnce sync.Once
)

func getDefaultIdent(g *git.Repository) AuthorInfo {
    sig, err := g.DefaultSignature()
    if err == nil {
        return AuthorInfo(*sig)
    }

    // libgit2 failed for some reason (i.e. user.name config not set). Let's cook ident ourselves
    defaultIdentOnce.Do(func() {
        var username, name string
        u, _ := user.Current()
        if u != nil {
            username = u.Username
            name = u.Name
        } else {
            username = "?"
            name = "?"
        }

        // XXX it is better to get hostname as fqdn
        hostname, _ := os.Hostname()
        if hostname == "" {
            hostname = "?"
        }

        defaultIdent.Name = name
        defaultIdent.Email = fmt.Sprintf("%s@%s", username, hostname)
    })

    ident := defaultIdent
    ident.When = time.Now()
    return ident
}

// `git commit-tree` -> commit_sha1,   raise on error
func xcommit_tree2(g *git.Repository, tree Sha1, parents []Sha1, msg string, author AuthorInfo, committer AuthorInfo) Sha1 {
    ident := getDefaultIdent(g)

    if author.Name  == ""       { author.Name       = ident.Name  }
    if author.Email == ""       { author.Email      = ident.Email }
    if author.When.IsZero()     { author.When       = ident.When  }
    if committer.Name  == ""    { committer.Name    = ident.Name  }
    if committer.Email == ""    { committer.Email   = ident.Email }
    if committer.When.IsZero()  { committer.When    = ident.When  }

    commit := fmt.Sprintf("tree %s\n", tree)
    for _, p := range parents {
        commit += fmt.Sprintf("parent %s\n", p)
    }
    commit += fmt.Sprintf("author %s\n", &author)
    commit += fmt.Sprintf("committer %s\n", &committer)
    commit += fmt.Sprintf("\n%s", msg)

    sha1, err := WriteObject(g, mem.Bytes(commit), git.ObjectCommit)
    exc.Raiseif(err)

    return sha1
}

func xcommit_tree(g *git.Repository, tree Sha1, parents []Sha1, msg string) Sha1 {
    return xcommit_tree2(g, tree, parents, msg, AuthorInfo{}, AuthorInfo{})
}


// gittype converts string to git.ObjectType.
//
// Only valid concrete git types are converted successfully.
func gittype(typ string) (git.ObjectType, bool) {
    switch typ {
    case "commit": return git.ObjectCommit, true
    case "tree":   return git.ObjectTree, true
    case "blob":   return git.ObjectBlob, true
    case "tag":    return git.ObjectTag, true
    }

    return git.ObjectBad, false

}

// gittypestr converts git.ObjectType to string.
//
// We depend on this conversion being exact and matching how Git encodes it in
// objects. git.ObjectType.String() is different (e.g. "Blob" instead of
// "blob"), and can potentially change over time.
//
// gittypestr expects the type to be valid and concrete - else it panics.
func gittypestr(typ git.ObjectType) string {
    switch typ {
    case git.ObjectCommit:  return "commit"
    case git.ObjectTree:    return "tree"
    case git.ObjectBlob:    return "blob"
    case git.ObjectTag:     return "tag"
    }

    panic(fmt.Sprintf("git type %#v invalid", typ))
}
