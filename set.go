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

// Git-backup | Set "template" type
// TODO -> go:generate + template
package main

// Set<Sha1>
type Sha1Set map[Sha1]struct{}

func (s Sha1Set) Add(v Sha1) {
    s[v] = struct{}{}
}

func (s Sha1Set) Contains(v Sha1) bool {
    _, ok := s[v]
    return ok
}

// all elements of set as slice
func (s Sha1Set) Elements() []Sha1 {
    ev := make([]Sha1, len(s))
    i := 0
    for e := range s {
        ev[i] = e
        i++
    }
    return ev
}

// Set<string>
type StrSet map[string]struct{}

func (s StrSet) Add(v string) {
    s[v] = struct{}{}
}

func (s StrSet) Contains(v string) bool {
    _, ok := s[v]
    return ok
}

// all elements of set as slice
func (s StrSet) Elements() []string {
    ev := make([]string, len(s))
    i := 0
    for e := range s {
        ev[i] = e
        i++
    }
    return ev
}
