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

import (
	"strings"
	"testing"
)

func TestPathEscapeUnescape(t *testing.T) {
	type TestEntry struct { path string; escapedv []string }
	te := func(path string, escaped ...string) TestEntry {
		return TestEntry{path, escaped}
	}
	var tests = []TestEntry{
		//  path           escaped        non-canonical escapes
		te("hello/world", "hello/world", "%68%65%6c%6c%6f%2f%77%6f%72%6c%64"),
		te("hello/мир", "hello/мир"),
		te("hello/ мир", "hello/%20мир"),
		te("hel%lo/мир", "hel%25lo/мир"),
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
		te("/hello/./world", "/hello/%2E/world"),
		te("/hello/.a/world", "/hello/%2Ea/world"),
		te("/hello/a./world", "/hello/a./world"),
		te("/hello/../world", "/hello/%2E./world"),
		te("/hello/a..b/world", "/hello/a%2E.b/world"),
		te("/hello/a.c.b/world", "/hello/a.c.b/world"),
		te("/hello/a.c..b/world", "/hello/a.c%2E.b/world"),

		// special & control characters
		te("/hel lo/wor\tld/a:?[\\^~*@%b/\001\004\n\xc2\xa0", "/hel%20lo/wor%09ld/a%3A%3F%5B%5C%5E%7E%2A%40%25b/%01%04%0A%C2%A0"),

		// utf8 error
		te("a\xc5z", "a%C5z"),
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
