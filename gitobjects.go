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
    "strings"
)

type Commit struct {
    tree    Sha1
    parentv []Sha1
    msg     string
}

type Tag struct {
    tagged_type string
    tagged_sha1 Sha1
    // TODO msg
}

// TODO Tree (if/when needed)
// TODO Blob (if/when needed)

// load/parse Commit

// extract .tree .parent[] and .msg
//
// unfortunately `git show --format=%B` adds newline and optionally wants to
// reencode commit message and otherwise heavily rely on rev-list traversal
// machinery -> so we decode commit by hand in a plumbing way.
func xload_commit(commit_sha1 Sha1) (commit *Commit, commit_raw string) {
    gerr, commit_raw, _ := git("cat-file", "commit", commit_sha1, RunWith{raw: true})
    if gerr != nil {
        raise(&CommitLoadError{commit_sha1, gerr})
    }
    commit, err := commit_parse(commit_raw)
    if err != nil {
        raise(&CommitLoadError{commit_sha1, err})
    }
    return commit, commit_raw
}

type CommitLoadError struct {
    commit_sha1 Sha1
    err         error
}

func (e *CommitLoadError) Error() string {
    return fmt.Sprintf("commit %s: %s", e.commit_sha1, e.err)
}

func commit_parse(commit_raw string) (*Commit, error) {
    c := Commit{}
    head, msg, err := headtail(commit_raw, "\n\n")
    c.msg = msg
    if err != nil {
        return nil, errors.New("cannot split to head & msg")
    }

    headv := strings.Split(head, "\n")
    if len(headv) == 0 {
        return nil, errors.New("empty header")
    }
    _, err = fmt.Sscanf(headv[0], "tree %s\n", &c.tree)
    if err != nil {
        return nil, errors.New("bad tree entry")
    }
    for _, h := range headv[1:] {
        if !strings.HasPrefix(h, "parent ") {
            break
        }
        p := Sha1{}
        _, err = fmt.Sscanf(h, "parent %s\n", &p)
        if err != nil {
            return nil, errors.New("bad parent entry")
        }
        c.parentv = append(c.parentv, p)
    }
    return &c, nil
}

// load/parse Tag
func xload_tag(tag_sha1 Sha1) (tag *Tag, tag_raw string) {
    gerr, tag_raw, _ := git("cat-file", "tag", tag_sha1, RunWith{raw: true})
    if gerr != nil {
        raise(&TagLoadError{tag_sha1, gerr})
    }
    tag, err := tag_parse(tag_raw)
    if err != nil {
        raise(&TagLoadError{tag_sha1, err})
    }
    return tag, tag_raw
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
