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
    "reflect"
    "strings"
    "testing"
)

// check that String() and Bytes() create correct objects which alias original object memory
func TestStringBytes(t *testing.T) {
    s := "Hello"
    b := []byte(s)

    s1 := String(b)
    b1 := Bytes(s1)
    if s1 != s                      { t.Error("string -> []byte -> String != Identity") }
    if !reflect.DeepEqual(b1, b)    { t.Error("[]byte -> String -> Bytes != Identity") }
    b[0] = 'I'
    if s  != "Hello"                { t.Error("string -> []byte not copied") }
    if s1 != "Iello"                { t.Error("[]byte -> String not aliased") }
    if !reflect.DeepEqual(b1, b)    { t.Error("string -> Bytes  not aliased") }
}

func TestSplit2(t *testing.T) {
    var tests = []struct { input, s1, s2 string; ok bool } {
        {"", "", "", false},
        {" ", "", "", true},
        {"hello", "", "", false},
        {"hello world", "hello", "world", true},
        {"hello world 1", "", "", false},
    }

    for _, tt := range tests {
        s1, s2, err := split2(tt.input, " ")
        ok := err == nil
        if s1 != tt.s1 || s2 != tt.s2 || ok != tt.ok {
            t.Errorf("split2(%q) -> %q %q %v  ; want %q %q %v", tt.input, s1, s2, ok, tt.s1, tt.s2, tt.ok)
        }
    }
}

func TestHeadtail(t *testing.T) {
    var tests = []struct { input, head, tail string; ok bool } {
        {"",                "", "", false},
        {" ",               "", "", true},
        {"  ",              "", " ", true},
        {"hello world",     "hello", "world", true},
        {"hello world 1",   "hello", "world 1", true},
        {"hello  world 2",  "hello", " world 2", true},
    }

    for _, tt := range tests {
        head, tail, err := headtail(tt.input, " ")
        ok := err == nil
        if head != tt.head || tail != tt.tail || ok != tt.ok {
            t.Errorf("headtail(%q) -> %q %q %v  ; want %q %q %v", tt.input, head, tail, ok, tt.head, tt.tail, tt.ok)
        }
    }
}

func TestPathEscapeUnescape(t *testing.T) {
    type TestEntry struct { path string; escapedv []string }
    te := func(path string, escaped ...string) TestEntry {
        return TestEntry{path, escaped}
    }
    var tests = []TestEntry{
        //  path           escaped        non-canonical escapes
        te("hello/world", "hello/world", "%68%65%6c%6c%6f%2f%77%6f%72%6c%64"),
        te("hello/мир",   "hello/мир"),
        te("hello/ мир",  "hello/%20мир"),
        te("hel%lo/мир",  "hel%25lo/мир"),
        te(".hello/.world", "%2Ehello/%2Eworld"),
        te("..hello/world.loc", "%2E.hello/world.loc"),
        te("..hello/world.lock", "%2E.hello/world%2Elock"),
        // leading /
        te("/hello/world", "/hello/world"),
        te("//hello///world", "//hello///world"),
        // trailing /
        te("/hello/world/", "/hello/world"),
        te("/hello/world//", "/hello/world"),

        // trailing ...
        te("/hello/world.", "/hello/world."),
        te("/hello/world..", "/hello/world%2E."),
        te("/hello/world...", "/hello/world%2E%2E."),
        te("/hello/world...git", "/hello/world%2E%2E.git"),

        // .. anywhere
        te("/hello/./world",    "/hello/%2E/world"),
        te("/hello/.a/world",   "/hello/%2Ea/world"),
        te("/hello/a./world",   "/hello/a./world"),
        te("/hello/../world",   "/hello/%2E./world"),
        te("/hello/a..b/world", "/hello/a%2E.b/world"),
        te("/hello/a.c.b/world", "/hello/a.c.b/world"),
        te("/hello/a.c..b/world", "/hello/a.c%2E.b/world"),

        // special & control characters
        te("/hel lo/wor\tld/a:?[\\^~*@%b/\001\004\n\xc2\xa0", "/hel%20lo/wor%09ld/a%3A%3F%5B%5C%5E%7E%2A%40%25b/%01%04%0A%C2%A0"),

        // utf8 error
        te("a\xc5z",    "a%C5z"),
    }

    for _, tt := range tests {
        escaped := path_refescape(tt.path)
        if escaped != tt.escapedv[0] {
            t.Errorf("path_refescape(%q) -> %q  ; want %q", tt.path, escaped, tt.escapedv[0])
        }
        // also check the decoding
        pathok := strings.TrimRight(tt.path, "/")
        for _, escaped := range tt.escapedv {
            unescaped, err := path_refunescape(escaped)
            if unescaped != pathok || err != nil {
                t.Errorf("path_refunescape(%q) -> %q %v  ; want %q nil", escaped, unescaped, err, tt.path)
            }
        }
    }
}

func TestPathUnescapeErr(t *testing.T) {
    var tests = []struct{ escaped string }{
        {"%"},
        {"%2"},
        {"%2q"},
        {"hell%2q/world"},
    }

    for _, tt := range tests {
        unescaped, err := path_refunescape(tt.escaped)
        if err == nil || unescaped != "" {
            t.Errorf("path_refunescape(%q) -> %q %v  ; want \"\" err", tt.escaped, unescaped, err)
        }
    }
}
