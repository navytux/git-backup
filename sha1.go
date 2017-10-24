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

// Git-backup | Sha1 type to work with SHA1 oids
package main

import (
    "bytes"
    "encoding/hex"
    "fmt"

    "lab.nexedi.com/kirr/go123/mem"

    git "github.com/libgit2/git2go"
)

const SHA1_RAWSIZE = 20

// SHA1 value in raw form
// NOTE zero value of Sha1{} is NULL sha1
// NOTE Sha1 size is 20 bytes. On amd64
//      - string size = 16 bytes
//      - slice  size = 24 bytes
//      -> so it is reasonable to pass Sha1 not by reference
type Sha1 struct {
    sha1 [SHA1_RAWSIZE]byte
}

// fmt.Stringer
var _ fmt.Stringer = Sha1{}

func (sha1 Sha1) String() string {
    return hex.EncodeToString(sha1.sha1[:])
}

func Sha1Parse(sha1str string) (Sha1, error) {
    sha1 := Sha1{}
    if hex.DecodedLen(len(sha1str)) != SHA1_RAWSIZE {
        return Sha1{}, fmt.Errorf("sha1parse: %q invalid", sha1str)
    }
    _, err := hex.Decode(sha1.sha1[:], mem.Bytes(sha1str))
    if err != nil {
        return Sha1{}, fmt.Errorf("sha1parse: %q invalid: %s", sha1str, err)
    }

    return sha1, nil
}

// fmt.Scanner
var _ fmt.Scanner = (*Sha1)(nil)

func (sha1 *Sha1) Scan(s fmt.ScanState, ch rune) error {
    switch ch {
    case 's', 'v':
    default:
        return fmt.Errorf("Sha1.Scan: invalid verb %q", ch)
    }

    tok, err := s.Token(true, nil)
    if err != nil {
        return err
    }

    *sha1, err = Sha1Parse(mem.String(tok))
    return err
}

// check whether sha1 is null
func (sha1 *Sha1) IsNull() bool {
    return *sha1 == Sha1{}
}

// for sorting by Sha1
type BySha1 []Sha1

func (p BySha1) Len() int           { return len(p) }
func (p BySha1) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p BySha1) Less(i, j int) bool { return bytes.Compare(p[i].sha1[:], p[j].sha1[:]) < 0 }

// interoperability with git2go
func (sha1 *Sha1) AsOid() *git.Oid {
    return (*git.Oid)(&sha1.sha1)
}

func Sha1FromOid(oid *git.Oid) Sha1 {
    return Sha1{*oid}
}
