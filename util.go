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

// Git-backup | Miscellaneous utilities
package main

import (
    "encoding/hex"
    "fmt"
    "os"
    "strings"
    "syscall"
    "unicode"
    "unicode/utf8"

    "lab.nexedi.com/kirr/go123/mem"
)

// split string into lines. The last line, if it is empty, is omitted from the result
// (rationale is: string.Split("hello\nworld\n", "\n") -> ["hello", "world", ""])
func splitlines(s, sep string) []string {
    sv := strings.Split(s, sep)
    l := len(sv)
    if l > 0 && sv[l-1] == "" {
        sv = sv[:l-1]
    }
    return sv
}

// split string by sep and expect exactly 2 parts
func split2(s, sep string) (s1, s2 string, err error) {
    parts := strings.Split(s, sep)
    if len(parts) != 2 {
        return "", "", fmt.Errorf("split2: %q has %v parts (expected 2, sep: %q)", s, len(parts), sep)
    }
    return parts[0], parts[1], nil
}

// (head+sep+tail) -> head, tail
func headtail(s, sep string) (head, tail string, err error) {
    parts := strings.SplitN(s, sep, 2)
    if len(parts) != 2 {
        return "", "", fmt.Errorf("headtail: %q has no %q", s, sep)
    }
    return parts[0], parts[1], nil
}

// strip_prefix("/a/b", "/a/b/c/d/e") -> "c/d/e" (without leading /)
// path must start with prefix
func strip_prefix(prefix, path string) string {
    if !strings.HasPrefix(path, prefix) {
        panic(fmt.Errorf("strip_prefix: %q has no prefix %q", path, prefix))
    }
    path = path[len(prefix):]
    for strings.HasPrefix(path, "/") {
        path = path[1:] // strip leading /
    }
    return path
}

// reprefix("/a", "/b", "/a/str") -> "/b/str"
// path must start with prefix_from
func reprefix(prefix_from, prefix_to, path string) string {
    path = strip_prefix(prefix_from, path)
    return fmt.Sprintf("%s/%s", prefix_to, path)
}

// like ioutil.WriteFile() but takes native mode/perm
func writefile(path string, data []byte, perm uint32) error {
    fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, perm)
    if err != nil {
        return &os.PathError{"open", path, err}
    }
    f := os.NewFile(uintptr(fd), path)
    _, err = f.Write(data)
    err2 := f.Close()
    if err == nil {
        err = err2
    }
    return err
}

// escape path so that git is happy to use it as ref
// https://git.kernel.org/cgit/git/git.git/tree/refs.c?h=v2.9.0-37-g6d523a3#n34
// XXX very suboptimal
func path_refescape(path string) string {
    outv := []string{}
    for _, component := range strings.Split(path, "/") {
        out := ""
        dots := 0 // number of seen consecutive dots
        for len(component) > 0 {
            r, size := utf8.DecodeRuneInString(component)

            // no ".." anywhere - we replace dots run to %46%46... with trailing "."
            // this way for single "." case we'll have it intact and avoid .. anywhere
            // also this way: trailing .git is always encoded as ".git"
            if r == '.' {
                dots += 1
                component = component[size:]
                continue
            }
            if dots != 0 {
                out += strings.Repeat(escape("."), dots-1)
                out += "."
                dots = 0
            }

            rbytes := component[:size]
            if shouldEscape(r) {
                rbytes = escape(rbytes)
            }
            out += rbytes
            component = component[size:]
        }

        // handle trailing dots
        if dots != 0 {
            out += strings.Repeat(escape("."), dots-1)
            out += "."
        }

        if len(out) > 0 {
            // ^. not allowed
            if out[0] == '.' {
                out = escape(".") + out[1:]
            }
            // .lock$ not allowed
            if strings.HasSuffix(out, ".lock") {
                out = out[:len(out)-5] + escape(".") + "lock"
            }
        }
        outv = append(outv, out)
    }

    // strip trailing /
    for len(outv) > 0 {
        if len(outv[len(outv)-1]) != 0 {
            break
        }
        outv = outv[:len(outv)-1]
    }
    return strings.Join(outv, "/")
}

func shouldEscape(r rune) bool {
    if unicode.IsSpace(r) || unicode.IsControl(r) {
        return true
    }
    switch r {
    // NOTE RuneError is for always escaping non-valid UTF-8
    case ':', '?', '[', '\\', '^', '~', '*', '@', '%', utf8.RuneError:
        return true
    }
    return false
}

func escape(s string) string {
    out := ""
    for i := 0; i < len(s); i++ {
        out += fmt.Sprintf("%%%02X", s[i])
    }
    return out
}

// unescape path encoded by path_refescape()
// decoding is permissive - any byte can be %-encoded, not only special cases
// XXX very suboptimal
func path_refunescape(s string) (string, error) {
    l := len(s)
    out := make([]byte, 0, len(s))
    for i := 0; i < l; i++ {
        c := s[i]
        if c == '%' {
            if i+2 >= l {
                return "", EscapeError(s)
            }
            b, err := hex.DecodeString(s[i+1:i+3])
            if err != nil {
                return "", EscapeError(s)
            }

            c = b[0]
            i += 2
        }
        out = append(out, c)
    }
    return mem.String(out), nil
}

type EscapeError string

func (e EscapeError) Error() string {
    return fmt.Sprintf("%q: invalid escape format", string(e))
}
