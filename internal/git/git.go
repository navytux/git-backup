// Copyright (C) 2025  Nexedi SA and Contributors.
//                     Kirill Smelkov <kirr@nexedi.com>
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

// Package internal/git wraps package git2go with providing unconditional safety.
//
// For example git2go.Object.Data() returns []byte that aliases unsafe memory
// that can go away from under []byte if original Object is garbage collected.
// The following code snippet is thus _not_ correct:
//
//	obj = odb.Read(sha1)
//	data = obj.Data()
//	... use data
//
// because obj can be garbage-collected right after `data = obj.Data()` but
// before `use data` leading to either crashes or memory corruption. A
// runtime.KeepAlive(obj) needs to be added to the end of the snippet - after
// `use data` - to make that code correct.
//
// Given that obj.Data() is not "speaking" by itself as unsafe, and that there
// are many similar methods, it is hard to see which places in the code needs
// special attention.
//
// For this reason git-backup took decision to localize git2go-related code in
// one small place here, and to expose only safe things to outside. That is we
// make data copies when reading object data and similar things to provide
// unconditional safety to the caller via that copy cost.
//
// The copy cost is smaller compared to the cost of either spawning e.g. `git
// cat-file` for every object, or interacting with `git cat-file --batch`
// server spawned once, but still spending context switches on every request
// and still making the copy on socket or pipe transfer. But most of all the
// copy cost is negligible to the cost of catching hard to reproduce crashes or
// data corruptions in the production environment.
package git

import (
	"runtime"

	git2go "github.com/libgit2/git2go/v31"
)

// constants are safe to propagate as is.
const (
	ObjectAny     = git2go.ObjectAny
	ObjectInvalid = git2go.ObjectInvalid
	ObjectCommit  = git2go.ObjectCommit
	ObjectTree    = git2go.ObjectTree
	ObjectBlob    = git2go.ObjectBlob
	ObjectTag     = git2go.ObjectTag
)


// types that are safe to propagate as is.
type (
	ObjectType = git2go.ObjectType // int
	Oid        = git2go.Oid        // [20]byte             ; cloned when retrieved
	Signature  = git2go.Signature  // struct with strings  ; strings are cloned when retrieved
	TreeEntry  = git2go.TreeEntry  // struct with sting, Oid, ...  ; strings and oids are cloned when retrieved
)


// types that we wrap to provide safety.

// Repository provides safe wrapper over git2go.Repository .
type Repository struct {
	repo       *git2go.Repository
	References *ReferenceCollection
}

// ReferenceCollection provides safe wrapper over git2go.ReferenceCollection .
type ReferenceCollection struct {
	r *Repository
}

// Reference provides safe wrapper over git2go.Reference .
type Reference struct {
	ref *git2go.Reference
}

// Commit provides safe wrapper over git2go.Commit .
type Commit struct {
	commit *git2go.Commit
}

// Tree provides safe wrapper over git2go.Tree .
type Tree struct {
	tree *git2go.Tree
}

// Odb provides safe wrapper over git2go.Odb .
type Odb struct {
	odb *git2go.Odb
}

// OdbObject provides safe wrapper over git2go.OdbObject .
type OdbObject struct {
	obj *git2go.OdbObject
}


// function and methods to navigate object hierarchy from Repository to e.g. OdbObject or Commit.

func OpenRepository(path string) (*Repository, error) {
	repo, err := git2go.OpenRepository(path)
	if err != nil {
		return nil, err
	}
	r := &Repository{repo: repo}
	r.References = &ReferenceCollection{r}
	return r, nil
}

func (rdb *ReferenceCollection) Create(name string, id *Oid, force bool, msg string) (*Reference, error) {
	ref, err := rdb.r.repo.References.Create(name, id, force, msg)
	if err != nil {
		return nil, err
	}
	return &Reference{ref}, nil
}

func (r *Repository) LookupCommit(id *Oid) (*Commit, error) {
	commit, err := r.repo.LookupCommit(id)
	if err != nil {
		return nil, err
	}
	return &Commit{commit}, nil
}

func (c *Commit) Tree() (*Tree, error) {
	tree, err := c.commit.Tree()
	if err != nil {
		return nil, err
	}
	return &Tree{tree}, nil
}

func (r *Repository) Odb() (*Odb, error) {
	odb, err := r.repo.Odb()
	if err != nil {
		return nil, err
	}
	return &Odb{odb}, nil
}

func (o *Odb) Read(oid *Oid) (*OdbObject, error) {
	obj, err := o.odb.Read(oid)
	if err != nil {
		return nil, err
	}
	return &OdbObject{obj}, nil
}


// wrappers over safe methods

func (c *Commit) ParentCount() uint	{ return c.commit.ParentCount() }
func (o *OdbObject) Type() ObjectType	{ return o.obj.Type() }


// wrappers over unsafe, or potentially unsafe methods

func (r *Repository) Path() string {
	path := stringsClone( r.repo.Path() )
	runtime.KeepAlive(r)
	return path
}

func (r *Repository) DefaultSignature() (*Signature, error) {
	s, err := r.repo.DefaultSignature()
	if s != nil {
		s = &Signature{
			Name:  stringsClone(s.Name),
			Email: stringsClone(s.Email),
			When:  s.When,
		}
	}
	runtime.KeepAlive(r)
	return s, err
}


func (c *Commit) Message() string {
	msg := stringsClone( c.commit.Message() )
	runtime.KeepAlive(c)
	return msg
}

func (c *Commit) ParentId(n uint) *Oid {
	pid := oidClone( c.commit.ParentId(n) )
	runtime.KeepAlive(c)
	return pid
}

func (t *Tree) EntryByName(filename string) *TreeEntry {
	e := t.tree.EntryByName(filename)
	if e != nil {
		e = &TreeEntry{
			Name:     stringsClone(e.Name),
			Id:       oidClone(e.Id),
			Type:     e.Type,
			Filemode: e.Filemode,
		}
	}
	runtime.KeepAlive(t)
	return e
}


func (o *Odb) Write(data []byte, otype ObjectType) (*Oid, error) {
	oid, err := o.odb.Write(data, otype)
	oid = oidClone(oid)
	runtime.KeepAlive(o)
	return oid, err
}


func (o *OdbObject) Id() *Oid {
	id := oidClone( o.obj.Id() )
	runtime.KeepAlive(o)
	return id
}

func (o *OdbObject) Data() []byte {
	data := bytesClone( o.obj.Data() )
	runtime.KeepAlive(o)
	return data
}


// misc

func oidClone(oid *Oid) *Oid {
	var oid2 Oid
	if oid == nil {
		return nil
	}
	copy(oid2[:], oid[:])
	return &oid2
}
